// Package session 管理两类「会话」，注意区分：
//
//  1. LoginSession —— 浏览器登录态（Cookie + 服务端 sessions 表）。
//  2. Room         —— 实时传输区的授权码会话，只存在于内存。
//
// 授权码会话不落库是刻意的设计：文档要求实时消息「只存在内存」，
// 服务器重启或会话结束后即消失，符合「临时传输」的定位，
// 同时避免路由器 Overlay / 数据分区被高频写入。
package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// 授权码字符集：去掉了容易混淆的 0/O/1/I/L，方便口头或手抄传递。
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// 授权码长度约束。默认 4 位在局域网内已足够且便于输入；
// 允许自定义时放宽到 8 位，方便用有意义的名字（如 "K7MF"、"OFFICE"）。
const (
	MinCodeLength = 4
	MaxCodeLength = 8
)

// 房间存活时长的可选档位（分钟）。
//
// 用白名单而不是接受任意数值：前端只需给几个直观选项，
// 服务端也能拒绝掉离谱的值（比如负数或一亿分钟）。
//
// 这里刻意**不含 0**。0 曾被用作「不限时」，但一个永远不会自动消失的房间
// 既违背「临时传输、用完即毁」的产品定位，也会让聊天文件的回收失去触发点
// （文件是随房间销毁而删的）。房间必须有一个确定的终点，
// 因此 0 不再是合法档位 —— 传 0 会被 ValidTTL 拒绝。
var AllowedTTLMinutes = []int{10, 60, 360, 1440}

// DefaultTTLMinutes 是创建房间时的默认存活时长（小时级，够一次传文件）。
const DefaultTTLMinutes = 60

// codeLength 是自动生成授权码时的长度。
const codeLength = 4

// maxMessagesPerRoom 限制单个会话保留的消息条数，
// 防止长期不重启导致内存缓慢增长。
const maxMessagesPerRoom = 300

// roomIdleTimeout：房间所有人离线超过该时长即回收。
//
// 保留是为了兜住「expiresAt 为零值」的房间。现在 API 已经不再接受 0
// （见 AllowedTTLMinutes），所以正常路径上不会有这种房间；但只要
// Room 的零值仍代表「没有到期时间」，回收逻辑就必须对它有明确行为，
// 否则一旦哪天有代码路径漏设 expiresAt，房间和它的聊天文件就永远留着。
const roomIdleTimeout = 2 * time.Hour

// Message 是一条实时消息。
//
// 支持三种类型：
//
//	text  普通文本
//	link  链接（服务端识别 URL 后决定，保证各客户端类型一致）
//	file  文件卡片（文件本体走 HTTP，这里只带元数据与下载地址）
type Message struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // text | link | file
	Content string `json:"content"`
	Sender  string `json:"sender"`
	SentAt  int64  `json:"sentAt"` // Unix 毫秒
	// 以下四个字段仅 type=file 时有值。
	// 文件本体不经过 WebSocket（大文件会把内存和带宽都吃掉），
	// 这里只传「叫什么、多大、去哪儿下」。
	FileName string `json:"fileName,omitempty"`
	FileSize int64  `json:"fileSize,omitempty"`
	FileText string `json:"fileText,omitempty"`
	FileURL  string `json:"fileUrl,omitempty"`
	// Cid 原样回传发送方给的临时标识，只用于「这条广播是谁发的」的确认。
	// 服务端不解释它；历史消息里不带（omitempty 且不写入），
	// 因为历史回放时前端不需要去重。
	Cid string `json:"cid,omitempty"`
}

// Client 是房间里的一个连接。用接口而不是具体类型，
// 避免 session 包反向依赖 websocket 包。
type Client interface {
	// Send 向该连接投递一条消息。
	Send(msg []byte)
	// Label 返回该连接的展示名（用于在线列表）。
	Label() string
}

// Room 是一个授权码会话。
type Room struct {
	Code string

	mu        sync.RWMutex
	clients   map[Client]struct{}
	messages  []Message
	createdAt time.Time
	lastSeen  time.Time
	// expiresAt 是房间的自动销毁时刻。零值表示没有到期时间 ——
	// API 层已不再允许创建这种房间（AllowedTTLMinutes 不含 0），
	// 这里保留零值语义只是为了 reapOnce 的兜底分支有明确行为。
	expiresAt time.Time
}

// ErrCodeTaken 表示请求的自定义房间号已被占用。
var ErrCodeTaken = errors.New("该房间号已被占用")

// ErrBadCode 表示房间号不合法。
var ErrBadCode = errors.New("房间号只能包含字母和数字，长度 4 - 8 位")

// ValidTTL 判断存活时长是否在允许档位内。
func ValidTTL(minutes int) bool {
	for _, m := range AllowedTTLMinutes {
		if m == minutes {
			return true
		}
	}
	return false
}

