package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lanshare/internal/auth"
	"lanshare/internal/files"
	"lanshare/internal/logger"
	"lanshare/internal/session"
	"lanshare/internal/storage"
)

func newQuotaTestEnv(t *testing.T, maxStorage int64) *testEnv {
	t.Helper()

	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fileSvc, err := files.New(filepath.Join(root, "files"))
	if err != nil {
		t.Fatalf("创建文件服务失败: %v", err)
	}

	log, err := logger.New(filepath.Join(root, "test.log"))
	if err != nil {
		t.Fatalf("创建日志器失败: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	srv := NewServer(Options{
		Store:             store,
		Auth:              auth.New(store),
		Files:             fileSvc,
		Sessions:          session.NewManager(),
		Logger:            log,
		WebFS:             http.NotFoundHandler(),
		SessionTTL:        3600,
		MaxStorage:        maxStorage,
		UploadConcurrency: 0,
	})
	return &testEnv{srv: srv, store: store, files: fileSvc, root: root}
}

func (e *testEnv) uploadIntoFolder(t *testing.T, name string, data []byte, folderID int64, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("kind", "permanent")
	if folderID > 0 {
		_ = mw.WriteField("folderId", itoa(folderID))
	}
	_ = mw.WriteField("name", name)
	pw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("构造表单失败: %v", err)
	}
	if _, err := pw.Write(data); err != nil {
		t.Fatalf("写入文件内容失败: %v", err)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	e.srv.Routes().ServeHTTP(w, req)
	return w
}

// TestStorageQuotaEnforcement 验证文件仓库总容量上限（含 /api/status 回传、预检与落盘后校验）。
func TestStorageQuotaEnforcement(t *testing.T) {
	env := newQuotaTestEnv(t, 100)
	admin := env.mkAdmin(t, "root")

	// /api/status 返回 maxStorage。
	sw := env.do(t, http.MethodGet, "/api/status", nil, nil)
	if sw.Code != http.StatusOK {
		t.Fatalf("GET /api/status 状态码 = %d", sw.Code)
	}
	var st struct {
		MaxStorage int64 `json:"maxStorage"`
	}
	_ = json.Unmarshal(sw.Body.Bytes(), &st)
	if st.MaxStorage != 100 {
		t.Fatalf("maxStorage = %d, 期望 100", st.MaxStorage)
	}

	// 上传 60 字节应成功。
	if w := env.uploadIntoFolder(t, "a.bin", bytes.Repeat([]byte("a"), 60), 0, admin); w.Code != http.StatusOK {
		t.Fatalf("首次上传 60B 应当成功，得到 %d: %s", w.Code, w.Body.String())
	}

	// 再传 50 字节超出 100 字节配额，应返回 507 且不入库。
	if w := env.uploadIntoFolder(t, "b.bin", bytes.Repeat([]byte("b"), 50), 0, admin); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("超限上传 50B 应当返回 507，得到 %d: %s", w.Code, w.Body.String())
	}
	if used, _ := env.store.TotalUsage(storage.KindPermanent); used != 60 {
		t.Fatalf("超限后占用应当保持 60，实际 %d", used)
	}

	// 恰好 40 字节总计 100 字节应允许。
	if w := env.uploadIntoFolder(t, "c.bin", bytes.Repeat([]byte("c"), 40), 0, admin); w.Code != http.StatusOK {
		t.Fatalf("刚好达配额上限 40B 应当成功，得到 %d: %s", w.Code, w.Body.String())
	}

	// 已满后再传 1 字节直接被拒。
	if w := env.uploadIntoFolder(t, "d.bin", []byte("x"), 0, admin); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("满额后上传 1B 应当返回 507，得到 %d: %s", w.Code, w.Body.String())
	}
}

