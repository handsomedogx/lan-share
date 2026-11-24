package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"lanshare/internal/files"
	"lanshare/internal/httpx"
	"lanshare/internal/storage"
)

// folderResp 是返回给前端的文件夹元数据。
type folderResp struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ParentID  *int64 `json:"parentId,omitempty"`
	Owner     string `json:"owner"`
	CreatedAt int64  `json:"createdAt"`
}

func toFolderResp(f *storage.Folder) folderResp {
	return folderResp{
		ID:        f.ID,
		Name:      f.Name,
		ParentID:  f.ParentID,
		Owner:     f.OwnerName,
		CreatedAt: f.CreatedAt.UnixMilli(),
	}
}

func toFolderResps(list []*storage.Folder) []folderResp {
	out := make([]folderResp, 0, len(list))
	for _, f := range list {
		out = append(out, toFolderResp(f))
	}
	return out
}

// handleListFolders 列出仓库中的全部文件夹（对所有人开放，与文件列表一致）。
func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListFolders()
	if err != nil {
		s.log.Error("查询文件夹列表失败: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件夹列表失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"folders": toFolderResps(list),
	})
}

type createFolderReq struct {
	Name     string `json:"name"`
	ParentID *int64 `json:"parentId"`
}

// handleCreateFolder 创建文件夹（需登录）。
func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	user := httpx.CurrentUser(r, s.store)
	if user == nil {
		httpx.Fail(w, http.StatusUnauthorized, "请先登录后再创建文件夹")
		return
	}

	var req createFolderReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求内容格式不正确")
		return
	}

	raw := strings.TrimSpace(req.Name)
	if raw == "" {
		httpx.Fail(w, http.StatusBadRequest, "请输入文件夹名称")
		return
	}
	name := files.SafeDisplayName(raw)
	if name == "file" && raw != "file" {
		httpx.Fail(w, http.StatusBadRequest, "文件夹名称包含无效字符")
		return
	}

	parentPtr, ok := s.resolveTargetFolder(w, req.ParentID, "父文件夹不存在")
	if !ok {
		return
	}

	uid := user.ID
	rec := &storage.Folder{
		Name:      name,
		ParentID:  parentPtr,
		OwnerID:   &uid,
		OwnerName: user.Username,
		CreatedAt: nowTime(),
	}
	id, err := s.store.CreateFolder(rec)
	if err != nil {
		if errors.Is(err, storage.ErrFolderExists) {
			httpx.Fail(w, http.StatusConflict, err.Error())
			return
		}
		s.log.Error("创建文件夹失败: name=%q err=%v", name, err)
		httpx.Fail(w, http.StatusInternalServerError, "创建文件夹失败")
		return
	}
	rec.ID = id

	s.log.Info("创建文件夹: id=%d name=%q parent=%v user=%s ip=%s",
		id, name, derefID(parentPtr), user.Username, httpx.ClientIP(r))
	httpx.WriteJSON(w, http.StatusOK, toFolderResp(rec))
}

type updateFolderReq struct {
	Name     *string `json:"name"`
	ParentID *int64  `json:"parentId"`
}

