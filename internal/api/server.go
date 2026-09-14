// Package api 组装 HTTP 路由与全部接口。
//
// 路由刻意使用标准库 net/http 的 ServeMux（Go 1.22+ 已支持
// "GET /api/files/{id}" 这种带方法与通配的写法），不引入任何 router 依赖。
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lanshare/internal/auth"
	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/logger"
	"lanshare/internal/session"
	"lanshare/internal/storage"
)

// Server 持有各依赖，供全部 handler 使用。
type Server struct {
	store    *storage.Store
	auth     *auth.Service
	files    *files.Service
	sessions *session.Manager
	log      *logger.Logger
	web      http.Handler

	sessionTTL  int
	maxUpload   int64
	tempFileTTL int
	// chatMaxUpload 是聊天室单文件上限，独立于文件仓库的 maxUpload。
	// 聊天是即时投递，前端的 XHR 进度条与房间生命周期都不适合超大文件，
	// 所以给一个更克制的默认值（见 config.ChatMaxUploadBytes）。
	chatMaxUpload int64
}

// Options 是构造参数。
type Options struct {
	Store       *storage.Store
	Auth        *auth.Service
	Files       *files.Service
	Sessions    *session.Manager
	Logger      *logger.Logger
	WebFS       http.Handler
	SessionTTL  int
	MaxUpload   int64
	TempFileTTL int
	// ChatMaxUpload 是聊天室单文件上限，0 表示沿用 MaxUpload。
	ChatMaxUpload int64
}

// NewServer 创建 API 服务。
func NewServer(o Options) *Server {
	s := &Server{
		store:         o.Store,
		auth:          o.Auth,
		files:         o.Files,
		sessions:      o.Sessions,
		log:           o.Logger,
		web:           o.WebFS,
		sessionTTL:    o.SessionTTL,
		maxUpload:     o.MaxUpload,
		tempFileTTL:   o.TempFileTTL,
		chatMaxUpload: o.ChatMaxUpload,
	}
	if s.chatMaxUpload <= 0 {
		s.chatMaxUpload = s.maxUpload
	}
	// 房间一销毁就把它名下的聊天文件删干净，让「房间到期自动删除」对文件也成立。
	s.sessions.SetRoomGoneHandler(s.purgeRoomFiles)
	return s
}

// Routes 返回装配好的 handler。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// ---- 登录态 ----
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)

	// ---- 会话（实时传输区）----
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{code}", s.handleSessionInfo)
	mux.HandleFunc("GET /ws/session/{code}", s.handleWS)
	// 聊天室文件：凭房间号即可上传，不要求登录（实时传输区整体不需要登录）。
	// 文件随房间销毁而删除，与「消息只存内存」的定位一致。
	mux.HandleFunc("POST /api/sessions/{code}/files", s.handleChatUpload)
	mux.HandleFunc("GET /api/chat-files/{id}", s.handleChatDownload)

	// ---- 文件仓库 ----
	mux.HandleFunc("GET /api/files", s.handleListFiles)
	mux.HandleFunc("POST /api/files", s.handleUpload)
	mux.HandleFunc("GET /api/files/{id}", s.handleDownload)
	mux.HandleFunc("DELETE /api/files/{id}", s.handleDelete)

	// ---- 状态 ----
	mux.HandleFunc("GET /api/status", s.handleStatus)

	// ---- 管理员（只有注册开关与角色调整，不做完整后台）----
	mux.HandleFunc("GET /api/admin/users", s.handleAdminUsers)
	mux.HandleFunc("PUT /api/admin/users/{id}/role", s.handleAdminSetRole)
	mux.HandleFunc("GET /api/admin/settings", s.handleAdminSettings)
	mux.HandleFunc("PUT /api/admin/settings", s.handleAdminSettings)

	// ---- 静态页面（嵌入二进制）----
	mux.Handle("/", s.web)

	return s.withMiddleware(mux)
}

// withMiddleware 统一处理 panic 恢复、日志与常用响应头。
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic: %v [%s %s]", rec, r.Method, r.URL.Path)
				httpx.Fail(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()

		// 局域网工具，禁止被外站嵌套，同时禁止浏览器嗅探类型。
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")

		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 登录态

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResp struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	IsAdmin  bool   `json:"isAdmin"`
}

// toUserResp 把存储层用户转成对外的响应。
// 绝不包含 password_hash —— 响应结构体里根本没有这个字段，从类型层面杜绝泄露。
func toUserResp(u *storage.User) userResp {
	return userResp{
		ID:       u.ID,
		Username: u.Username,
		Role:     string(u.Role),
		IsAdmin:  u.IsAdmin(),
	}
}

