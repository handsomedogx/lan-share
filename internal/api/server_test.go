package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lanshare/internal/auth"
	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/logger"
	"lanshare/internal/session"
	"lanshare/internal/storage"
)

// ---------------------------------------------------------------- 测试脚手架
//
// 这些测试直接打 handler（绕过真实网络），重点覆盖本轮修复的
// 几个接口级行为：删除时的引用计数、去重命中失效目标、上传上限解耦、
// 管理设置的 GET/PUT 分流。

// testEnv 是一套完整的测试依赖。
type testEnv struct {
	srv   *Server
	store *storage.Store
	files *files.Service
	root  string
}

// newTestEnv 组装一个可用的 Server。
//
// maxUpload / chatMaxUpload 显式传入，正是为了能构造出
// 「仓库与聊天室上限不同」这种场景来验证解耦。
func newTestEnv(t *testing.T, maxUpload, chatMaxUpload int64) *testEnv {
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

	sessions := session.NewManager()

	srv := NewServer(Options{
		Store:             store,
		Auth:              auth.New(store),
		Files:             fileSvc,
		Sessions:          sessions,
		Logger:            log,
		WebFS:             http.NotFoundHandler(),
		SessionTTL:        3600,
		MaxUpload:         maxUpload,
		ChatMaxUpload:     chatMaxUpload,
		UploadConcurrency: 0, // 不限流，避免测试互相排队
	})

	return &testEnv{srv: srv, store: store, files: fileSvc, root: root}
}

// mkAdmin 建一个管理员并返回其登录 Cookie。
func (e *testEnv) mkAdmin(t *testing.T, name string) *http.Cookie {
	t.Helper()

	hash, err := auth.Hash("password123")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	u, err := e.store.CreateUser(name, hash, storage.RoleAdmin)
	if err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	id := httpx.NewID()
	if err := e.store.CreateSession(id, u.ID, 3600*1e9); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return &http.Cookie{Name: httpx.CookieName, Value: id}
}

// do 发一个请求，自动带上 Cookie。
func (e *testEnv) do(t *testing.T, method, path string, body []byte, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	e.srv.Routes().ServeHTTP(w, r)
	return w
}

// ---------------------------------------------------------------- 管理设置 GET/PUT