// NormalizeCode 规范化用户输入的房间号：去空白、统一大写。
//
// 统一大写是为了让「口头传的 k7mf」和「输入的 K7MF」等价 ——
// 房间号本来就是给人念的，大小写敏感只会添麻烦。
func NormalizeCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// ValidateCode 校验房间号格式（不做占用检查）。
func ValidateCode(code string) error {
	n := len(code)
	if n < MinCodeLength || n > MaxCodeLength {
		return ErrBadCode
	}
	for _, r := range code {
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isUpper && !isDigit {
			return ErrBadCode
		}
	}
	return nil
}

// Manager 管理全部房间与在线客户端。
type Manager struct {
	mu    sync.RWMutex
	rooms map[string]*Room

	// onRoomGone 在房间被回收时回调，参数是被销毁的房间号。
	//
	// 用它把「房间消亡」这件事通知给外面，让关联资源（聊天室文件）
	// 一起被删掉 —— 房间到期即自动删除，对文件也必须成立。
	// 用回调而不是让 session 包直接依赖 storage/files，
	// 是为了保持这个包「只管内存里的房间」这一纯粹职责。
	onRoomGone func(code string)
}

// SetRoomGoneHandler 注册房间销毁回调。必须在并发使用前调用。
func (m *Manager) SetRoomGoneHandler(fn func(code string)) {
	m.onRoomGone = fn
}

// NewManager 创建会话管理器，并启动房间回收协程。
func NewManager() *Manager {
	m := &Manager{rooms: make(map[string]*Room)}
	go m.reapLoop()
	return m
}

// GenerateCode 生成一个未被占用的授权码。
func (m *Manager) GenerateCode() string {
	for i := 0; i < 100; i++ {
		code := randomCode()
		m.mu.RLock()
		_, exists := m.rooms[code]
		m.mu.RUnlock()
		if !exists {
			return code
		}
	}
	// 理论上不会走到这里；真到了就用时间戳兜底。
	return fmt.Sprintf("%04X", time.Now().UnixNano()&0xFFFF)
}

func randomCode() string {
	b := make([]byte, codeLength)
	max := big.NewInt(int64(len(codeAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand 在路由器上失败极其罕见，退化为时间取模。
			b[i] = codeAlphabet[time.Now().UnixNano()%int64(len(codeAlphabet))]
			continue
		}
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b)
}

// Get 返回房间，不存在返回 nil。已过期的房间视为不存在。
func (m *Manager) Get(code string) *Room {
	m.mu.RLock()
	r := m.rooms[code]
	m.mu.RUnlock()
	if r != nil && r.Expired() {
		return nil
	}
	return r
}

// ExistsButExpired 判断某个房间号是否「存在但已过期」。
//
// 用于区分「从没创建过」（可以自动建）和「已过期」（必须拒绝）：
// 若不区分，过期房间会被下一个连接无限续命，存活时长就失去意义。
func (m *Manager) ExistsButExpired(code string) bool {
	m.mu.RLock()
	r := m.rooms[code]
	m.mu.RUnlock()
	return r != nil && r.Expired()
}

// Create 生成随机房间号并创建房间。
func (m *Manager) Create(ttlMinutes int) (*Room, error) {
	return m.CreateWithCode("", ttlMinutes)
}

// CreateWithCode 创建一个房间。
//
// code 为空时自动生成；非空时用调用方指定的房间号，
// 若已被占用（且未过期）则返回 ErrCodeTaken。
//
// ttlMinutes 为 0（或非白名单值）时房间**没有**到期时间 ——
// 调用方应先用 ValidTTL 拦掉，避免造出永不回收的房间。
func (m *Manager) CreateWithCode(code string, ttlMinutes int) (*Room, error) {
	if code == "" {
		code = m.GenerateCode()
	} else if err := ValidateCode(code); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if old, ok := m.rooms[code]; ok {
		// 同名房间还在且没过期 —— 真正的冲突。
		if !old.Expired() {
			return nil, ErrCodeTaken
		}
		// 已过期：直接顶掉，让用户能复用刚被回收的名字。
		delete(m.rooms, code)
	}

	now := time.Now()
	r := &Room{
		Code:      code,
		clients:   make(map[Client]struct{}),
		messages:  make([]Message, 0, 16),
		createdAt: now,
		lastSeen:  now,
	}
	if ttlMinutes > 0 {
		r.expiresAt = now.Add(time.Duration(ttlMinutes) * time.Minute)
	}
	m.rooms[code] = r
	return r, nil
}

// Stats 返回房间数与在线人数统计。
func (m *Manager) Stats() (rooms, clients int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rooms = len(m.rooms)
	for _, r := range m.rooms {
		r.mu.RLock()
		clients += len(r.clients)
		r.mu.RUnlock()
	}
	return
}

// reapLoop 定期回收房间：
//   - 设了存活时长的，到点即删（无论有没人）。
//   - 没有到期时间的（零值，正常路径下不该出现），空闲超过
//     roomIdleTimeout 才删 —— 兜底，避免出现永不消失的房间。
func (m *Manager) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		m.reapOnce(time.Now())
	}
}

