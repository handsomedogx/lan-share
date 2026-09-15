package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	Pinned      bool   `json:"pinned"`
	DownloadURL string `json:"downloadUrl"`
}

func toFileResp(f *storage.File) fileResp {
	return fileResp{
		ID:          f.ID,
		Name:        f.OriginalName,
		Size:        f.Size,
		SizeText:    files.HumanSize(f.Size),
		SHA256:      f.SHA256,
		Kind:        string(f.Kind),
		Owner:       f.OwnerName,
		CreatedAt:   f.CreatedAt.UnixMilli(),
		Pinned:      f.Pinned,
		DownloadURL: fmt.Sprintf("/api/files/%d", f.ID),
	}
}

// ---------------------------------------------------------------- 列表

// handleListFiles 列出文件仓库内容。
//
// 列表对所有人开放（含未登录）—— 局域网里的取用不必先建账号。
// 写操作（上传、删除、改名、置顶）才需要登录。
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	// 仓库现在只有 permanent 一种类型（temporary 已随按时间过期的
	// 那套逻辑一起移除）。kind 仍从 query 读取，是为了让老客户端
	// 显式传 `kind=permanent` 时不必改代码；传了别的值则明确报错，
	// 而不是静默当成 permanent —— 静默会让调用方以为拿到了想要的数据。
	kind := storage.FileKind(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = storage.KindPermanent
	}
	if kind != storage.KindPermanent {
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
//	kind  permanent（默认，目前也是唯一合法值）
//	name  可选，覆盖原始文件名
//
// 表单字段名保持 "file"，与浏览器 FormData 的常规写法一致。
//
// 注意字段顺序：必须把 kind / name 放在 file 之前。
// 服务端是流式解析，遇到 file part 就立刻开始落盘，
// 之后出现的字段已经读不到了（详见 upload.go 里 uploadStream 的注释）。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	// 磁盘空间先查一次。ContentLength 是含 multipart 封装的总请求体，
	// 比文件本身略大，拿它当「至少要用多少」来判正合适（宁可保守）。
	//
	// 放在拿名额之前：空间不够就没必要排队了，直接拒绝。
	if err := s.files.HasRoomForUpload(r.ContentLength); err != nil {
		if errors.Is(err, files.ErrNoSpace) {
			s.log.Error("上传前磁盘空间检查未通过: %v", err)
			httpx.Fail(w, http.StatusInsufficientStorage, "磁盘空间不足，请先清理文件仓库")
			return
		}
	}

	// 先排队等名额，再开始收数据 —— 被限流的请求不该占用带宽和磁盘。
	if err := s.acquireUploadSlot(r.Context()); err != nil {
		return
	}
	defer s.releaseUploadSlot()

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

	// 仓库现在只有 permanent 一种。kind 仍解析并在非 permanent 时明确报错，
	// 而不是静默纠正 —— 让调用方立刻知道自己的请求没被按预期理解。
	kind := storage.FileKind(up.Field("kind"))
	if kind == "" {
		kind = storage.KindPermanent
	}
	if kind != storage.KindPermanent {
		httpx.Fail(w, http.StatusBadRequest, "未知的文件类型")
		return
	}

	// 仓库文件必须登录 —— 不登录就不知道文件算谁的，归属链会断掉。
	owner := httpx.CurrentUser(r, s.store)
	if owner == nil {
		httpx.Fail(w, http.StatusUnauthorized, "请先登录后再上传到文件仓库")
		return
	}

	displayName := files.SafeDisplayName(up.Field("name"))
	if displayName == "file" {
		displayName = files.SafeDisplayName(up.Filename)
	}

	// 计时从这里开始：名额已经拿到，排队等待不算进「上传速度」里，
	// 否则限流造成的等待会把速度数字拉得毫无参考价值。
	start := time.Now()

	// 到这里才开始真正落盘：网络 → 小缓冲 → 磁盘，只有一次写入。
	// 显式传入仓库自己的上限，与聊天室的上限互不影响。
	res, err := s.files.Save(string(kind), up.Body, s.maxUpload)
	elapsed := time.Since(start)
	if err != nil {
		if errors.Is(err, files.ErrTooLarge) || isRequestBodyTooLarge(err) {
			httpx.Fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件超过上限 %s", files.HumanSize(s.maxUpload)))
			return
		}
		// err 里已带「已接收多少」（见 files.Save），据此能区分
		// 「网络中断」和「磁盘写满」这两类完全不同的故障。
		s.log.Error("上传失败: name=%q duration=%s err=%v", displayName, durationText(elapsed), err)
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
	// 为什么只对仓库文件做（也就是 chat 不做）：聊天文件的删磁盘逻辑是
	// 「记录没了就 unlink」（按房间存亡）。它们之间复用存储名会导致
	// 一方被清、另一方的文件凭空消失。仓库文件是「所有记录都没了才删」，
	// 才是唯一安全的场景。
	stored := res.StoredName
	deduped := false
	var dedupID int64

	{
		// 顺序很关键：**先删刚写下的小副本，再复用旧存储名**。
		//
		// 反过来的话，一旦之后写库失败需要回滚，回滚删掉的是那个**共享**的
		// 旧存储名 —— 会把旧记录指向的文件一起删掉，旧记录变成指向空文件。
		// 按现在的顺序，最坏情况只是白写了一次盘，没有任何记录受损。
		if old, lookErr := s.store.FileBySHA256(res.SHA256, kind, owner.ID); lookErr == nil {
			// 去重命中前必须先确认旧存储名对应的磁盘文件还在。
			//
			// 数据库有记录 ≠ 磁盘有文件：管理员手工删过、文件系统异常、
			// 外部脚本误删、历史 bug 都可能留下「有记录没文件」。
			// 若不检查就复用，会把刚上传成功的好文件删掉，转而引用一个
			// 根本不存在的旧文件，最终产出一条永远下载失败的坏记录 ——
			// 用户看到的是「上传成功但下载 404」。
			if !s.files.Exists(string(kind), old.StoredName) {
				s.log.Error("发现失效的去重记录，保留本次新上传的文件: oldID=%d oldStored=%s sha256=%s",
					old.ID, old.StoredName, res.SHA256)
				// 刻意不在这里删除那条坏记录：上传路径不该静默破坏用户元数据。
				// 旧记录留给以后的「存储一致性检查/修复」功能处理。
			} else {
				if rmErr := s.files.Remove(string(kind), res.StoredName); rmErr != nil {
					s.log.Error("去重时删除重复副本失败: %v", rmErr)
					httpx.Fail(w, http.StatusInternalServerError, "保存文件失败")
					return
				}
				stored = old.StoredName
				deduped = true
				dedupID = old.ID
			}
		} else if !errors.Is(lookErr, storage.ErrNotFound) {
			// 查库异常不该阻塞上传：退化为「不去重」，只是多占一份磁盘。
			s.log.Error("查询重复文件失败: %v", lookErr)
		}
	}

	ownerID := owner.ID
	rec := &storage.File{
		OriginalName: displayName,
		StoredName:   stored,
		Size:         res.Size,
		SHA256:       res.SHA256,
		Kind:         kind,
		OwnerID:      &ownerID,
		OwnerName:    owner.Username,
		CreatedAt:    nowTime(),
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
	// duration + speed 用来回答「到底慢在哪」：千兆局域网理论约 110MB/s，
	// 实测低一个数量级就该去查 Wi-Fi 或磁盘，而不是靠猜。
	if deduped {
		s.log.Info("文件上传（内容重复，复用磁盘文件）: id=%d dupOf=%d name=%q size=%d duration=%s speed=%s user=%s ip=%s",
			id, dedupID, displayName, res.Size, durationText(elapsed), speedText(res.Size, elapsed),
			rec.OwnerName, httpx.ClientIP(r))
	} else {
		s.log.Info("文件上传: id=%d name=%q size=%d kind=%s duration=%s speed=%s sha256=%s user=%s ip=%s",
			id, displayName, res.Size, kind, durationText(elapsed), speedText(res.Size, elapsed),
			res.SHA256, rec.OwnerName, httpx.ClientIP(r))
	}

	httpx.WriteJSON(w, http.StatusOK, toFileResp(rec))
}

// ---------------------------------------------------------------- 下载

// handleDownload 按 ID 下载文件，走普通 HTTP，不使用 WebSocket。
//
// 不需要登录：文件仓库是「局域网共享盘」，分享出去一个链接对方就能取，
// 这才是它存在的意义。真正的闸门在写操作上（上传 / 改名 / 置顶 / 删除）。
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
//   - 聊天文件：无需登录即可删（凭房间码取用，随房间消亡）。
//
// 这里绝不能放开登录校验：未登录时就无法确定「你是谁」，
// 匿名删除 = 任何人凭一个列表里的 id 就能抹掉全仓库的文件。
//
// 校验逻辑走 writableFile —— 和改名、置顶共用同一份规则。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	f, user, ok := s.writableFile(w, r, "删除")
	if !ok {
		return
	}
	// 权限先判，再动数据 —— 别让越权请求走到「删记录」那一步。
	id := f.ID

	// 删除顺序：先删记录，再按引用计数决定要不要动磁盘。
	//
	// 与清理协程的「先删磁盘再删记录」刻意相反，因为这里可能遇到共享文件：
	// 去重后多条记录指向同一个 stored_name，只有引用它的记录**全部**删完
	// （计数归零）才能 unlink，否则会把别人还在用的文件删掉。
	//
	// 先删记录还有个附带好处：权限一撤销文件立刻不可见，语义更干净。
	if err := s.store.DeleteFileRecord(id); err != nil {
		s.log.Error("删除文件记录失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusInternalServerError, "删除文件记录失败")
		return
	}

	if shouldKeepOnDisk(f) {
		// 磁盘上的物理对象是 stored_name，所以这里问的是
		// 「还有没有记录引用这个 stored_name」，而不是按 sha256 绕一圈。
		n, cntErr := s.store.CountFilesByStoredName(f.StoredName)
		if cntErr != nil {
			// 计数失败时**必须**保守处理：绝不能碰磁盘。
			//
			// 注意 n 的零值是 0，所以修复前的写法（只记日志、继续往下走）
			// 实际会误删仍被其它记录引用的文件 —— 与注释里的「保守处理」正好相反。
			//
			// 这里仍返回 200 而不是 500：数据库里的用户记录已经删成功了。
			// 若报 500，用户会以为删除失败，再点一次只会得到「记录不存在」，
			// 实际只是磁盘上可能多留一个无人引用的文件。
			// 对这种场景，优先级是 不误删 > 不残留；孤儿留给维护任务清理。
			s.log.Error("统计磁盘文件引用失败，保守地保留磁盘文件: id=%d stored=%s err=%v",
				id, f.StoredName, cntErr)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
			return
		}
		if n > 0 {
			s.log.Info("文件删除（磁盘文件仍被 %d 条记录引用，保留磁盘文件）: id=%d stored=%s name=%q user=%s ip=%s",
				n, id, f.StoredName, f.OriginalName, userOrAnon(user), httpx.ClientIP(r))
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
// 只有「永久文件」可能与他人共享磁盘数据：
//   - 非永久文件（chat）从不去重，一记录一文件，直接删即可；
//
// 曾经这里还要求「有哈希 + 有主」，那是沿用 sha256 计数口径的残留条件。
// 现在引用计数直接围绕 stored_name 进行，门槛只剩 kind —— 更宽也更准确：
// 只要还有别的记录指向同一个磁盘对象就不删，哪怕那条记录的 sha256 为空
// 或 owner_id 为 NULL（老数据、无主数据同样可能与他人共享存储名）。
func shouldKeepOnDisk(f *storage.File) bool {
	return f.Kind == storage.KindPermanent
}

// ---------------------------------------------------------------- 改名 / 置顶

// renameReq 是改名请求体。
type renameReq struct {
	Name string `json:"name"`
}

// handleRename 修改文件的展示名（前端是双击文件名就地编辑）。
//
// 权限与删除完全一致（登录 + 本人或管理员），复用 writableFile：
// 改名和删除一样，都是「动了别人会看见的东西」，不能因为它是轻量操作
// 就放松 —— 否则任何人都能把共享盘里的文件名改成任意内容。
//
// 为什么是 PATCH 而不是 PUT：这是一次局部更新（只改 name 一个字段）。
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	f, user, ok := s.writableFile(w, r, "重命名")
	if !ok {
		return
	}

	var req renameReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求内容格式不正确")
		return
	}

	// 必须过一遍清洗：名字会被塞进 Content-Disposition 与前端 DOM，
	// 路径分隔符、控制字符、引号都得在入口处干掉。
	// 注意 SafeDisplayName 对空串会回退成 "file" —— 空名不报错，
	// 因为「把名字清空」在语义上等同于「恢复一个兜底的名字」，
	// 而这种请求几乎只可能来自误操作，给个 file 比报错更省事。
	name := files.SafeDisplayName(req.Name)
	if name == f.OriginalName {
		// 没变化就不写库，省一次 WAL 写入（路由器上的闪光卡寿命有限）。
		httpx.WriteJSON(w, http.StatusOK, toFileResp(f))
		return
	}

	updated, err := s.store.RenameFile(f.ID, name)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件不存在")
			return
		}
		s.log.Error("重命名文件失败: id=%d err=%v", f.ID, err)
		httpx.Fail(w, http.StatusInternalServerError, "重命名失败")
		return
	}

	s.log.Info("文件重命名: id=%d %q -> %q user=%s ip=%s",
		f.ID, f.OriginalName, name, userOrAnon(user), httpx.ClientIP(r))

	httpx.WriteJSON(w, http.StatusOK, toFileResp(updated))
}

// pinReq 是置顶请求体。指针类型让「没传」与「传了 false」区分开。
type pinReq struct {
	Pinned *bool `json:"pinned"`
}

// handlePin 切换（或显式设置）文件的置顶状态。
//
// 权限同样与删除一致：置顶是这块共享盘上的全局顺序，
// 让任何人都能改等于把列表头变成公共涂鸦墙。
//
// 只允许仓库文件：置顶的意义是「常用文件不必往下翻」，
// 聊天文件随房间一起消亡，给它置顶没有意义（前端也不展示它们）。
func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	f, user, ok := s.writableFile(w, r, "置顶")
	if !ok {
		return
	}
	if f.Kind != storage.KindPermanent {
		httpx.Fail(w, http.StatusBadRequest, "只有文件仓库中的文件可以置顶")
		return
	}

	var req pinReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求内容格式不正确")
		return
	}
	// 不传 pinned 就当作「切换」，这样前端一个按钮就够，
	// 不用先读当前状态再算目标状态（少一次竞态）。
	target := !f.Pinned
	if req.Pinned != nil {
		target = *req.Pinned
	}

	updated, err := s.store.SetFilePinned(f.ID, target)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件不存在")
			return
		}
		s.log.Error("设置文件置顶失败: id=%d err=%v", f.ID, err)
		httpx.Fail(w, http.StatusInternalServerError, "设置置顶失败")
		return
	}

	s.log.Info("文件置顶变更: id=%d name=%q pinned=%v user=%s ip=%s",
		f.ID, f.OriginalName, target, userOrAnon(user), httpx.ClientIP(r))

	httpx.WriteJSON(w, http.StatusOK, toFileResp(updated))
}