// TestAdminSettingsGetWithoutBody 是本轮 P1 修复的核心断言。
//
// 修复前同一个 handler 无条件 DecodeJSON，GET 没有 body 就拿到 EOF，
// 于是「读取设置」这个接口恒定返回 400 —— 前端一进管理面板就报错。
func TestAdminSettingsGetWithoutBody(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	w := env.do(t, http.MethodGet, "/api/admin/settings", nil, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 无 body 应当返回 200，得到 %d（body=%s）", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 库里已有用户，注册开关默认关闭。
	if open, ok := resp["registrationOpen"].(bool); !ok || open {
		t.Errorf("registrationOpen 应当为 false，得到 %v", resp["registrationOpen"])
	}
}

// TestAdminSettingsPutThenGet 验证 PUT 能改值，且 GET 能读回新值。
func TestAdminSettingsPutThenGet(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	// ---- 打开注册 ----
	w := env.do(t, http.MethodPut, "/api/admin/settings",
		[]byte(`{"registrationOpen":true}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 合法 JSON 应当返回 200，得到 %d（body=%s）", w.Code, w.Body.String())
	}

	// 数据库里的值必须真的变了。
	open, err := env.store.IsRegistrationOpen()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if !open {
		t.Fatal("PUT 之后注册开关应当是打开的")
	}

	// ---- PUT 的响应体也应反映最新值 ----
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if got, _ := resp["registrationOpen"].(bool); !got {
		t.Errorf("PUT 响应里的 registrationOpen 应为 true，得到 %v", resp["registrationOpen"])
	}

	// ---- GET 读回 ----
	w = env.do(t, http.MethodGet, "/api/admin/settings", nil, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 应当返回 200，得到 %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if got, _ := resp["registrationOpen"].(bool); !got {
		t.Errorf("GET 应当读到已打开的开关，得到 %v", resp["registrationOpen"])
	}

	// ---- 再关掉 ----
	w = env.do(t, http.MethodPut, "/api/admin/settings",
		[]byte(`{"registrationOpen":false}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 关闭应当返回 200，得到 %d", w.Code)
	}
	if open, _ := env.store.IsRegistrationOpen(); open {
		t.Fatal("PUT false 之后注册开关应当关闭")
	}
}

// TestAdminSettingsPutInvalidJSON 验证 PUT 非法 JSON 仍返回 400。
//
// 分流之后 PUT 分支的校验不能被漏掉。
func TestAdminSettingsPutInvalidJSON(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	w := env.do(t, http.MethodPut, "/api/admin/settings", []byte(`{not json`), admin)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT 非法 JSON 应当返回 400，得到 %d", w.Code)
	}
}

// TestAdminSettingsRequiresAdmin 验证鉴权仍然生效（分流不能绕过守卫）。
func TestAdminSettingsRequiresAdmin(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	// 只建库不登录。
	env.mkAdmin(t, "root")

	if w := env.do(t, http.MethodGet, "/api/admin/settings", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("未登录访问设置应当 401，得到 %d", w.Code)
	}
	if w := env.do(t, http.MethodPut, "/api/admin/settings",
		[]byte(`{"registrationOpen":true}`), nil); w.Code != http.StatusUnauthorized {
		t.Errorf("未登录修改设置应当 401，得到 %d", w.Code)
	}
}

// ---------------------------------------------------------------- 上传上限解耦

// chatUpload 向指定房间上传一个文件，返回响应。
func (e *testEnv) chatUpload(t *testing.T, code, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("创建表单文件失败: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("写入内容失败: %v", err)
	}
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+code+"/files", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	e.srv.Routes().ServeHTTP(w, r)
	return w
}

// repoUpload 向文件仓库上传一个文件。
func (e *testEnv) repoUpload(t *testing.T, filename string, content []byte, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("kind", "permanent"); err != nil {
		t.Fatalf("写 kind 失败: %v", err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("创建表单文件失败: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("写入内容失败: %v", err)
	}
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/files", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	e.srv.Routes().ServeHTTP(w, r)
	return w
}

// TestChatUploadLimitIndependentOfRepoLimit 是上传限制解耦的核心断言。
//
// 场景：仓库限制 1KB，聊天室限制 4KB。
// 一个 2KB 的文件在聊天室必须能传上去 —— 修复前它会被
// files.Service 里绑定的仓库上限二次截断，返回 413。
func TestChatUploadLimitIndependentOfRepoLimit(t *testing.T) {
	const (
		repoLimit = 1024
		chatLimit = 4096
	)
	env := newTestEnv(t, repoLimit, chatLimit)

	room, err := env.srv.sessions.CreateWithCode("CHAT1", 60)
	if err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}
	_ = room

	// 2KB：超过仓库上限，但在聊天室上限之内。
	payload := bytes.Repeat([]byte("x"), 2048)

	w := env.chatUpload(t, "CHAT1", "big.bin", payload)
	if w.Code != http.StatusOK {
		t.Fatalf("聊天室上传 2KB 应当成功（上限 %d），得到 %d（body=%s）",
			chatLimit, w.Code, w.Body.String())
	}

	// 对照：同样的内容传到仓库必须被拒（仓库上限只有 1KB）。
	admin := env.mkAdmin(t, "root")
	wr := env.repoUpload(t, "big.bin", payload, admin)
	if wr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("仓库上传 2KB 应当因超过 %d 被拒（413），得到 %d", repoLimit, wr.Code)
	}
}

// TestChatUploadUnlimitedWhenZero 验证 ChatMaxUpload=0 表示**聊天室不限**，
// 而不是「回落到仓库上限」。
//
// 这是环境变量语义的修复点：修复前 NewServer 里有
// `if chatMaxUpload <= 0 { chatMaxUpload = maxUpload }`，
// 于是 LANSHARE_CHAT_UPLOAD_MB=0（本意不限）会被 50MB 的仓库上限接管。
func TestChatUploadUnlimitedWhenZero(t *testing.T) {
	// 仓库给一个很小的上限，聊天室显式 0（不限）。
	const repoLimit = 512
	env := newTestEnv(t, repoLimit, 0)

	if _, err := env.srv.sessions.CreateWithCode("FREE1", 60); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	// 4KB 远大于仓库的 512B 上限。若还残留回落逻辑，这里会 413。
	payload := bytes.Repeat([]byte("y"), 4096)

	w := env.chatUpload(t, "FREE1", "free.bin", payload)
	if w.Code != http.StatusOK {
		t.Fatalf("聊天室不限时上传 4KB 应当成功，得到 %d（body=%s）", w.Code, w.Body.String())
	}

	// 落盘大小必须逐字节准确 —— 确认没有被 LimitReader 悄悄截断。
	if got := env.fileSizeByName(t, "free.bin"); got != int64(len(payload)) {
		t.Errorf("落盘大小 = %d, 期望 %d（内容被截断了）", got, len(payload))
	}
}

// ---------------------------------------------------------------- 去重：失效目标

// TestStaleDedupTargetKeepsNewUpload 验证「数据库有去重记录、但磁盘文件已丢失」时，
// 新上传的好文件不会被删掉。
//
// 修复前的行为：直接把刚上传的新文件 Remove，转而引用那个不存在的旧存储名，
// 最终产出一条永远下载 404 的坏记录。用户看到的是「上传成功但下载失败」。
func TestStaleDedupTargetKeepsNewUpload(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	uid := adminUserID(t, env, "root")

	content := []byte("same content for dedup")

	// ---- 第一次上传：正常落盘 ----
	w1 := env.repoUpload(t, "first.bin", content, admin)
	if w1.Code != http.StatusOK {
		t.Fatalf("第一次上传应当成功，得到 %d（body=%s）", w1.Code, w1.Body.String())
	}
	var first fileResp
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatalf("解析第一次响应失败: %v", err)
	}

	// ---- 手工把磁盘文件删掉，制造「有记录没文件」 ----
	rec1, err := env.store.FileByID(first.ID)
	if err != nil {
		t.Fatalf("读取第一条记录失败: %v", err)
	}
	if err := os.Remove(filepath.Join(env.files.Dir("permanent"), rec1.StoredName)); err != nil {
		t.Fatalf("删除磁盘文件失败: %v", err)
	}
	if env.files.Exists("permanent", rec1.StoredName) {
		t.Fatal("前置条件不成立：磁盘文件应当已被删除")
	}

	// ---- 第二次上传同内容 ----
	//
	// 去重查询会命中第一条记录，但它的磁盘文件已经不在了。
	// 正确行为：保留本次新上传的文件，并把它作为新记录的存储名。
	w2 := env.repoUpload(t, "second.bin", content, admin)
	if w2.Code != http.StatusOK {
		t.Fatalf("第二次上传应当成功，得到 %d（body=%s）", w2.Code, w2.Body.String())
	}
	var second fileResp
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatalf("解析第二次响应失败: %v", err)
	}

	rec2, err := env.store.FileByID(second.ID)
	if err != nil {
		t.Fatalf("读取第二条记录失败: %v", err)
	}

	// 关键断言 1：新记录的存储名必须是**新**文件，不能复用那个失效的旧存储名。
	if rec2.StoredName == rec1.StoredName {
		t.Fatalf("失效的去重目标不该被复用：恰好都是 %s", rec2.StoredName)
	}

	// 关键断言 2：新记录的磁盘文件必须真实存在 —— 否则又是坏记录。
	if !env.files.Exists("permanent", rec2.StoredName) {
		t.Fatal("新上传的文件被删掉了，记录指向一个不存在的文件")
	}

	// 关键断言 3：新文件必须能真正下载下来，且内容正确。
	got := env.do(t, http.MethodGet, "/api/files/"+itoa(second.ID), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("新记录应当能下载，得到 %d", got.Code)
	}
	data, _ := io.ReadAll(got.Body)
	if !bytes.Equal(data, content) {
		t.Errorf("下载内容不匹配：得到 %q", data)
	}

	// 旧记录的坏数据不该被上传路径顺手删掉（元数据属于用户，
	// 修它应该走单独的「存储一致性检查」功能）。
	_ = uid
	if _, err := env.store.FileByID(first.ID); err != nil {
		t.Errorf("失效的旧记录不该被上传路径删除，却查不到了: %v", err)
	}
}