// handleUpdateFolder 重命名或移动文件夹（登录 + 本人或管理员）。
func (s *Server) handleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	f, user, ok := s.writableFolder(w, r, "修改")
	if !ok {
		return
	}

	var req updateFolderReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "请求内容格式不正确")
		return
	}
	if req.Name == nil && req.ParentID == nil {
		httpx.Fail(w, http.StatusBadRequest, "没有需要修改的内容")
		return
	}

	updated := f

	if req.ParentID != nil {
		targetPtr, ok := s.resolveTargetFolder(w, req.ParentID, "目标文件夹不存在")
		if !ok {
			return
		}
		if !sameInt64Ptr(updated.ParentID, targetPtr) {
			next, err := s.store.MoveFolder(f.ID, targetPtr)
			if err != nil {
				switch {
				case errors.Is(err, storage.ErrNotFound):
					httpx.Fail(w, http.StatusNotFound, "文件夹不存在")
				case errors.Is(err, storage.ErrFolderCycle):
					httpx.Fail(w, http.StatusBadRequest, err.Error())
				case errors.Is(err, storage.ErrFolderExists):
					httpx.Fail(w, http.StatusConflict, err.Error())
				default:
					s.log.Error("移动文件夹失败: id=%d err=%v", f.ID, err)
					httpx.Fail(w, http.StatusInternalServerError, "移动文件夹失败")
				}
				return
			}
			s.log.Info("移动文件夹: id=%d name=%q parent=%d user=%s ip=%s",
				f.ID, updated.Name, derefID(targetPtr), user.Username, httpx.ClientIP(r))
			updated = next
		}
	}

	if req.Name != nil {
		raw := strings.TrimSpace(*req.Name)
		if raw == "" {
			httpx.Fail(w, http.StatusBadRequest, "请输入文件夹名称")
			return
		}
		name := files.SafeDisplayName(raw)
		if name == "file" && raw != "file" {
			httpx.Fail(w, http.StatusBadRequest, "文件夹名称包含无效字符")
			return
		}
		if name != updated.Name {
			next, err := s.store.RenameFolder(f.ID, name)
			if err != nil {
				switch {
				case errors.Is(err, storage.ErrNotFound):
					httpx.Fail(w, http.StatusNotFound, "文件夹不存在")
				case errors.Is(err, storage.ErrFolderExists):
					httpx.Fail(w, http.StatusConflict, err.Error())
				default:
					s.log.Error("重命名文件夹失败: id=%d err=%v", f.ID, err)
					httpx.Fail(w, http.StatusInternalServerError, "重命名文件夹失败")
				}
				return
			}
			s.log.Info("重命名文件夹: id=%d %q -> %q user=%s ip=%s",
				f.ID, updated.Name, name, user.Username, httpx.ClientIP(r))
			updated = next
		}
	}

	httpx.WriteJSON(w, http.StatusOK, toFolderResp(updated))
}

// handleDeleteFolder 删除文件夹，并将其内部直接子文件与子文件夹上移到父目录。
func (s *Server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	f, user, ok := s.writableFolder(w, r, "删除")
	if !ok {
		return
	}

	movedFiles, movedFolders, err := s.store.DeleteFolderAndUnwrap(f.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件夹不存在")
			return
		}
		s.log.Error("删除文件夹失败: id=%d err=%v", f.ID, err)
		httpx.Fail(w, http.StatusInternalServerError, "删除文件夹失败")
		return
	}

	s.log.Info("删除文件夹: id=%d name=%q movedFiles=%d movedFolders=%d user=%s ip=%s",
		f.ID, f.Name, movedFiles, movedFolders, user.Username, httpx.ClientIP(r))

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"id":           f.ID,
		"movedFiles":   movedFiles,
		"movedFolders": movedFolders,
	})
}

func (s *Server) writableFolder(w http.ResponseWriter, r *http.Request, action string) (*storage.Folder, *storage.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, http.StatusBadRequest, "非法的文件夹 ID")
		return nil, nil, false
	}

	f, err := s.store.FolderByID(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, "文件夹不存在")
			return nil, nil, false
		}
		s.log.Error("查询文件夹失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件夹失败")
		return nil, nil, false
	}

	user := httpx.CurrentUser(r, s.store)
	if user == nil {
		httpx.Fail(w, http.StatusUnauthorized, "请先登录后再"+action+"文件夹")
		return nil, nil, false
	}
	if !user.IsAdmin() && !ownsFolder(f, user) {
		httpx.Fail(w, http.StatusForbidden, "只能"+action+"自己创建的文件夹")
		return nil, nil, false
	}
	return f, user, true
}

func (s *Server) resolveTargetFolder(w http.ResponseWriter, raw *int64, notFoundMsg string) (*int64, bool) {
	if raw == nil || *raw == 0 {
		return nil, true
	}
	if *raw < 0 {
		httpx.Fail(w, http.StatusBadRequest, "非法的文件夹 ID")
		return nil, false
	}
	id := *raw
	if _, err := s.store.FolderByID(id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, notFoundMsg)
			return nil, false
		}
		s.log.Error("查询目标文件夹失败: id=%d err=%v", id, err)
		httpx.Fail(w, http.StatusInternalServerError, "读取文件夹失败")
		return nil, false
	}
	return &id, true
}

func ownsFolder(f *storage.Folder, u *storage.User) bool {
	if f == nil || u == nil || f.OwnerID == nil {
		return false
	}
	return *f.OwnerID == u.ID
}

func derefID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
