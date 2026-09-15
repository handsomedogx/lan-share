package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/session"
	"lanshare/internal/storage"
	"lanshare/internal/websocket"
)

// humanSize 是 files.HumanSize 的短别名，让消息构造更紧凑。
func humanSize(n int64) string { return files.HumanSize(n) }

// codePattern 限定授权码形态：4~8 位大写字母数字。
// 与生成端保持一致的宽松度，便于将来扩展长度。
var codePattern = regexp.MustCompile(`^[A-Z0-9]{4,8}$`)

// urlPattern 用于识别消息内容是否为链接。
// 只认 http/https，避免把 file:// 之类的危险 scheme 渲染成可点击链接。
var urlPattern = regexp.MustCompile(`(?i)^https?://[^\s<>"]+$`)

// ---------------------------------------------------------------- 创建会话

// createSessionReq 是创建房间的请求体。
type createSessionReq struct {
	// Code 是自定义房间号，留空则服务端自动生成。
	Code string `json:"code"`
	// TTLMinutes 是存活时长（分钟）。留空则用 DefaultTTLMinutes。
	//
	// 必须落在 session.AllowedTTLMinutes 白名单内，否则整个请求被拒 ——
	// 白名单里已经**没有 0**（曾表示「不限时」），所以房间必定有确定的终点。
	TTLMinutes *int `json:"ttlMinutes"`
}

type createSessionResp struct {
	Code      string `json:"code"`
	Online    int    `json:"online"`
	CreatedAt int64  `json:"createdAt"`
	// ExpiresAt 是自动销毁时刻（Unix 毫秒）。
	ExpiresAt int64 `json:"expiresAt"`
	// TTLMinutes 回显总时长（分钟）。
	TTLMinutes int `json:"ttlMinutes"`
}

// handleCreateSession 创建一个新的实时会话。
//
// 无需登录：实时传输区的定位就是「不方便登录聊天软件时快速发个链接」，
// 加登录反而违背初衷。真正的门槛是那个房间号 —— 只有拿到的人才能加入。
//
// 请求体可选：
//
//	{"code":"K7MF","ttlMinutes":60}   自定义房间号与时长
//	{}                                 全用默认（随机码 + 1 小时）
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	// 允许空 body：老客户端不带请求体也能创建。
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.Fail(w, http.StatusBadRequest, "请求格式错误")
			return
		}
	}

	ttl := session.DefaultTTLMinutes
	if req.TTLMinutes != nil {
		ttl = *req.TTLMinutes
	}
	if !session.ValidTTL(ttl) {
		httpx.Fail(w, http.StatusBadRequest, "不支持的存活时长")
		return
	}

	code := session.NormalizeCode(req.Code)
	room, err := s.sessions.CreateWithCode(code, ttl)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrCodeTaken):
			httpx.Fail(w, http.StatusConflict, "该房间号已被占用，换一个吧")
		case errors.Is(err, session.ErrBadCode):
			httpx.Fail(w, http.StatusBadRequest, session.ErrBadCode.Error())
		default:
			s.log.Error("创建会话失败: %v", err)
			httpx.Fail(w, http.StatusInternalServerError, "创建会话失败")
		}
		return
	}

	s.log.Info("创建实时会话: %s (ttl=%dm ip=%s)", room.Code, ttl, httpx.ClientIP(r))
	httpx.WriteJSON(w, http.StatusOK, createSessionResp{
		Code:       room.Code,
		Online:     room.Online(),
		CreatedAt:  room.CreatedAt().UnixMilli(),
		ExpiresAt:  expiresAtMillis(room),
		TTLMinutes: room.TTLMinutes(),
	})
}

// expiresAtMillis 把房间过期时刻转成 Unix 毫秒；没有到期时间时返回 0。
func expiresAtMillis(room *session.Room) int64 {
	if room.ExpiresAt().IsZero() {
		return 0
	}
	return room.ExpiresAt().UnixMilli()
}

// ---------------------------------------------------------------- 会话信息

type sessionInfoResp struct {
	Code    string   `json:"code"`
	Online  int      `json:"online"`
	Members []string `json:"members"`
	// ExpiresAt 是自动销毁时刻（Unix 毫秒）。
	ExpiresAt int64 `json:"expiresAt"`
	// RemainingSeconds 是剩余秒数；-1 表示没有到期时间（兜底场景，正常不会出现）。
	RemainingSeconds int `json:"remainingSeconds"`
}