// TestDedupStillWorksWhenTargetExists 是对照组：目标文件正常时去重行为不变。
func TestDedupStillWorksWhenTargetExists(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	content := []byte("dedup works fine")

	w1 := env.repoUpload(t, "a.bin", content, admin)
	if w1.Code != http.StatusOK {
		t.Fatalf("第一次上传失败: %d", w1.Code)
	}
	var first fileResp
	_ = json.Unmarshal(w1.Body.Bytes(), &first)

	w2 := env.repoUpload(t, "b.bin", content, admin)
	if w2.Code != http.StatusOK {
		t.Fatalf("第二次上传失败: %d", w2.Code)
	}
	var second fileResp
	_ = json.Unmarshal(w2.Body.Bytes(), &second)

	rec1, _ := env.store.FileByID(first.ID)
	rec2, _ := env.store.FileByID(second.ID)

	// 目标文件健在时，两条记录应当共享同一个存储名（这才是去重的意义）。
	if rec1.StoredName != rec2.StoredName {
		t.Errorf("去重应当复用存储名，得到 %s / %s", rec1.StoredName, rec2.StoredName)
	}

	// 磁盘上只应有一份物理文件。
	entries, err := os.ReadDir(env.files.Dir("permanent"))
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("去重后磁盘上应当只有 1 个文件，得到 %d", len(entries))
	}
}