// TestFolderCRUDCycleAndUnwrap 验证文件夹创建、重名冲突、防成环移动、文件移入移出以及删除上移。
func TestFolderCRUDCycleAndUnwrap(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	// 未登录不能建文件夹。
	if w := env.do(t, http.MethodPost, "/api/folders", []byte(`{"name":"docs"}`), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录创建文件夹应当 401，得到 %d", w.Code)
	}

	// 建根目录 docs。
	w := env.do(t, http.MethodPost, "/api/folders", []byte(`{"name":"docs"}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("创建 docs 应当 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var docs folderResp
	_ = json.Unmarshal(w.Body.Bytes(), &docs)

	// 同级忽略大小写重名返回 409。
	if w := env.do(t, http.MethodPost, "/api/folders", []byte(`{"name":"DOCS"}`), admin); w.Code != http.StatusConflict {
		t.Fatalf("同级重名应当 409，得到 %d", w.Code)
	}

	// 在 docs 下建子文件夹 sub。
	w = env.do(t, http.MethodPost, "/api/folders", []byte(`{"name":"sub","parentId":`+itoa(docs.ID)+`}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("创建 sub 应当 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var sub folderResp
	_ = json.Unmarshal(w.Body.Bytes(), &sub)

	// 尝试把 docs 移入自己的子目录 sub 应当 400。
	if w := env.do(t, http.MethodPatch, "/api/folders/"+itoa(docs.ID), []byte(`{"parentId":`+itoa(sub.ID)+`}`), admin); w.Code != http.StatusBadRequest {
		t.Fatalf("父目录移入子目录应当 400，得到 %d: %s", w.Code, w.Body.String())
	}

	// 重命名 sub -> notes。
	w = env.do(t, http.MethodPatch, "/api/folders/"+itoa(sub.ID), []byte(`{"name":"notes"}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("重命名文件夹应当 200，得到 %d: %s", w.Code, w.Body.String())
	}

	// 上传文件直接归入 notes。
	uw := env.uploadIntoFolder(t, "readme.txt", []byte("hello"), sub.ID, admin)
	if uw.Code != http.StatusOK {
		t.Fatalf("上传到文件夹应当 200，得到 %d: %s", uw.Code, uw.Body.String())
	}
	var uf fileResp
	_ = json.Unmarshal(uw.Body.Bytes(), &uf)
	if uf.FolderID == nil || *uf.FolderID != sub.ID {
		t.Fatalf("上传返回的 folderId = %v, 期望 %d", uf.FolderID, sub.ID)
	}

	// 移动文件到根目录，再移回 notes。
	w = env.do(t, http.MethodPatch, "/api/files/"+itoa(uf.ID), []byte(`{"folderId":0}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("移到根目录应当 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var moved fileResp
	_ = json.Unmarshal(w.Body.Bytes(), &moved)
	if moved.FolderID != nil {
		t.Fatalf("移回根目录后 folderId 应为 nil，得到 %v", moved.FolderID)
	}

	w = env.do(t, http.MethodPatch, "/api/files/"+itoa(uf.ID), []byte(`{"folderId":`+itoa(sub.ID)+`}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("移回 notes 应当 200，得到 %d: %s", w.Code, w.Body.String())
	}

	// 删除外层 docs：子目录 notes 应上移到根目录，且 readme.txt 仍留在 notes 内。
	if w := env.do(t, http.MethodDelete, "/api/folders/"+itoa(docs.ID), nil, admin); w.Code != http.StatusOK {
		t.Fatalf("删除 docs 应当 200，得到 %d: %s", w.Code, w.Body.String())
	}
	notesAfter, err := env.store.FolderByID(sub.ID)
	if err != nil {
		t.Fatalf("notes 不应被连带删除: %v", err)
	}
	if notesAfter.ParentID != nil {
		t.Fatalf("docs 删除后 notes.ParentID 应上移为 nil，实际 %v", notesAfter.ParentID)
	}

	// 再删除 notes：readme.txt 应上移到根目录（不能连带删文件）。
	if w := env.do(t, http.MethodDelete, "/api/folders/"+itoa(sub.ID), nil, admin); w.Code != http.StatusOK {
		t.Fatalf("删除 notes 应当 200，得到 %d: %s", w.Code, w.Body.String())
	}
	fAfter, err := env.store.FileByID(uf.ID)
	if err != nil {
		t.Fatalf("文件夹删除后文件不应被删除: %v", err)
	}
	if fAfter.FolderID != nil {
		t.Fatalf("notes 删除后文件应上移至根目录，实际 folderId=%v", fAfter.FolderID)
	}
}

// TestFolderPermissions 验证普通用户不能修改/删除他人文件夹，管理员放行。
func TestFolderPermissions(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	alice := env.mkUserCookie(t, "alice", storage.RoleUser)
	bob := env.mkUserCookie(t, "bob", storage.RoleUser)

	w := env.do(t, http.MethodPost, "/api/folders", []byte(`{"name":"alice-dir"}`), alice)
	if w.Code != http.StatusOK {
		t.Fatalf("alice 建文件夹应当 200，得到 %d", w.Code)
	}
	var dir folderResp
	_ = json.Unmarshal(w.Body.Bytes(), &dir)

	if w := env.do(t, http.MethodPatch, "/api/folders/"+itoa(dir.ID), []byte(`{"name":"hacked"}`), bob); w.Code != http.StatusForbidden {
		t.Errorf("bob 重命名 alice 文件夹应当 403，得到 %d", w.Code)
	}
	if w := env.do(t, http.MethodDelete, "/api/folders/"+itoa(dir.ID), nil, bob); w.Code != http.StatusForbidden {
		t.Errorf("bob 删除 alice 文件夹应当 403，得到 %d", w.Code)
	}
	if w := env.do(t, http.MethodPatch, "/api/folders/"+itoa(dir.ID), []byte(`{"name":"admin-renamed"}`), admin); w.Code != http.StatusOK {
		t.Errorf("管理员重命名应当 200，得到 %d", w.Code)
	}
}

// TestRepoInlinePreviewHeaders 验证文件仓库 ?inline=1 仅对白名单位图与 .txt 返回内联响应，且列表带上 isImage/isText。
func TestRepoInlinePreviewHeaders(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	pngID := env.uploadOne(t, "photo.png", []byte("fake-png"), admin)
	txtID := env.uploadOne(t, "notes.txt", []byte("hello world"), admin)
	svgID := env.uploadOne(t, "icon.svg", []byte("<svg><script>alert(1)</script></svg>"), admin)
	htmlID := env.uploadOne(t, "page.html", []byte("<script>alert(1)</script>"), admin)

	cases := []struct {
		id         int64
		query      string
		wantType   string
		wantDisp   string
	}{
		{pngID, "?inline=1", "image/png", "inline"},
		{txtID, "?inline=1", "text/plain; charset=utf-8", "inline"},
		{svgID, "?inline=1", "application/octet-stream", "attachment"},
		{htmlID, "?inline=1", "application/octet-stream", "attachment"},
		{pngID, "", "application/octet-stream", "attachment"},
		{txtID, "", "application/octet-stream", "attachment"},
	}

	for _, tc := range cases {
		w := env.do(t, http.MethodGet, "/api/files/"+itoa(tc.id)+tc.query, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/files/%d%s 状态码 = %d", tc.id, tc.query, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != tc.wantType {
			t.Errorf("id=%d%s Content-Type = %q, 期望 %q", tc.id, tc.query, got, tc.wantType)
		}
		if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, tc.wantDisp) {
			t.Errorf("id=%d%s Content-Disposition = %q, 期望以 %q 开头", tc.id, tc.query, got, tc.wantDisp)
		}
	}

	// 验证列表包含 isImage / isText 与 folders 字段。
	lw := env.do(t, http.MethodGet, "/api/files?kind=permanent", nil, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("GET /api/files 状态码 = %d", lw.Code)
	}
	var listResp struct {
		Files   []fileResp   `json:"files"`
		Folders []folderResp `json:"folders"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("解析列表响应失败: %v", err)
	}
	if listResp.Folders == nil {
		t.Error("folders 字段不应为 nil")
	}
	flags := map[int64]fileResp{}
	for _, f := range listResp.Files {
		flags[f.ID] = f
	}
	if !flags[pngID].IsImage || flags[pngID].IsText {
		t.Errorf("png flags 错误: %+v", flags[pngID])
	}
	if !flags[txtID].IsText || flags[txtID].IsImage {
		t.Errorf("txt flags 错误: %+v", flags[txtID])
	}
	if flags[svgID].IsImage || flags[svgID].IsText {
		t.Errorf("svg 不应被标为可内联预览: %+v", flags[svgID])
	}
}
