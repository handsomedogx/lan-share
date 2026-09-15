package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"lanshare/internal/files"
	"lanshare/internal/storage"
)

// 本文件覆盖「聊天里的图片内联显示」这条链路的两端：
//
//	① 服务端判定 —— 哪个文件名算图片、WS 卡片会不会带上 isImage；
//	② 内联响应 —— ?inline=1 只有在白名单内才生效，且类型正确。
//
// 之所以两边都要测：如果判定变了而响应头没变（或反过来），
// 前端会拿到「卡片说是图片、请求回来却是 attachment」的矛盾结果，
// 表现就是缩略图永远加载失败 —— 这种两处漂移正是要防的东西。

// ---------------------------------------------------------------- 类型判定

func TestImageExtWhitelist(t *testing.T) {
	yes := []string{
		"a.png", "b.PNG", "c.jpg", "d.jpeg", "e.JFIF",
		"f.gif", "g.webp", "h.bmp", "i.avif", "j.ico",
		"相片 2024.图片.PNG", // 多点 + 中文 + 空格
	}
	for _, name := range yes {
		if !files.IsImageName(name) {
			t.Errorf("应当识别为图片: %q", name)
		}
	}

	no := []string{
		"", "readme.txt", "archive.zip", "noext",
		// SVG 是刻意排除的：它是 XML，可内嵌 <script>，
		// 而聊天文件是免登录可访问的 —— 内联它等于开一个同源脚本执行入口。
		"logo.svg", "a.SVG",
		// 危险/伪装名一律不算图片。
		"evil.png.html", "x.png.exe", "trailing.png.",
	}
	for _, name := range no {
		if files.IsImageName(name) {
			t.Errorf("不应识别为图片: %q", name)
		}
	}
}

// TestImageNameStripsPath 确认判定只看最后一段 ——
// 即便调用方漏了 SafeDisplayName，带路径的名字也不会被当成别的类型。
//
// 实测语义：
//   - 正斜杠路径：path.Ext 取最后一段，识别为图片；
//   - 反斜杠路径：path.Ext 同样会返回 ".png"（它只按最后一个点切），
//     所以也会被识别 —— 这**不是漏洞**，因为即使识别成图片，
//     后续也只会走到 inline 分支去 Open(storedName)，而 storedName
//     是服务端自己生成的随机名，跟用户给的名字毫无关系。
func TestImageNameStripsPath(t *testing.T) {
	if files.ImageExt("/etc/passwd.png") != ".png" {
		t.Error("带路径的图片名应当仍被识别")
	}
	if !files.IsImageName(`C:\pics\a.png`) {
		t.Error("反斜杠路径按 path.Ext 的语义同样应识别为 .png")
	}
	// 关键性质：无论路径怎么写，都不可能因为「点号后缀」而把
	// 一个非图片变成图片 —— 判定只用扩展名，不接受任何路径语义。
	if files.IsImageName(`C:\pics\a.txt`) {
		t.Error(".txt 在任何路径形式下都不该被当成图片")
	}
}

func TestImageMIMELookup(t *testing.T) {
	cases := map[string]string{
		".png":  "image/png",
		".JPG":  "image/jpeg", // 大小写不敏感
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".svg":  "", // 白名单外
		".txt":  "",
		"":      "",
	}
	for ext, want := range cases {
		if got := files.ImageMIME(ext); got != want {
			t.Errorf("ImageMIME(%q) = %q, 期望 %q", ext, got, want)
		}
	}
}

// TestImageExtsMatchesMIME 保证「扩展名清单」和「MIME 表」是同一份数据 ——
// IconExts 若与 imageMIME 漂移，前端兜底清单会和服务端判定对不上。
func TestImageExtsMatchesMIME(t *testing.T) {
	exts := files.IconExts()
	if len(exts) == 0 {
		t.Fatal("IconExts 不应为空")
	}
	for _, ext := range exts {
		if files.ImageMIME(ext) == "" {
			t.Errorf("清单里的 %q 在 MIME 表里找不到", ext)
		}
	}
}

// ---------------------------------------------------------------- 内联响应

// chatFileID 往房间里传一个文件，返回它的 id。
func (e *testEnv) chatFileID(t *testing.T, code, filename string, content []byte) int64 {
	t.Helper()
	w := e.chatUpload(t, code, filename, content)
	if w.Code != http.StatusOK {
		t.Fatalf("上传 %s 失败: status=%d body=%s", filename, w.Code, w.Body.String())
	}
	var resp chatFileResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析上传响应失败: %v", err)
	}
	if resp.ID <= 0 {
		t.Fatalf("上传未返回有效 id: %+v", resp)
	}
	return resp.ID
}

// getChatFile 请求聊天文件接口，可以带查询串。
func (e *testEnv) getChatFile(t *testing.T, id int64, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet,
		"/api/chat-files/"+strconv.FormatInt(id, 10)+query, nil)
	w := httptest.NewRecorder()
	e.srv.Routes().ServeHTTP(w, r)
	return w
}

// 一张 1x1 的合法 PNG（最小的真实图片）。
var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

// TestChatFileInlineServesImageMIME 是内联预览的核心断言：
// 图片 + inline=1 → 正确的 image/* 类型 + inline 头，内容逐字节一致。
func TestChatFileInlineServesImageMIME(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	if _, err := env.srv.sessions.CreateWithCode("IMG1", 60); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	id := env.chatFileID(t, "IMG1", "shot.png", tinyPNG)

	w := env.getChatFile(t, id, "?inline=1")
	if w.Code != http.StatusOK {
		t.Fatalf("inline 请求失败: status=%d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, 期望 image/png", got)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "inline") {
		t.Errorf("Content-Disposition = %q, 期望以 inline 开头", cd)
	}
	// 名字仍要给出，便于右键另存为时拿到原文件名。
	if !strings.Contains(cd, "shot.png") {
		t.Errorf("Content-Disposition 应当带上原文件名: %q", cd)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("内联响应也必须带 nosniff")
	}
	if !bytes.Equal(w.Body.Bytes(), tinyPNG) {
		t.Errorf("内联内容与上传不一致: got=%dB want=%dB",
			w.Body.Len(), len(tinyPNG))
	}
}