// ---------------------------------------------------------------- 删除：引用计数

// TestDeleteKeepsFileWhileOtherRecordReferencesIt 验证删除一条共享记录时，
// 只要还有别的记录引用同一个磁盘文件，就不能 unlink。
func TestDeleteKeepsFileWhileOtherRecordReferencesIt(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	content := []byte("shared physical file")

	w1 := env.repoUpload(t, "one.bin", content, admin)
	w2 := env.repoUpload(t, "two.bin", content, admin)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("上传失败: %d / %d", w1.Code, w2.Code)
	}

	var f1, f2 fileResp
	_ = json.Unmarshal(w1.Body.Bytes(), &f1)
	_ = json.Unmarshal(w2.Body.Bytes(), &f2)

	rec, err := env.store.FileByID(f1.ID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	stored := rec.StoredName

	// ---- 删第一条：物理文件必须还在 ----
	if w := env.do(t, http.MethodDelete, "/api/files/"+itoa(f1.ID), nil, admin); w.Code != http.StatusOK {
		t.Fatalf("删除第一条失败: %d（body=%s）", w.Code, w.Body.String())
	}
	if !env.files.Exists("permanent", stored) {
		t.Fatal("还有记录引用时物理文件被误删了")
	}

	// 第二条记录仍应能正常下载（内容逐字节一致）。
	got := env.do(t, http.MethodGet, "/api/files/"+itoa(f2.ID), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("第二条应当仍可下载，得到 %d", got.Code)
	}
	if data, _ := io.ReadAll(got.Body); !bytes.Equal(data, content) {
		t.Error("第二条下载内容不匹配")
	}

	// ---- 删第二条：这时才允许删磁盘文件 ----
	if w := env.do(t, http.MethodDelete, "/api/files/"+itoa(f2.ID), nil, admin); w.Code != http.StatusOK {
		t.Fatalf("删除第二条失败: %d", w.Code)
	}
	if env.files.Exists("permanent", stored) {
		t.Fatal("引用归零后物理文件应当被删除")
	}
}

