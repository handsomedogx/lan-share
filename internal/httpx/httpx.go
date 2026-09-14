// Package httpx 提供「浏览器登录态」相关的 HTTP 辅助能力。
package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"lanshare/internal/storage"
)

// CookieName 是登录 Cookie 的名称。
const CookieName = "lanshare_session"

// SessionCookie 根据会话 ID 构造登录 Cookie。
//
// 安全设计（对应文档第 11 节）：
//   - HttpOnly：前端 JS 无法读取，避免 XSS 直接盗取会话
//   - SameSite=Lax：防止跨站请求伪造
//   - Secure 暂不开启：局域网 HTTP 环境下开启会导致 Cookie 根本发不出去，
//     未来接入 HTTPS 后再置为 true（函数已预留参数）
func SessionCookie(id string, ttl time.Duration, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(ttl.Seconds()),
	}
}

// ClearCookie 返回一个立即过期的 Cookie，用于登出。
func ClearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// NewID 生成 32 字节随机十六进制串，用作会话 ID。
func NewID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下的兜底，仍然保证唯一性。
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

// CurrentUser 从请求中解析当前登录用户；未登录返回 nil。
func CurrentUser(r *http.Request, store *storage.Store) *storage.User {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := store.SessionUser(c.Value)
	if err != nil {
		return nil
	}
	return u
}

// ---------------------------------------------------------------- JSON

// WriteJSON 以 JSON 形式返回数据。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 局域网内都是自己的前端，禁用缓存避免刷新拿到旧数据。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// ErrorBody 是统一的错误响应结构。
type ErrorBody struct {
	Error string `json:"error"`
}

// Fail 返回统一格式的错误。
func Fail(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorBody{Error: msg})
}

// DecodeJSON 解析请求体 JSON，限制体积防止内存被打爆。
func DecodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// ClientIP 取客户端 IP，优先信任 nginx 传来的 X-Forwarded-For 首个地址。
// 只用于日志，因此不必担心伪造。
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return xr
	}
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
