package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/session"
	"lanshare/internal/storage"
)

// chatFileResp 是聊天室文件的元数据。
//
// 与文件仓库的 fileResp 分开：聊天文件没有 owner / kind 这些概念，
// 前端拿到的就是「一个能点开下载的名字和地址」。
type chatFileResp struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SizeText    string `json:"sizeText"`
	DownloadURL string `json:"downloadUrl"`
}

// handleChatUpload 接收聊天室文件上传。
//
// 路径：POST /api/sessions/{code}/files
//
// 为什么不要求登录：实时传输区的定位就是「不方便登录聊天软件时随手传点东西」。
// 房间号本身就是凭据 —— 只有拿到它的人才能进这个房间，也才能往里面传文件。
// 这与「永久文件仓库必须登录」形成了清晰的分界：
//
//	左边（实时传输）= 凭房间号，用完即毁；右边（文件仓库）= 凭账号，长期保存。
//
// 表单字段：file（必填）、name（可选，覆盖文件名）。
func (s *Server) handleChatUpload(w http.ResponseWriter, r *http.Request) {
	code := session.NormalizeCode(r.PathValue("code"))
	if !codePattern.MatchString(code) {
		httpx.Fail(w, http.StatusBadRequest, "房间号格式不正确")
		return
	}

	// 房间必须存在且未过期 —— 否则文件会挂在一个永远不会被回收的房间号下，
	// 变成没人能认领、也没人会清理的孤儿数据。
	if s.sessions.ExistsButExpired(code) {
		httpx.Fail(w, http.StatusGone, "房间已过期，文件无法上传")
		return
	}
	if s.sessions.Get(code) == nil {
		httpx.Fail(w, http.StatusNotFound, "房间不存在，请先创建或加入房间")
		return
	}

	if err := s.files.HasRoomForUpload(r.ContentLength); err != nil {
		if errors.Is(err, files.ErrNoSpace) {
			s.log.Error("聊天文件上传前磁盘空间检查未通过: %v", err)
			httpx.Fail(w, http.StatusInsufficientStorage, "磁盘空间不足，无法发送文件")
			return
		}
	}

	// 与文件仓库共用一个上传名额池：两条链路抢的是同一块磁盘和 CPU，
	// 分开限流等于没限。
	if err := s.acquireUploadSlot(r.Context()); err != nil {
		return
	}
	defer s.releaseUploadSlot()

	// 与文件仓库走同一套流式解析：聊天室也照样会传几百 MB 的安装包，
	// 只改 /api/files 而放过这里的话，OOM 和二次拷贝会原样留在这条链路上。
	up, err := openUploadStream(w, r, "file", s.chatMaxUpload)
	if err != nil {
		switch {
		case errors.Is(err, ErrUploadTooLarge):
			httpx.Fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件超过上限 %s", files.HumanSize(s.chatMaxUpload)))
		case errors.Is(err, ErrNoFilePart):
			httpx.Fail(w, http.StatusBadRequest, "没有收到文件")
		default:
			httpx.Fail(w, http.StatusBadRequest, "解析上传内容失败")
		}
		return
	}
	defer up.Close()

	displayName := files.SafeDisplayName(up.Field("name"))
	if displayName == "file" {
		displayName = files.SafeDisplayName(up.Filename)
	}

	start := time.Now()
	// 显式传入聊天室自己的上限 —— 不再借用文件仓库的 Service 级上限，
	// 否则 LANSHARE_CHAT_UPLOAD_MB 会被 LANSHARE_MAX_UPLOAD_MB 二次截断。
	res, err := s.files.Save(string(storage.KindChat), up.Body, s.chatMaxUpload)
	elapsed := time.Since(start)
	if err != nil {
		if errors.Is(err, files.ErrTooLarge) || isRequestBodyTooLarge(err) {
			httpx.Fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件超过上限 %s", files.HumanSize(s.chatMaxUpload)))
			return
		}
		s.log.Error("聊天文件上传失败: room=%s name=%q duration=%s err=%v",
			code, displayName, durationText(elapsed), err)
		httpx.Fail(w, http.StatusInternalServerError, "保存文件失败")
		return
	}

	rec := &storage.File{
		OriginalName: displayName,
		StoredName:   res.StoredName,
		Size:         res.Size,
		SHA256:       res.SHA256,
		Kind:         storage.KindChat,
		RoomCode:     code,
		OwnerName:    "匿名设备",
		CreatedAt:    nowTime(),
	}
	// 登录用户上传就记个名，方便日志追溯（不强制）。
	if u := httpx.CurrentUser(r, s.store); u != nil {
		id := u.ID
		rec.OwnerID = &id
		rec.OwnerName = u.Username
	}

	id, err := s.store.CreateFile(rec)
	if err != nil {
		// 写库失败必须把刚落盘的文件删掉，否则留下无引用的垃圾。
		_ = s.files.Remove(string(storage.KindChat), res.StoredName)
		s.log.Error("写入聊天文件记录失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "保存文件记录失败")
		return
	}
	rec.ID = id

	s.log.Info("聊天文件上传: id=%d room=%s name=%q size=%d duration=%s speed=%s user=%s ip=%s",
		id, code, displayName, res.Size, durationText(elapsed), speedText(res.Size, elapsed),
		rec.OwnerName, httpx.ClientIP(r))

	httpx.WriteJSON(w, http.StatusOK, chatFileResp{
		ID:          id,
		Name:        displayName,
		Size:        res.Size,
		SizeText:    files.HumanSize(res.Size),
		DownloadURL: fmt.Sprintf("/api/chat-files/%d", id),
	})
}

// handleChatDownload 下载一个聊天室文件。
//
// 路径：GET /api/chat-files/{id}
//
// 不需要登录，也不校验房间号 —— 能拿到这个 URL 本身就说明他当时在房间里
// （URL 是 WebSocket 广播出去的）。反过来若要求带房间号，会让「别人转发过来的
// 下载链接」失效，而这恰恰是局域网传文件最常见的用法。
func (s *Server) handleChatDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, http.StatusBadRequest, "非法的文件 ID")
		return
	}

	f, err := s.store.FileByID(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件不存在或已被清理")
			return
		}
		s.log.Error("查询聊天文件失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件失败")
		return
	}

	// 只允许通过这个入口取聊天文件，避免被拿来下载仓库文件绕过登录。
	if f.Kind != storage.KindChat {
		httpx.Fail(w, http.StatusNotFound, "文件不存在")
		return
	}

	// 房间已经没了，文件就该视为不存在 —— 即便清理协程还没来得及删。
	// 这样「房间过期 = 文件消失」在语义上是立即成立的。
	if s.sessions.Get(f.RoomCode) == nil {
		httpx.Fail(w, http.StatusGone, "房间已结束，该文件已被清理")
		return
	}

	fh, err := s.files.Open(string(f.Kind), f.StoredName)
	if err != nil {
		s.log.Error("打开聊天文件失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusNotFound, "文件已不存在")
		return
	}
	defer fh.Close()

	w.Header().Set("Content-Disposition", contentDisposition(f.OriginalName))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// ServeContent 自带 Range 支持，顺带获得基础断点续传。
	http.ServeContent(w, r, f.OriginalName, f.CreatedAt, fh)
}

// purgeRoomFiles 删除某个房间名下的全部聊天文件（记录 + 磁盘）。
//
// 注册为 session.Manager 的房间销毁回调。设计上刻意「尽力而为」：
// 单个文件删失败只记日志继续删下一个，绝不因为一个文件卡住而留下其余垃圾；
// 漏掉的由 cleanup 协程的孤儿清理兜底，最终一致。
func (s *Server) purgeRoomFiles(code string) {
	list, err := s.store.FilesByRoom(code)
	if err != nil {
		s.log.Error("查询房间 %s 的文件失败: %v", code, err)
		return
	}
	if len(list) == 0 {
		return
	}

	var freed int64
	for _, f := range list {
		if err := s.files.Remove(string(f.Kind), f.StoredName); err != nil {
			s.log.Error("删除房间文件 %s 失败: %v", f.StoredName, err)
			continue
		}
		if err := s.store.DeleteFileRecord(f.ID); err != nil {
			s.log.Error("删除房间文件记录 %d 失败: %v", f.ID, err)
			continue
		}
		freed += f.Size
	}
	s.log.Info("房间 %s 销毁，已清理聊天文件 %d 个（%s）",
		code, len(list), files.HumanSize(freed))
}