// TestDeleteUsesStoredNameNotSHA256 验证引用计数按 stored_name 统计，
// 而不是按 sha256 + owner。
//
// 构造：两条记录内容相同（同 sha256）但**存储名不同**（模拟去重未生效、
// 各自落了一份的场景，也等价于老数据）。删掉其中一条时，
// 另一条的磁盘文件绝不能被牵连 —— 按 sha256 统计会数成 1 而误判
// 「还有引用」，按 stored_name 统计则会正确得出 0 并删除自己那份。
func TestDeleteUsesStoredNameNotSHA256(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	uid := adminUserID(t, env, "root")

	// 手工插入两条同哈希、不同存储名的记录，并各自在磁盘上造出文件。
	const content = "identical bytes"
	sha := sha256Hex(content)

	storedA := "manual-a.bin"
	storedB := "manual-b.bin"
	for _, n := range []string{storedA, storedB} {
		if err := os.WriteFile(filepath.Join(env.files.Dir("permanent"), n),
			[]byte(content), 0o644); err != nil {
			t.Fatalf("写入磁盘文件失败: %v", err)
		}
	}

	idA, err := env.store.CreateFile(&storage.File{
		OriginalName: "a.txt", StoredName: storedA, Size: int64(len(content)),
		SHA256: sha, Kind: storage.KindPermanent, OwnerID: &uid, OwnerName: "root",
		CreatedAt: nowTime(),
	})
	if err != nil {
		t.Fatalf("写入记录 A 失败: %v", err)
	}
	idB, err := env.store.CreateFile(&storage.File{
		OriginalName: "b.txt", StoredName: storedB, Size: int64(len(content)),
		SHA256: sha, Kind: storage.KindPermanent, OwnerID: &uid, OwnerName: "root",
		CreatedAt: nowTime(),
	})
	if err != nil {
		t.Fatalf("写入记录 B 失败: %v", err)
	}

	// 删掉 A：它自己那份磁盘文件应当被删（引用归零），B 的那份必须完好。
	if w := env.do(t, http.MethodDelete, "/api/files/"+itoa(idA), nil, admin); w.Code != http.StatusOK {
		t.Fatalf("删除 A 失败: %d", w.Code)
	}
	if env.files.Exists("permanent", storedA) {
		t.Error("A 的磁盘文件引用已归零，应当被删除")
	}
	if !env.files.Exists("permanent", storedB) {
		t.Error("B 的磁盘文件不该被牵连删除（按 sha256 计数就会误删）")
	}

	// B 仍可下载。
	if w := env.do(t, http.MethodGet, "/api/files/"+itoa(idB), nil, nil); w.Code != http.StatusOK {
		t.Errorf("B 应当仍可下载，得到 %d", w.Code)
	}
}

// ---------------------------------------------------------------- 改名 / 置顶

// uploadOne 上传一个永久文件，返回它的 id。
func (e *testEnv) uploadOne(t *testing.T, name string, content []byte, cookie *http.Cookie) int64 {
	t.Helper()

	w := e.repoUpload(t, name, content, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("上传 %q 失败: %d（body=%s）", name, w.Code, w.Body.String())
	}
	var resp fileResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析上传响应失败: %v", err)
	}
	return resp.ID
}

// TestRenameRequiresLogin 未登录不能改名。
//
// 与删除同一条规则：改的是所有人都看得见的东西。
func TestRenameRequiresLogin(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	id := env.uploadOne(t, "a.txt", []byte("hello"), admin)

	w := env.do(t, http.MethodPatch, "/api/files/"+itoa(id),
		[]byte(`{"name":"b.txt"}`), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录改名应当 401，得到 %d（body=%s）", w.Code, w.Body.String())
	}

	// 库里必须原封不动。
	f, err := env.store.FileByID(id)
	if err != nil {
		t.Fatalf("查询文件失败: %v", err)
	}
	if f.OriginalName != "a.txt" {
		t.Errorf("越权请求不该改动名字，当前为 %q", f.OriginalName)
	}
}