// handleSessionInfo 查询会话是否存在与在线情况，
// 供客户端在建立 WebSocket 之前先做一次友好校验。
func (s *Server) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	code := session.NormalizeCode(r.PathValue("code"))
	if !codePattern.MatchString(code) {
		httpx.Fail(w, http.StatusBadRequest, "房间号格式不正确")
		return
	}
	room := s.sessions.Get(code)
	if room == nil {
		httpx.Fail(w, http.StatusNotFound, "会话不存在、已过期或已结束")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sessionInfoResp{
		Code:             room.Code,
		Online:           room.Online(),
		Members:          room.Labels(),
		ExpiresAt:        expiresAtMillis(room),
		RemainingSeconds: room.RemainingSeconds(),
	})
}

// ---------------------------------------------------------------- WebSocket

// wsIn 是客户端发来的消息。
type wsIn struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	// FileID 用于 type=file：客户端先走 HTTP 上传拿到 id，再通过 WS 广播这张卡片。
	//
	// 刻意只收 id 而不是完整的文件信息（名字/大小/URL）——
	// 后者全部由服务端根据 id 查库填充，客户端无法伪造「这条卡片指向别的文件」。
	FileID int64 `json:"fileId,omitempty"`
	// Cid 是发送方生成的临时标识，服务端只负责原样回传，不作任何校验。
	// 前端靠它认出「这条广播就是我刚发的那条」，从而去掉本地乐观渲染的那条。
	// 用 cid 而不是「昵称 + 内容」做指纹，是因为昵称可能为空、内容可能重复，
	// 两者都会让去重判断失效（同名同内容的两次发送也会互相吃掉）。
	Cid string `json:"cid,omitempty"`
}

// wsOut 是服务端下发的消息信封。
//
// 所有下行都走同一个信封（而不是裸发消息体），
// 这样前端可以用一个 switch 处理全部事件，且方便以后加新事件类型。
type wsOut struct {
	Event   string            `json:"event"` // hello | message | join | leave | error
	Code    string            `json:"code,omitempty"`
	Message *session.Message  `json:"message,omitempty"`
	Online  int               `json:"online,omitempty"`
	Members []string          `json:"members,omitempty"`
	History []session.Message `json:"history,omitempty"`
	// Self 是服务端给**这条连接**分配的显示名。
	// 必须下发，否则前端无从知道自己叫什么 —— 昵称留空时服务端会用 IP 兜底，
	// 前端若还按空昵称去匹配 sender，就会把自己的消息当成别人的，导致重复渲染。
	Self string `json:"self,omitempty"`
	// ExpiresAt / RemainingSeconds 描述房间存活情况，前端据此显示倒计时。
	// RemainingSeconds 为 -1 表示没有到期时间（兜底场景）；只在 hello 里下发。
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	RemainingSeconds int    `json:"remainingSeconds,omitempty"`
	Error            string `json:"error,omitempty"`
}

// wsClient 是 session.Client 的实现，桥接 Conn 与房间广播。
type wsClient struct {
	conn  *websocket.Conn
	label string
	code  string
	// ch 是该连接的发送队列。所有下发都先入队再写出，
	// 保证「写」永远只有一个 goroutine，避免并发写坏帧。
	ch     chan []byte
	closed chan struct{}
	once   bool
}

// Send 实现 session.Client：把消息投入发送队列。
//
// 队列满说明该客户端消费不过来（网络慢或卡住），
// 此时丢弃这条消息而不是阻塞广播 —— 实时传输区宁可丢一条也不能拖垮全房间。
func (c *wsClient) Send(b []byte) {
	select {
	case c.ch <- b:
	case <-c.closed:
	default:
	}
}

// Label 实现 session.Client。
func (c *wsClient) Label() string { return c.label }