// handleRegister 注册新用户。
//
// 策略（方案 B）：
//   - 系统里一个用户都没有 → 直接允许注册，且这个人成为管理员；
//   - 已经有用户 → 只有当注册开关打开时才允许（由管理员控制），
//     否则返回 403 并提示「注册已关闭」。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 注册开关：空库必然开放（首个用户即管理员），否则看设置项。
	open, err := s.store.IsRegistrationOpen()
	if err != nil {
		s.log.Error("读取注册开关失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "注册失败")
		return
	}
	if !open {
		httpx.Fail(w, http.StatusForbidden, "注册已关闭，请联系管理员开通账号")
		return
	}

	u, err := s.auth.Register(req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrUserExists):
			httpx.Fail(w, http.StatusConflict, "该用户名已被使用")
		case errors.Is(err, auth.ErrWeakPassword),
			errors.Is(err, auth.ErrEmptyUsername):
			httpx.Fail(w, http.StatusBadRequest, err.Error())
		default:
			// ValidateCredentials 里还有一条用户名非法字符的错误，透传文案。
			if strings.Contains(err.Error(), "只能包含") {
				httpx.Fail(w, http.StatusBadRequest, err.Error())
				return
			}
			s.log.Error("注册失败: %v", err)
			httpx.Fail(w, http.StatusInternalServerError, "注册失败")
		}
		return
	}

	// 注册成功后直接登录，省掉一次输入。
	id := httpx.NewID()
	if err := s.store.CreateSession(id, u.ID, sessionTTL(s.sessionTTL)); err != nil {
		s.log.Error("注册后创建会话失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "注册成功但自动登录失败，请手动登录")
		return
	}
	http.SetCookie(w, httpx.SessionCookie(id, sessionTTL(s.sessionTTL), false))

	if u.IsAdmin() {
		s.log.Info("首个用户注册成为管理员: %s (ip=%s)", u.Username, httpx.ClientIP(r))
	} else {
		s.log.Info("新用户注册: %s (ip=%s)", u.Username, httpx.ClientIP(r))
	}
	httpx.WriteJSON(w, http.StatusOK, toUserResp(u))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	u, err := s.auth.Login(req.Username, req.Password)
	if err != nil {
		// 登录失败必须记录（文档第 18 节）。
		s.log.Info("登录失败: user=%q ip=%s", req.Username, httpx.ClientIP(r))
		if errors.Is(err, auth.ErrInvalidCredentials) {
			httpx.Fail(w, http.StatusUnauthorized, "用户名或密码错误")
			return
		}
		s.log.Error("登录异常: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "登录失败")
		return
	}

	id := httpx.NewID()
	if err := s.store.CreateSession(id, u.ID, sessionTTL(s.sessionTTL)); err != nil {
		s.log.Error("创建登录会话失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "登录失败")
		return
	}

	http.SetCookie(w, httpx.SessionCookie(id, sessionTTL(s.sessionTTL), false))
	s.log.Info("用户登录成功: %s (ip=%s, role=%s)", u.Username, httpx.ClientIP(r), u.Role)
	httpx.WriteJSON(w, http.StatusOK, toUserResp(u))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(httpx.CookieName); err == nil && c.Value != "" {
		_ = s.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, httpx.ClearCookie())
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := httpx.CurrentUser(r, s.store)
	if u == nil {
		httpx.Fail(w, http.StatusUnauthorized, "未登录")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUserResp(u))
}

// ---------------------------------------------------------------- 状态

// statusResp 是 /api/status 的响应。
//
// needSetup 让前端能区分「系统还没初始化」和「注册被关闭了」：
//
//	没有用户        → needSetup=true，界面引导「创建管理员账号」
//	有用户但关注册  → needSetup=false, registrationOpen=false，只让登录
type statusResp struct {
	Product          string    `json:"product"`
	Version          string    `json:"version"`
	LoggedIn         bool      `json:"loggedIn"`
	UserCount        int       `json:"userCount"`
	NeedSetup        bool      `json:"needSetup"`
	RegistrationOpen bool      `json:"registrationOpen"`
	Rooms            int       `json:"rooms"`
	OnlineTotal      int       `json:"onlineTotal"`
	PermanentNum     int       `json:"permanentNum"`
	PermanentUse     int64     `json:"permanentUse"`
	User             *userResp `json:"user,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	u := httpx.CurrentUser(r, s.store)
	rooms, clients := s.sessions.Stats()

	resp := statusResp{
		Product:     "LAN Share",
		Version:     Version,
		LoggedIn:    u != nil,
		Rooms:       rooms,
		OnlineTotal: clients,
	}

	if n, err := s.store.CountUsers(); err == nil {
		resp.UserCount = n
		resp.NeedSetup = n == 0
	}
	// 注册开关：空库时 IsRegistrationOpen 会直接返回 true（首个用户即管理员）。
	if open, err := s.store.IsRegistrationOpen(); err == nil {
		resp.RegistrationOpen = open
	}
	if n, err := s.store.TotalUsage(storage.KindPermanent); err == nil {
		resp.PermanentUse = n
	}
	if list, err := s.store.ListFiles(storage.KindPermanent); err == nil {
		resp.PermanentNum = len(list)
	}
	if u != nil {
		ur := toUserResp(u)
		resp.User = &ur
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// Version 由编译期注入（-ldflags "-X lanshare/internal/api.Version=..."），此处仅作默认值。
var Version = "dev"

// sessionTTL 把配置里的秒数转成 time.Duration。
func sessionTTL(sec int) time.Duration { return time.Duration(sec) * time.Second }

// tempTTL 同上，用于临时文件存活时间。
func tempTTL(sec int) time.Duration { return time.Duration(sec) * time.Second }

// nowTime 集中取当前时间，便于测试替换。
func nowTime() time.Time { return time.Now() }

// parseID 解析路径里的正整数 ID。
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