// reapOnce 跑一轮回收，now 作为判定基准。
//
// 抽成独立方法是为了能在单元测试里「把时间拨快」——
// 真等 10 分钟去验证过期删除是不现实的，而 TTL 的正确性恰恰是本功能的核心。
//
// 回调在**释放锁之后**才触发：删房间要顺带删磁盘文件，
// 那是慢 I/O，握着全局锁做会拖住所有房间的操作。
func (m *Manager) reapOnce(now time.Time) {
	var gone []string

	m.mu.Lock()
	for code, r := range m.rooms {
		r.mu.RLock()
		empty := len(r.clients) == 0
		last := r.lastSeen
		exp := r.expiresAt
		r.mu.RUnlock()

		switch {
		case !exp.IsZero() && now.After(exp):
			delete(m.rooms, code)
			gone = append(gone, code)
		case exp.IsZero() && empty && now.Sub(last) > roomIdleTimeout:
			delete(m.rooms, code)
			gone = append(gone, code)
		}
	}
	m.mu.Unlock()

	for _, code := range gone {
		m.notifyGone(code)
	}
}

// notifyGone 触发房间销毁回调。回调为空时静默跳过。
func (m *Manager) notifyGone(code string) {
	if m.onRoomGone != nil {
		m.onRoomGone(code)
	}
}

// ActiveCodes 返回当前所有活跃房间号。
// 清理协程用它和数据库里的聊天文件做差集，找出无主文件。
func (m *Manager) ActiveCodes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.rooms))
	for code := range m.rooms {
		out = append(out, code)
	}
	return out
}

// ---------------------------------------------------------------- Room

// Join 把一个连接加入房间。
func (r *Room) Join(c Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[c] = struct{}{}
	r.lastSeen = time.Now()
}

// Leave 把一个连接移出房间。
func (r *Room) Leave(c Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, c)
	r.lastSeen = time.Now()
}

// Online 返回在线人数。
func (r *Room) Online() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// Clients 返回当前在线连接的快照。
func (r *Room) Clients() []Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Client, 0, len(r.clients))
	for c := range r.clients {
		out = append(out, c)
	}
	return out
}

// Labels 返回在线用户展示名列表（去重）。
func (r *Room) Labels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{}, len(r.clients))
	out := make([]string, 0, len(r.clients))
	for c := range r.clients {
		l := c.Label()
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out
}

// History 返回房间内的历史消息（旧 → 新）。
// 返回副本，避免调用方在锁外读到被修改的切片。
func (r *Room) History() []Message {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Message, len(r.messages))
	copy(out, r.messages)
	return out
}

// Append 追加一条消息并保留最近 maxMessagesPerRoom 条。
func (r *Room) Append(msg Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, msg)
	if len(r.messages) > maxMessagesPerRoom {
		// 切片整体前移，只保留最近的若干条。
		drop := len(r.messages) - maxMessagesPerRoom
		r.messages = append(r.messages[:0], r.messages[drop:]...)
	}
	r.lastSeen = time.Now()
}

// Broadcast 向房间内所有连接（可排除某个）投递原始字节。
func (r *Room) Broadcast(payload []byte, except Client) {
	r.mu.RLock()
	targets := make([]Client, 0, len(r.clients))
	for c := range r.clients {
		if except != nil && c == except {
			continue
		}
		targets = append(targets, c)
	}
	r.mu.RUnlock()

	for _, c := range targets {
		c.Send(payload)
	}
}

// CreatedAt 返回房间创建时间。
func (r *Room) CreatedAt() time.Time { return r.createdAt }

// ExpiresAt 返回房间的自动销毁时刻。零值表示没有到期时间
// （正常路径不会出现，API 已不接受 0 时长；见 AllowedTTLMinutes）。
func (r *Room) ExpiresAt() time.Time { return r.expiresAt }

// Expired 判断房间是否已过期。没有到期时间的房间永远返回 false。
func (r *Room) Expired() bool {
	if r.expiresAt.IsZero() {
		return false
	}
	return time.Now().After(r.expiresAt)
}

// TTLMinutes 返回房间的总存活时长（分钟）。
//
// 没有到期时间时返回 0 —— 但注意这个 0 表示「无期限」，与
// AllowedTTLMinutes 里被移除的那个 0 不是一回事：后者曾是「用户可选的不限时」，
// 现在是非法输入。这个返回值只在兜底场景出现。
func (r *Room) TTLMinutes() int {
	if r.expiresAt.IsZero() {
		return 0
	}
	return int(r.expiresAt.Sub(r.createdAt).Minutes())
}

// RemainingSeconds 返回剩余存活秒数。
//
// 没有到期时间时返回 -1（区分于「已过期」的 0），前端据此不显示倒计时。
func (r *Room) RemainingSeconds() int {
	if r.expiresAt.IsZero() {
		return -1
	}
	d := time.Until(r.expiresAt)
	if d <= 0 {
		return 0
	}
	return int(d.Seconds())
}
