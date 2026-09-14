package api

import (
	"net/http"
	"strings"

	"lanshare/internal/httpx"
	"lanshare/internal/storage"
)

// 本文件实现「管理员」相关的少量接口。
//
// 第一版刻意不做完整的管理后台（文档明确排除），
// 只保留真正必要的一件事：控制注册开关。
// 因为方案 B 下「首个用户成为管理员、之后注册自动关闭」，
// 管理员必须有个地方能把注册重新打开，否则加人只能靠 SSH。

// adminUserResp 是管理员看到的用户条目。
type adminUserResp struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	IsAdmin   bool   `json:"isAdmin"`
	CreatedAt int64  `json:"createdAt"`
	FileCount int    `json:"fileCount"`
}

// requireAdmin 是管理接口的统一守卫。
// 返回 nil 时表示校验不通过，响应已经写好了。
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *storage.User {
	u := httpx.CurrentUser(r, s.store)
	if u == nil {
		httpx.Fail(w, http.StatusUnauthorized, "请先登录")
		return nil
	}
	if !u.IsAdmin() {
		httpx.Fail(w, http.StatusForbidden, "需要管理员权限")
		return nil
	}
	return u
}

// handleAdminUsers 列出全部用户（GET /api/admin/users）。
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	users, err := s.store.ListUsers()
	if err != nil {
		s.log.Error("列出用户失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "读取用户列表失败")
		return
	}

	out := make([]adminUserResp, 0, len(users))
	for _, u := range users {
		out = append(out, adminUserResp{
			ID:        u.ID,
			Username:  u.Username,
			Role:      string(u.Role),
			IsAdmin:   u.IsAdmin(),
			CreatedAt: u.CreatedAt.UnixMilli(),
		})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": out, "count": len(out)})
}

// settingsReq 是更新设置的请求体。
// 用指针区分「未提供」与「显式设为 false」。
type settingsReq struct {
	RegistrationOpen *bool `json:"registrationOpen"`
}

// handleAdminSettings 读取或修改运行时设置（GET/PUT /api/admin/settings）。
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	var req settingsReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	if req.RegistrationOpen != nil {
		v := "0"
		if *req.RegistrationOpen {
			v = "1"
		}
		if err := s.store.SetSetting(storage.SettingRegistrationOpen, v); err != nil {
			s.log.Error("保存注册开关失败: %v", err)
			httpx.Fail(w, http.StatusInternalServerError, "保存设置失败")
			return
		}
		s.log.Info("注册开关更新为: %s", v)
	}

	open, err := s.store.IsRegistrationOpen()
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "读取设置失败")
		return
	}
	// 没有用户时，注册逻辑上必然是开放的（首个用户走的是 setup 路径）。
	n, _ := s.store.CountUsers()
	if n == 0 {
		open = true
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"registrationOpen": open})
}

// handleAdminSetRole 修改某个用户的角色（PUT /api/admin/users/{id}/role）。
func (s *Server) handleAdminSetRole(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}

	id, ok := parseID(r.PathValue("id"))
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, "非法的用户 ID")
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	role := storage.Role(strings.TrimSpace(req.Role))
	if role != storage.RoleAdmin && role != storage.RoleUser {
		httpx.Fail(w, http.StatusBadRequest, "角色只能是 admin 或 user")
		return
	}

	target, err := s.store.UserByID(id)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "用户不存在")
		return
	}

	// 防止把最后一个管理员降级，导致没人能管理注册开关。
	if target.IsAdmin() && role == storage.RoleUser {
		n, err := s.store.CountAdmins()
		if err != nil {
			httpx.Fail(w, http.StatusInternalServerError, "校验管理员数量失败")
			return
		}
		if n <= 1 {
			httpx.Fail(w, http.StatusBadRequest, "系统至少需要保留一名管理员")
			return
		}
	}

	if err := s.store.SetUserRole(id, role); err != nil {
		s.log.Error("修改用户角色失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "修改角色失败")
		return
	}

	s.log.Info("用户角色修改: %s -> %s (操作者 %s)", target.Username, role, admin.Username)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": id, "username": target.Username, "role": string(role),
	})
}
