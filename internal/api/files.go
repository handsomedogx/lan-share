package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/storage"
)

// fileResp 是返回给前端的文件元数据。
//
// 刻意不返回 stored_name：存储名是服务端内部实现细节，
// 只通过 /api/files/{id} 暴露下载，避免客户端能拼出磁盘路径。
type fileResp struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SizeText    string `json:"sizeText"`
	SHA256      string `json:"sha256,omitempty"`
	Kind        string `json:"kind"`
	Owner       string `json:"owner"`
	CreatedAt   int64  `json:"createdAt"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
	DownloadURL string `json:"downloadUrl"`
}

func toFileResp(f *storage.File) fileResp {
	r := fileResp{
		ID:          f.ID,
		Name:        f.OriginalName,
		Size:        f.Size,
		SizeText:    files.HumanSize(f.Size),
		SHA256:      f.SHA256,
		Kind:        string(f.Kind),
		Owner:       f.OwnerName,
		CreatedAt:   f.CreatedAt.UnixMilli(),
		DownloadURL: fmt.Sprintf("/api/files/%d", f.ID),
	}
	if f.ExpiresAt != nil {
		r.ExpiresAt = f.ExpiresAt.UnixMilli()
	}
	return r
}

// ---------------------------------------------------------------- 列表

// handleListFiles 列出文件仓库内容。
//
// 列表对所有人开放（含未登录）—— 局域网里的取用不必先建账号。
// 写操作（上传、删除）才需要登录。
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	kind := storage.FileKind(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = storage.KindPermanent
	}
	if kind != storage.KindPermanent && kind != storage.KindTemporary {
		httpx.Fail(w, http.StatusBadRequest, "未知的文件类型")
		return
	}

	list, err := s.store.ListFiles(kind)
	if err != nil {
		s.log.Error("查询文件列表失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件列表失败")
		return
	}

	out := make([]fileResp, 0, len(list))
	var total int64
	for _, f := range list {
		out = append(out, toFileResp(f))
		total += f.Size
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"files":     out,
		"count":     len(out),
		"totalSize": total,
		"totalText": files.HumanSize(total),
	})
}

// ---------------------------------------------------------------- 上传

// handleUpload 接收 multipart 上传。
//
// 字段：
//
//	file  必填，文件内容
//	kind  permanent（默认）| temporary
//	name  可选，覆盖原始文件名
//
// 表单字段名保持 "file"，与浏览器 FormData 的常规写法一致。
//
// 注意字段顺序：必须把 kind / name 放在 file 之前。
// 服务端是流式解析，遇到 file part 就立刻开始落盘，
// 之后出现的字段已经读不到了（详见 upload.go 里 uploadStream 的注释）。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	up, err := openUploadStream(w, r, "file", s.maxUpload)
	if err != nil {
		switch {
		case errors.Is(err, ErrUploadTooLarge):
			httpx.Fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件超过上限 %s", files.HumanSize(s.maxUpload)))
		case errors.Is(err, ErrNoFilePart):
			httpx.Fail(w, http.StatusBadRequest, "没有收到文件")
		default:
			httpx.Fail(w, http.StatusBadRequest, "解析上传内容失败")
		}
		return
	}
	// 无论成功失败都要关掉 part；落盘失败时存储层自己会删 .part。
	defer up.Close()

	kind := storage.FileKind(up.Field("kind"))
	if kind == "" {
		// 正常请求一定会先发 kind（前端保证）。走到这里说明是别的客户端，
		// 此时文件类型未知，按最严格的 permanent 处理 —— 它要求登录，
		// 宁可拒绝，也不能让身份未明的请求在不知道存哪儿的情况下落盘。
		kind = storage.KindPermanent
	}
	if kind != storage.KindPermanent && kind != storage.KindTemporary {
		httpx.Fail(w, http.StatusBadRequest, "未知的文件类型")
		return
	}

	// 永久文件必须登录。
	var owner *storage.User
	if kind == storage.KindPermanent {
		owner = httpx.CurrentUser(r, s.store)
		if owner == nil {
			httpx.Fail(w, http.StatusUnauthorized, "请先登录后再上传到文件仓库")
			return
		}
	} else {
		// 临时文件可选登录（登录了就记名，方便追溯）。
		owner = httpx.CurrentUser(r, s.store)
	}

	displayName := files.SafeDisplayName(up.Field("name"))
	if displayName == "file" {
		displayName = files.SafeDisplayName(up.Filename)
	}

	// 到这里才开始真正落盘：网络 → 小缓冲 → 磁盘，只有一次写入。
	res, err := s.files.Save(string(kind), up.Body)
	if err != nil {
		if errors.Is(err, files.ErrTooLarge) || isRequestBodyTooLarge(err) {
			httpx.Fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件超过上限 %s", files.HumanSize(s.maxUpload)))
			return
		}
		s.log.Error("保存上传文件失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "保存文件失败")
		return
	}

	// ---- 内容去重（只对文件仓库，且只在同一上传者内）----
	//
	// 磁盘上的数据按内容共享，数据库记录却是一人一条 ——
	// 这样「每个人看到自己的文件名、各自删各自的」都成立，
	// 而同一份内容在磁盘上只占一份空间。
	//
	// 为什么要限定 owner：磁盘是共享的，但记录是私有的。
	// 若不限定，A 只要上传一份相同内容就能复用到 B 的存储名，
	// 进而通过「删自己的」把 B 的文件删掉。
	//
	// 为什么只对永久文件做：temporary / chat 的删磁盘逻辑是
	// 「记录没了就 unlink」。它们之间复用存储名会导致一方到期、
	// 另一方的文件凭空消失。仓库文件是「所有记录都没了才删」，
	// 才是唯一安全的场景。
	stored := res.StoredName
	deduped := false
	var dedupID int64

	if kind == storage.KindPermanent && owner != nil {
		// 顺序很关键：**先删刚写下的小副本，再复用旧存储名**。
		//
		// 反过来的话，一旦之后写库失败需要回滚，回滚删掉的是那个**共享**的
		// 旧存储名 —— 会把旧记录指向的文件一起删掉，旧记录变成指向空文件。
		// 按现在的顺序，最坏情况只是白写了一次盘，没有任何记录受损。
		if old, lookErr := s.store.FileBySHA256(res.SHA256, kind, owner.ID); lookErr == nil {
			if rmErr := s.files.Remove(string(kind), res.StoredName); rmErr != nil {
				s.log.Error("去重时删除重复副本失败: %v", rmErr)
				httpx.Fail(w, http.StatusInternalServerError, "保存文件失败")
				return
			}
			stored = old.StoredName
			deduped = true
			dedupID = old.ID
		} else if !errors.Is(lookErr, storage.ErrNotFound) {
			// 查库异常不该阻塞上传：退化为「不去重」，只是多占一份磁盘。
			s.log.Error("查询重复文件失败: %v", lookErr)
		}
	}

	rec := &storage.File{
		OriginalName: displayName,
		StoredName:   stored,
		Size:         res.Size,
		SHA256:       res.SHA256,
		Kind:         kind,
		OwnerName:    "匿名设备",
		CreatedAt:    nowTime(),
	}
	if owner != nil {
		id := owner.ID
		rec.OwnerID = &id
		rec.OwnerName = owner.Username
	}

	// 临时文件设定过期时间，交给 cleanup 协程回收。
	if kind == storage.KindTemporary {
		t := nowTime().Add(tempTTL(s.tempFileTTL))
		rec.ExpiresAt = &t
	}

	id, err := s.store.CreateFile(rec)
	if err != nil {
		// 记录写库失败要把刚落盘的文件删掉，否则留下无人引用的垃圾。
		// 但复用的那份是共享文件，删了会连带毁掉旧记录，绝不能碰。
		if !deduped {
			_ = s.files.Remove(string(kind), res.StoredName)
		}
		s.log.Error("写入文件记录失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "保存文件记录失败")
		return
	}
	rec.ID = id

	// 文件上传属于必须记录的事件（文档第 18 节）。
	if deduped {
		s.log.Info("文件上传（内容重复，复用磁盘文件）: id=%d dupOf=%d name=%q size=%d user=%s ip=%s",
			id, dedupID, displayName, res.Size, rec.OwnerName, httpx.ClientIP(r))
	} else {
		s.log.Info("文件上传: id=%d name=%q size=%d kind=%s user=%s ip=%s",
			id, displayName, res.Size, kind, rec.OwnerName, httpx.ClientIP(r))
	}

	httpx.WriteJSON(w, http.StatusOK, toFileResp(rec))
}

// ---------------------------------------------------------------- 下载

// handleDownload 按 ID 下载文件，走普通 HTTP，不使用 WebSocket。
//
// 不需要登录：文件仓库是「局域网共享盘」，分享出去一个链接对方就能取，
// 这才是它存在的意义。真正的闸门在写操作上（上传 / 删除）。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, http.StatusBadRequest, "非法的文件 ID")
		return
	}

	f, err := s.store.FileByID(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件不存在")
			return
		}
		s.log.Error("查询文件失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件失败")
		return
	}

	// 过期的临时文件直接拒绝，不必等清理协程。
	if f.Kind == storage.KindTemporary && f.ExpiresAt != nil && nowTime().After(*f.ExpiresAt) {
		httpx.Fail(w, http.StatusGone, "该临时文件已过期")
		return
	}

	fh, err := s.files.Open(string(f.Kind), f.StoredName)
	if err != nil {
		s.log.Error("打开文件失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusNotFound, "文件已不存在")
		return
	}
	defer fh.Close()

	// Content-Disposition 用 filename*=UTF-8'' 形式，保证中文名正确显示。
	// attachment 而非 inline：避免浏览器把上传的 html/svg 当成页面执行。
	w.Header().Set("Content-Disposition", contentDisposition(f.OriginalName))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// http.ServeContent 自动支持 Range 请求，顺便就拿到了基础断点续传能力。
	http.ServeContent(w, r, f.OriginalName, f.CreatedAt, fh)
}

// contentDisposition 构造兼容各浏览器的下载头。
func contentDisposition(name string) string {
	// ASCII 回退名：非 ASCII 字符替换为 _，供老浏览器使用。
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)

	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		ascii, url.PathEscape(name))
}

// ---------------------------------------------------------------- 删除

// handleDelete 删除一个文件（记录 + 磁盘）。
//
// 权限规则（下载开放、删除收紧 —— 见 handleDownload 的注释）：
//   - 永久文件：**必须登录**；登录者还得是上传者本人，或管理员。
//   - 临时文件：无需登录即可删（凭会话码取用，谁都不能长期占着）。
//
// 永久文件这里绝不能放开登录校验：未登录时就无法确定「你是谁」，
// 匿名删除 = 任何人凭一个列表里的 id 就能抹掉全仓库的文件。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, http.StatusBadRequest, "非法的文件 ID")
		return
	}

	f, err := s.store.FileByID(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件不存在")
			return
		}
		httpx.Fail(w, http.StatusInternalServerError, "读取文件失败")
		return
	}

	// 权限先判，再动数据 —— 别让越权请求走到「删记录」那一步。
	var user *storage.User
	if f.Kind == storage.KindPermanent {
		user = httpx.CurrentUser(r, s.store)
		if user == nil {
			httpx.Fail(w, http.StatusUnauthorized, "请先登录后再删除文件")
			return
		}
		// 普通用户只能删自己的；管理员放行。
		if !user.IsAdmin() && !ownsFile(f, user) {
			httpx.Fail(w, http.StatusForbidden, "只能删除自己上传的文件")
			return
		}
	} else {
		user = httpx.CurrentUser(r, s.store)
	}

	// 删除顺序：先删记录，再按引用计数决定要不要动磁盘。
	//
	// 与清理协程的「先删磁盘再删记录」刻意相反，因为这里可能遇到共享文件：
	// 去重后多条记录指向同一个 stored_name，只有当前上传者的同内容记录
	// 全部删完（计数归零）才能 unlink，否则会把别人还在用的文件删掉。
	//
	// 先删记录还有个附带好处：权限一撤销文件立刻不可见，语义更干净。
	if err := s.store.DeleteFileRecord(id); err != nil {
		s.log.Error("删除文件记录失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusInternalServerError, "删除文件记录失败")
		return
	}

	if shouldKeepOnDisk(f) {
		// 同一个上传者的同内容文件共用一份磁盘数据，还有引用就不能删。
		n, cntErr := s.store.CountFilesBySHA256(f.SHA256, f.Kind, *f.OwnerID)
		if cntErr != nil {
			// 计数失败时保守处理：不动磁盘。最坏留一个无主文件，
			// 下次有人传同样内容会被复用，比误删别人的文件好得多。
			s.log.Error("统计同内容文件失败: id=%d err=%v", id, cntErr)
		}
		if n > 0 {
			s.log.Info("文件删除（同内容仍有 %d 条记录引用，保留磁盘文件）: id=%d name=%q user=%s ip=%s",
				n, id, f.OriginalName, userOrAnon(user), httpx.ClientIP(r))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
			return
		}
	}

	if err := s.files.Remove(string(f.Kind), f.StoredName); err != nil {
		s.log.Error("删除文件失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusInternalServerError, "删除文件失败")
		return
	}

	s.log.Info("文件删除: id=%d name=%q user=%s ip=%s",
		id, f.OriginalName, userOrAnon(user), httpx.ClientIP(r))

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// shouldKeepOnDisk 判断删除该记录时是否需要先做引用计数。
//
// 只有「永久文件 + 有哈希 + 有主」的记录才可能与他人共享磁盘数据：
//   - 非永久文件（temporary / chat）从不去重，一记录一文件，直接删即可；
//   - 空哈希（老数据）不可能匹配到任何人，直接删；
//   - 无主记录（owner_id 为 NULL）不会被人复用 —— 去重查询要求 owner 相等，
//     NULL 不参与等值比较，所以它也是独占的。
func shouldKeepOnDisk(f *storage.File) bool {
	return f.Kind == storage.KindPermanent && f.SHA256 != "" && f.OwnerID != nil
}

// ---------------------------------------------------------------- 工具

// ownsFile 判断某用户是否为该文件的上传者。
func ownsFile(f *storage.File, u *storage.User) bool {
	if f == nil || u == nil || f.OwnerID == nil {
		return false
	}
	return *f.OwnerID == u.ID
}

func userOrAnon(u *storage.User) string {
	if u == nil {
		return "匿名设备"
	}
	return u.Username
}