// TestChatFileInlineIgnoredForNonImage 是这条通道的安全边界：
// 非图片即使显式带 inline=1，也只能是 attachment。
//
// 没有这条断言的话，?inline=1 就变成了「把上传的任意内容当页面渲染」的开关 ——
// 而聊天入口是免登录的。
func TestChatFileInlineIgnoredForNonImage(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	if _, err := env.srv.sessions.CreateWithCode("IMG2", 60); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	html := []byte("<script>alert(1)</script>")
	id := env.chatFileID(t, "IMG2", "page.html", html)

	w := env.getChatFile(t, id, "?inline=1")
	if w.Code != http.StatusOK {
		t.Fatalf("请求失败: status=%d", w.Code)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("HTML 不应被内联，Content-Disposition = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("非图片必须是 octet-stream，got=%q", got)
	}
}

// TestChatFileInlineRejectedForSVG 单独钉住 SVG 这条：
// 它在 MIME 表里**故意缺席**，因此必须回落成附件。
//
// SVG 是 XML，能内嵌 <script>；聊天文件又免登录可访问。
// 一旦内联，就等于「任何进过房间的人都能往本页同源里塞一段脚本」。
func TestChatFileInlineRejectedForSVG(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	if _, err := env.srv.sessions.CreateWithCode("IMG3", 60); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	id := env.chatFileID(t, "IMG3", "evil.svg", svg)

	w := env.getChatFile(t, id, "?inline=1")
	if got := w.Header().Get("Content-Type"); got == "image/svg+xml" {
		t.Fatal("SVG 绝不能被内联为 image/svg+xml（可执行脚本）")
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("SVG 必须是附件：Content-Disposition = %q", got)
	}
}

// TestChatFileWithoutInlineIsStillAttachment 确认没带 inline 时行为不变 ——
// 原来的下载语义（附件 + octet-stream）不能被这次改动碰到。
func TestChatFileWithoutInlineIsStillAttachment(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	if _, err := env.srv.sessions.CreateWithCode("IMG4", 60); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	id := env.chatFileID(t, "IMG4", "pic.png", tinyPNG)

	w := env.getChatFile(t, id, "")
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("不带 inline 时仍应是附件，got=%q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("不带 inline 时类型应是 octet-stream，got=%q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), tinyPNG) {
		t.Error("下载内容应当不变")
	}
}

// TestChatFileInlineOtherValueIgnored 只有精确的 "1" 才开启内联。
//
// 用「精确匹配」而不是「参数存在即生效」：后者会让 ?inline=0 / ?inline=false
// 也变成内联，语义上说不通，也容易在改前端时无意间把所有图片都放成内联。
func TestChatFileInlineOtherValueIgnored(t *testing.T) {
	env := newTestEnv(t, 0, 0)
	if _, err := env.srv.sessions.CreateWithCode("IMG5", 60); err != nil {
		t.Fatalf("创建房间失败: %v", err)
	}

	id := env.chatFileID(t, "IMG5", "pic.png", tinyPNG)

	for _, q := range []string{"?inline=0", "?inline=false", "?inline=yes", "?inline="} {
		w := env.getChatFile(t, id, q)
		if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("%s 不应触发内联，Content-Type = %q", q, got)
		}
	}
}

// TestChatFileInlineRoomGoneStill410 确认内联分支没有绕过「房间没了就 410」——
// 这是「房间过期 = 文件立即失效」的语义保证，缩略图也不例外。
//
// 构造方式：直接把文件记录的 room_code 写成**一个从来没建过的房间号**，
// 也就是「房间已被回收、文件记录还没轮到删」的那个中间态。
// 这在 handleChatDownload 里走的正是 `s.sessions.Get(f.RoomCode) == nil`
// 分支 —— 与真实过期房间完全同一条代码路径。
//
// （api 包拿不到 session.Room 的未导出字段，没法把 TTL 拨到过去；
// 真正的过期回收由 internal/session 的单测覆盖。）
func TestChatFileInlineRoomGoneStill410(t *testing.T) {
	env := newTestEnv(t, 0, 0)

	// 直接造一条指向不存在房间的聊天文件记录 —— 不需要先把房间建起来再改，
	// 因为被测的判定只关心「RoomCode 能不能在内存里查到」。
	rec := &storage.File{
		OriginalName: "pic.png",
		StoredName:   "not-a-real-file.bin",
		Size:         int64(len(tinyPNG)),
		Kind:         storage.KindChat,
		RoomCode:     "GONE99",
		OwnerName:    "匿名设备",
		CreatedAt:    time.Now(),
	}
	id, err := env.store.CreateFile(rec)
	if err != nil {
		t.Fatalf("写入文件记录失败: %v", err)
	}

	w := env.getChatFile(t, id, "?inline=1")
	if w.Code != http.StatusGone {
		t.Errorf("房间不存在时内联应返回 410，got=%d body=%s", w.Code, w.Body.String())
	}

	// 对照：同一个文件不带 inline 也必须 410 —— 两条分支共用同一段前置校验。
	if w := env.getChatFile(t, id, ""); w.Code != http.StatusGone {
		t.Errorf("不带 inline 也应 410，got=%d", w.Code)
	}
}