// writableFile 取路径里的文件，并校验当前用户是否有权修改它。
//
// 从 handleDelete 里抽出来，因为「删除 / 改名 / 置顶」三件事的权限规则
// 完全一样。规则只有一份，就不会出现「删除收紧了、改名忘了改」这类漏洞。
//
// 未通过校验时它自己已经写好响应，调用方直接 return 即可。
// 返回的 user 可能是 nil（聊天文件无需登录）。
func (s *Server) writableFile(w http.ResponseWriter, r *http.Request, action string) (*storage.File, *storage.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, http.StatusBadRequest, "非法的文件 ID")
		return nil, nil, false
	}

	f, err := s.store.FileByID(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件不存在")
			return nil, nil, false
		}
		s.log.Error("查询文件失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件失败")
		return nil, nil, false
	}

	var user *storage.User
	if f.Kind == storage.KindPermanent {
		user = httpx.CurrentUser(r, s.store)
		if user == nil {
			httpx.Fail(w, http.StatusUnauthorized, "请先登录后再"+action+"文件")
			return nil, nil, false
		}
		// 普通用户只能动自己的；管理员放行。
		if !user.IsAdmin() && !ownsFile(f, user) {
			httpx.Fail(w, http.StatusForbidden, "只能"+action+"自己上传的文件")
			return nil, nil, false
		}
	} else {
		user = httpx.CurrentUser(r, s.store)
	}
	return f, user, true
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