// handleWS 处理 GET /ws/session/{code}。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	code := session.NormalizeCode(r.PathValue("code"))
	if !codePattern.MatchString(code) {
		httpx.Fail(w, http.StatusBadRequest, "房间号格式不正确")
		return
	}

	// 允许「带码直达」：房间不存在时自动创建（沿用默认时长）。
	// 理由：用户 A 创建会话拿到码，用户 B 输入码时房间必然已存在；
	// 但如果 A 创建后自己先关掉页面，B 再输入就会失败，体验很差。
	// 自动建房间让房间号本身成为唯一凭据，更符合直觉。
	//
	// 注意：Get 会把「已过期」视为不存在，所以这里要能区分
	// 「从没创建过」（可自动建）和「已过期」（必须拒绝）——
	// 否则过期房间会被下一个人无限续命，存活时长就形同虚设。
	if s.sessions.ExistsButExpired(code) {
		httpx.Fail(w, http.StatusGone, "会话已过期")
		return
	}
	room := s.sessions.Get(code)
	if room == nil {
		var err error
		room, err = s.sessions.CreateWithCode(code, session.DefaultTTLMinutes)
		if err != nil {
			s.log.Error("自动创建会话失败 %s: %v", code, err)
			httpx.Fail(w, http.StatusInternalServerError, "无法建立会话")
			return
		}
	}

	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		// 握手失败通常是客户端不是合法 WebSocket，属于偶发，不写日志刷屏。
		return
	}

	// 显示名优先取 ?name=，留空则用客户端 IP 兜底。
	//
	// 当前网页端已不再提供昵称输入框（显示名统一由服务端分配），
	// 但查询参数保留：它是 URL 契约的一部分，脚本或其他客户端仍可直接指定。
	label := r.URL.Query().Get("name")
	if label == "" {
		label = defaultLabel(conn.RemoteAddr())
	}
	label = sanitizeLabel(label)

	client := &wsClient{
		conn:   conn,
		label:  label,
		code:   code,
		ch:     make(chan []byte, 64),
		closed: make(chan struct{}),
	}

	room.Join(client)

	// 下发 hello：带上在线名单与历史消息（刷新页面后能看到刚才的内容）。
	history := room.History()
	// 只回传最近 100 条，避免刚进房间就渲染上千条 DOM。
	if len(history) > 100 {
		history = history[len(history)-100:]
	}
	s.sendTo(client, wsOut{
		Event:            "hello",
		Code:             code,
		Self:             label,
		Online:           room.Online(),
		Members:          room.Labels(),
		History:          history,
		ExpiresAt:        expiresAtMillis(room),
		RemainingSeconds: room.RemainingSeconds(),
	})

	// 通知其他人有人加入。
	s.broadcastRoom(room, client, wsOut{
		Event:   "join",
		Code:    code,
		Online:  room.Online(),
		Members: room.Labels(),
	})

	s.log.Info("WebSocket 加入会话 %s: %s (在线 %d)", code, label, room.Online())

	done := make(chan struct{})
	go conn.PingLoop(done)

	// 写循环：独立 goroutine，串行消费发送队列。
	go func() {
		for {
			select {
			case <-client.closed:
				return
			case b := <-client.ch:
				if err := conn.WriteText(b); err != nil {
					return
				}
			}
		}
	}()

	// 读循环：阻塞在当前 goroutine。
	defer func() {
		// 关闭顺序很讲究，错了会丢事件：
		//
		//   1) 先停 ping 协程、关掉本连接 —— 断开是既成事实；
		//   2) 把这名成员移出房间；
		//   3) 再广播 leave，内容取「移除之后」的在线名单 ——
		//      收到这条的人看到的就该是当前真实状态，而不是包含
		//      一个已经断开的人。广播时显式 except 掉自己。
		//   4) 最后才关 client.closed，作为「发送队列作废」的信号。
		//      若提前关，其他协程可能在广播途中向已作废的队列投递。
		close(done)
		_ = conn.Close()

		room.Leave(client)

		payload, _ := json.Marshal(wsOut{
			Event:   "leave",
			Code:    code,
			Online:  room.Online(),
			Members: room.Labels(),
		})
		room.Broadcast(payload, client)
		close(client.closed)

		s.log.Info("WebSocket 离开会话 %s: %s (在线 %d)", code, label, room.Online())
	}()

	for {
		op, data, err := conn.ReadMessage()
		if err != nil {
			if !isNormalClose(err) {
				s.log.Info("WebSocket 连接结束 %s: %v", label, err)
			}
			return
		}
		// 第一版只处理文本帧；二进制帧直接忽略（文件不走 WebSocket）。
		if op != 0x1 {
			continue
		}
		s.handleClientMessage(room, client, data)
	}
}

// handleClientMessage 校验并广播一条客户端消息。
func (s *Server) handleClientMessage(room *session.Room, client *wsClient, data []byte) {
	var in wsIn
	if err := json.Unmarshal(data, &in); err != nil {
		s.sendTo(client, wsOut{Event: "error", Error: "消息格式错误"})
		return
	}

	// 文件卡片走独立分支：它的合法性来自「这个文件确实属于本房间」，
	// 而不是来自文本长度或 URL 形态。
	if in.Type == "file" {
		s.handleFileMessage(room, client, in)
		return
	}

	content := strings.TrimSpace(in.Content)
	if content == "" {
		return
	}
	// 单条消息限长：实时传输区不该用来传大段文本。
	if len([]rune(content)) > 4000 {
		s.sendTo(client, wsOut{Event: "error", Error: "消息过长（上限 4000 字）"})
		return
	}

	// 自动识别 URL：类型由服务端决定，保证所有客户端看到一致的类型。
	// 同时兼容「链接 + 说明文字」这种一行多段的常见粘贴场景。
	msgType := strings.TrimSpace(in.Type)
	if msgType != "link" {
		msgType = "text"
	}
	if urlPattern.MatchString(content) {
		msgType = "link"
	}

	msg := session.Message{
		ID:      newMsgID(),
		Type:    msgType,
		Content: content,
		Sender:  client.label,
		SentAt:  time.Now().UnixMilli(),
		Cid:     in.Cid,
	}
	// 只把不带 cid 的副本写进历史：历史回放不需要去重语义，
	// 而且别人的 cid 对后来者毫无意义。
	hist := msg
	hist.Cid = ""
	room.Append(hist)

	// 广播给所有人（含发送者自己）。包含发送者可让前端统一以「服务端确认」
	// 为准渲染消息，避免本地先插入再被覆盖造成的顺序错乱。
	s.broadcastRoom(room, nil, wsOut{Event: "message", Code: room.Code, Message: &msg})
}