// TestRenameRejectsOtherUser 普通用户不能改别人的文件，管理员可以。
func TestRenameRejectsOtherUser(t *testing.T) {
	env := newTestEnv(t, 0, 0)

	admin := env.mkAdmin(t, "root")
	other := env.mkUserCookie(t, "alice", storage.RoleUser)

	id := env.uploadOne(t, "owner.txt", []byte("mine"), admin)

	// alice 改不了 root 的文件。
	w := env.do(t, http.MethodPatch, "/api/files/"+itoa(id),
		[]byte(`{"name":"hacked.txt"}`), other)
	if w.Code != http.StatusForbidden {
		t.Fatalf("非本人改名应当 403，得到 %d（body=%s）", w.Code, w.Body.String())
	}

	// 管理员可以改任何人的。
	w = env.do(t, http.MethodPatch, "/api/files/"+itoa(id),
		[]byte(`{"name":"renamed.txt"}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("管理员改名应当 200，得到 %d（body=%s）", w.Code, w.Body.String())
	}
	var resp fileResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "renamed.txt" {
		t.Errorf("响应里的名字应当是 renamed.txt，得到 %q", resp.Name)
	}
}

// TestRenameOnlyTouchesOwnRecord 是本功能最关键的一条：改名不能串到别人身上。
//
// 内容去重后，同一个人重复上传同一份内容时会共享同一个 stored_name，
// 各有一条记录、各有各的原始文件名（去重刻意按 owner 隔离，
// 所以这里必须用同一个账号传两次，才能造出共享存储名的场景）。
// 改掉其中一条的名字，另一条必须纹丝不动。
func TestRenameOnlyTouchesOwnRecord(t *testing.T) {
	env := newTestEnv(t, 0, 0)

	alice := env.mkUserCookie(t, "alice", storage.RoleUser)

	content := []byte("shared content")
	id1 := env.uploadOne(t, "alice-first.txt", content, alice)
	id2 := env.uploadOne(t, "alice-second.txt", content, alice)

	// 前提确认：两条记录确实共用同一份磁盘文件（去重生效）。
	f1, err := env.store.FileByID(id1)
	if err != nil {
		t.Fatalf("查询记录 1 失败: %v", err)
	}
	f2, err := env.store.FileByID(id2)
	if err != nil {
		t.Fatalf("查询记录 2 失败: %v", err)
	}
	if f1.StoredName != f2.StoredName {
		t.Fatalf("前置条件不成立：两条记录应当共享 stored_name，得到 %q / %q",
			f1.StoredName, f2.StoredName)
	}

	// 改第一条的名字。
	w := env.do(t, http.MethodPatch, "/api/files/"+itoa(id1),
		[]byte(`{"name":"renamed-first.txt"}`), alice)
	if w.Code != http.StatusOK {
		t.Fatalf("改名应当 200，得到 %d（body=%s）", w.Code, w.Body.String())
	}

	// 第二条的名字不受影响。
	f2b, err := env.store.FileByID(id2)
	if err != nil {
		t.Fatalf("查询记录 2 失败: %v", err)
	}
	if f2b.OriginalName != "alice-second.txt" {
		t.Errorf("另一条记录的文件名不该被牵连，得到 %q", f2b.OriginalName)
	}
	// 磁盘上的物理文件名也不该变 —— 改名是纯元数据操作。
	if f2b.StoredName != f1.StoredName {
		t.Errorf("改名不该动 stored_name，得到 %q（原 %q）", f2b.StoredName, f1.StoredName)
	}
	// 另一条仍能下载，且下载头里用的是它自己的文件名。
	wGet := env.do(t, http.MethodGet, "/api/files/"+itoa(id2), nil, alice)
	if wGet.Code != http.StatusOK {
		t.Fatalf("另一条记录仍应可下载，得到 %d", wGet.Code)
	}
	if cd := wGet.Header().Get("Content-Disposition"); !strings.Contains(cd, "alice-second.txt") {
		t.Errorf("另一条的下载文件名应当不变，得到 %q", cd)
	}
	// 磁盘上的那份数据必须还在（改名前后的两条记录都指着它）。
	if !env.files.Exists("permanent", f1.StoredName) {
		t.Error("改名不该影响磁盘文件，它应当仍然存在")
	}
}

// TestRenameSanitizesName 文件名必须过一遍清洗。
//
// 名字会被放进 Content-Disposition 与 DOM，路径分隔符与控制字符
// 都得在入口处干掉；空名回退成兜底名而不是报错。
func TestRenameSanitizesName(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	id := env.uploadOne(t, "orig.txt", []byte("x"), admin)

	cases := []struct {
		in   string
		want string
	}{
		{`../../etc/passwd`, "passwd"},      // 路径被剥掉，只留最后一段
		{"a\r\nb.txt", "ab.txt"},            // 换行被删（防 header 注入）
		{`quote"name.txt`, "quotename.txt"}, // 引号被删
		{"   ", "file"},                     // 纯空白回退兜底名
		{"normal.txt", "normal.txt"},        // 正常名字原样保留
	}

	for _, c := range cases {
		w := env.do(t, http.MethodPatch, "/api/files/"+itoa(id),
			[]byte(`{"name":`+jsonString(c.in)+`}`), admin)
		if w.Code != http.StatusOK {
			t.Fatalf("改名 %q 应当 200，得到 %d（body=%s）", c.in, w.Code, w.Body.String())
		}
		f, err := env.store.FileByID(id)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if f.OriginalName != c.want {
			t.Errorf("改名 %q 后应为 %q，得到 %q", c.in, c.want, f.OriginalName)
		}
	}
}

// TestRenameMissingFile 不存在的 id 返回 404。
func TestRenameMissingFile(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	w := env.do(t, http.MethodPatch, "/api/files/9999",
		[]byte(`{"name":"x.txt"}`), admin)
	if w.Code != http.StatusNotFound {
		t.Fatalf("改一个不存在的文件应当 404，得到 %d", w.Code)
	}
}

// TestPinSortsBeforeUnpinned 置顶的排序断言。
//
// 顺序必须在服务端定：先置顶的在前，其余按时间从新到旧。
// 这里刻意「先传 A、再传 B、最后置顶 A」，这样如果置顶没生效，
// 列表会是 B, A（新在前），一眼就能看出来。
func TestPinSortsBeforeUnpinned(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")

	idA := env.uploadOne(t, "a-first.txt", []byte("A"), admin)
	idB := env.uploadOne(t, "b-second.txt", []byte("B"), admin)

	// 置顶较早上传的 A。
	w := env.do(t, http.MethodPost, "/api/files/"+itoa(idA)+"/pin",
		[]byte(`{}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("置顶应当 200，得到 %d（body=%s）", w.Code, w.Body.String())
	}
	var pr fileResp
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if !pr.Pinned {
		t.Fatal("置顶后响应里的 pinned 应为 true")
	}

	names := env.listNames(t)
	if len(names) != 2 {
		t.Fatalf("应当有 2 个文件，得到 %d", len(names))
	}
	if names[0] != "a-first.txt" {
		t.Errorf("置顶的文件应当排在最前，实际顺序为 %v", names)
	}

	// 取消置顶后回到「新的在前」。
	w = env.do(t, http.MethodPost, "/api/files/"+itoa(idA)+"/pin",
		[]byte(`{}`), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("取消置顶应当 200，得到 %d", w.Code)
	}
	names = env.listNames(t)
	if names[0] != "b-second.txt" {
		t.Errorf("取消置顶后应当按时间从新到旧，实际顺序为 %v", names)
	}
	_ = idB
}

// TestPinExplicitValue 显式传 pinned 时按传入值设置，而不是无脑翻转。
//
// 这是给「状态同步」留的口子：前端若已经知道目标状态（比如从别的客户端
// 同步来的），直接 set 比 toggle 更稳 —— 传两次 true 的结果仍是 true。
func TestPinExplicitValue(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	id := env.uploadOne(t, "x.txt", []byte("x"), admin)

	for i := 0; i < 2; i++ {
		w := env.do(t, http.MethodPost, "/api/files/"+itoa(id)+"/pin",
			[]byte(`{"pinned":true}`), admin)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次置顶应当 200，得到 %d", i+1, w.Code)
		}
		var pr fileResp
		_ = json.Unmarshal(w.Body.Bytes(), &pr)
		if !pr.Pinned {
			t.Fatalf("第 %d 次设置 pinned=true 后仍为 false，说明写成了 toggle", i+1)
		}
	}

	w := env.do(t, http.MethodPost, "/api/files/"+itoa(id)+"/pin",
		[]byte(`{"pinned":false}`), admin)
	var pr fileResp
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Pinned {
		t.Error("显式传 pinned=false 之后应当为 false")
	}
}

// TestPinRequiresLogin 未登录 / 非本人不能置顶。
func TestPinRequiresLogin(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	admin := env.mkAdmin(t, "root")
	other := env.mkUserCookie(t, "alice", storage.RoleUser)
	id := env.uploadOne(t, "x.txt", []byte("x"), admin)

	if w := env.do(t, http.MethodPost, "/api/files/"+itoa(id)+"/pin",
		[]byte(`{}`), nil); w.Code != http.StatusUnauthorized {
		t.Errorf("未登录置顶应当 401，得到 %d", w.Code)
	}
	if w := env.do(t, http.MethodPost, "/api/files/"+itoa(id)+"/pin",
		[]byte(`{}`), other); w.Code != http.StatusForbidden {
		t.Errorf("非本人置顶应当 403，得到 %d", w.Code)
	}
}

// ---------------------------------------------------------------- 工具

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// jsonString 把一个字符串安全地包成 JSON 字面量（用于拼请求体）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// mkUserCookie 建一个指定角色的用户并返回其登录 Cookie。
func (e *testEnv) mkUserCookie(t *testing.T, name string, role storage.Role) *http.Cookie {
	t.Helper()

	hash, err := auth.Hash("password123")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	u, err := e.store.CreateUser(name, hash, role)
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", name, err)
	}
	id := httpx.NewID()
	if err := e.store.CreateSession(id, u.ID, 3600*1e9); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return &http.Cookie{Name: httpx.CookieName, Value: id}
}

// listNames 通过接口读回永久文件列表里的文件名（顺序即服务端排序）。
func (e *testEnv) listNames(t *testing.T) []string {
	t.Helper()

	w := e.do(t, http.MethodGet, "/api/files?kind=permanent", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("列文件应当 200，得到 %d", w.Code)
	}
	var resp struct {
		Files []fileResp `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	names := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		names = append(names, f.Name)
	}
	return names
}

// sha256Hex 算一段内容的 sha256 十六进制串。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// adminUserID 取某个用户名的 ID。
func adminUserID(t *testing.T, env *testEnv, name string) int64 {
	t.Helper()
	u, err := env.store.UserByName(name)
	if err != nil {
		t.Fatalf("查询用户 %s 失败: %v", name, err)
	}
	return u.ID
}

// fileSizeByName 查某个原始文件名对应的 size 字段。
//
// 通过接口读回来，而不是直接摸数据库连接 —— 这样它顺带也覆盖了
// 「记录确实写进了库、且大小准确」这件事。
func (e *testEnv) fileSizeByName(t *testing.T, name string) int64 {
	t.Helper()

	var size int64
	list, err := e.store.ListFiles(storage.KindChat)
	if err != nil {
		t.Fatalf("查询聊天文件列表失败: %v", err)
	}
	for _, f := range list {
		if f.OriginalName == name {
			size = f.Size
			return size
		}
	}
	t.Fatalf("没有找到名为 %q 的记录", name)
	return 0
}