// ---------------------------------------------------------------- 工具

// handleFileMessage 广播一张文件卡片。
//
// 关键校验：该文件必须是 Kind=chat **且** 属于当前房间。
// 只看 id 存在是不够的 —— 否则任何人拿一个别的房间（甚至文件仓库）的 id
// 就能把别人的文件卡片广播到自己房间里。
func (s *Server) handleFileMessage(room *session.Room, client *wsClient, in wsIn) {
	if in.FileID <= 0 {
		s.sendTo(client, wsOut{Event: "error", Error: "文件标识无效"})
		return
	}

	f, err := s.store.FileByID(in.FileID)
	if err != nil {
		s.sendTo(client, wsOut{Event: "error", Error: "文件不存在或已被清理"})
		return
	}
	if f.Kind != storage.KindChat || f.RoomCode != room.Code {
		// 不泄漏「文件存在但不属于你」这种信息，统一说成不存在。
		s.sendTo(client, wsOut{Event: "error", Error: "文件不存在或已被清理"})
		return
	}

	msg := session.Message{
		ID:       newMsgID(),
		Type:     "file",
		Content:  f.OriginalName,
		Sender:   client.label,
		SentAt:   time.Now().UnixMilli(),
		FileName: f.OriginalName,
		FileSize: f.Size,
		FileText: humanSize(f.Size),
		FileURL:  fmt.Sprintf("/api/chat-files/%d", f.ID),
		Cid:      in.Cid,
		// 图片标记在这里判定、随卡片一起广播 ——
		// 前端拿到 isImage 就能直接把卡片渲染成缩略图，
		// 不必再发一次请求去问「这文件是不是图片」。
		IsImage: files.IsImageName(f.OriginalName),
	}
	hist := msg
	hist.Cid = ""
	room.Append(hist)

	s.broadcastRoom(room, nil, wsOut{Event: "message", Code: room.Code, Message: &msg})
}

// sendTo 只发给单个客户端。
func (s *Server) sendTo(c *wsClient, out wsOut) {
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	c.Send(b)
}

// broadcastRoom 广播给房间内所有连接，except 非空时跳过它。
func (s *Server) broadcastRoom(room *session.Room, except *wsClient, out wsOut) {
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	var skip session.Client
	if except != nil {
		skip = except
	}
	room.Broadcast(b, skip)
}

// newMsgID 生成消息 ID。用「时间戳 + 进程内递增序号」即可，
// 无需全局唯一：它只在单个房间的展示与前端去重里用到。
//
// 计数器必须用 atomic：这个函数会被**并发的** HTTP / WebSocket handler
// 调用（多个房间、多个连接同时发消息），裸的 msgSeq++ 是 data race ——
// `go test -race` 会直接报出来，高并发下也可能产生重复 ID。
var msgSeq atomic.Uint64

func newMsgID() string {
	seq := msgSeq.Add(1)
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + strconv.FormatUint(seq, 36)
}

// defaultLabel 在用户没有指定昵称时生成一个稳定的默认名。
func defaultLabel(remoteAddr string) string {
	ip := remoteAddr
	for i := len(ip) - 1; i >= 0; i-- {
		if ip[i] == ':' {
			ip = ip[:i]
			break
		}
	}
	if ip == "" {
		return "匿名设备"
	}
	return ip
}

// sanitizeLabel 清洗昵称，去掉控制字符并限长。
func sanitizeLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == '"' || r == '<' || r == '>' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "匿名设备"
	}
	r := []rune(s)
	if len(r) > 24 {
		s = string(r[:24])
	}
	return s
}

// isNormalClose 判断是否为正常的连接结束，正常结束不记日志（文档第 18 节）。
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer")
}
