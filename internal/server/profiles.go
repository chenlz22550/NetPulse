package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"netping/internal/config"
	"netping/internal/profiles"
)

type aiEpochKey struct{}

type aiSessionEpoch struct {
	profileID string
	epoch     int64
}

func (s *Server) activeProfileID() string {
	if s.Profiles == nil {
		return profiles.DefaultProfileID
	}
	return s.Profiles.ActiveID()
}

func (s *Server) activeProfileState() (string, int64) {
	if s.Profiles == nil {
		return profiles.DefaultProfileID, 0
	}
	return s.Profiles.ActiveState()
}

func (s *Server) checkProfileHeader(r *http.Request) error {
	want := strings.TrimSpace(r.Header.Get("X-NetPing-Profile"))
	if want == "" {
		return nil
	}
	cur := s.activeProfileID()
	if want != cur {
		return &apiError{
			status:        http.StatusConflict,
			code:          "profile_mismatch",
			activeProfile: cur,
			msg:           "当前激活机房已切换，请刷新页面后重试",
		}
	}
	return nil
}

func (s *Server) syncActiveRoomSettings(c config.Config) error {
	if s.Profiles == nil {
		return nil
	}
	return s.Profiles.SaveActiveSettings(c.RoomSettings())
}

func (s *Server) withBoundProfileEpoch(ctx context.Context, profileID string, epoch int64) context.Context {
	if s.Profiles == nil {
		return ctx
	}
	return context.WithValue(ctx, aiEpochKey{}, aiSessionEpoch{profileID: profileID, epoch: epoch})
}

func (s *Server) boundProfileEpoch(ctx context.Context) (string, int64, bool) {
	snap, ok := ctx.Value(aiEpochKey{}).(aiSessionEpoch)
	return snap.profileID, snap.epoch, ok
}

func (s *Server) withProfileEpoch(ctx context.Context) context.Context {
	if s.Profiles == nil {
		return ctx
	}
	if _, ok := ctx.Value(aiEpochKey{}).(aiSessionEpoch); ok {
		return ctx
	}
	id, ep := s.Profiles.ActiveState()
	return context.WithValue(ctx, aiEpochKey{}, aiSessionEpoch{profileID: id, epoch: ep})
}

func (s *Server) checkActiveBindingLocked(wantProfileID string, wantEpoch *int64) (string, int64, error) {
	activeID, epoch := s.activeProfileState()
	if wantProfileID != "" && wantProfileID != activeID {
		return activeID, epoch, &apiError{
			status: http.StatusConflict,
			msg:    fmt.Sprintf("当前操作仅支持当前激活机房（请求 %q，当前激活 %q，epoch=%d）", wantProfileID, activeID, epoch),
		}
	}
	if wantEpoch != nil && *wantEpoch != epoch {
		return activeID, epoch, &apiError{
			status: http.StatusConflict,
			msg:    fmt.Sprintf("机房版本 epoch 已变化（请求 %d，当前 %d），请刷新后重试", *wantEpoch, epoch),
		}
	}
	return activeID, epoch, nil
}

func (s *Server) checkProfileEpoch(ctx context.Context) error {
	if s.Profiles == nil {
		return nil
	}
	snap, ok := ctx.Value(aiEpochKey{}).(aiSessionEpoch)
	if !ok {
		return nil
	}
	curID, curEp := s.Profiles.ActiveState()
	if curID != snap.profileID || curEp != snap.epoch {
		return errors.New("当前激活机房已切换，本次 AI 操作已自动取消")
	}
	return nil
}

func mapProfileErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, profiles.ErrNotFound), errors.Is(err, profiles.ErrVersionNotFound):
		return &apiError{status: http.StatusNotFound, msg: err.Error()}
	case errors.Is(err, profiles.ErrBusy), errors.Is(err, profiles.ErrConflict):
		return &apiError{status: http.StatusConflict, msg: err.Error()}
	default:
		var ve interface{ Error() string }
		_ = ve
		if strings.Contains(err.Error(), "拓扑校验失败") {
			return err
		}
		return badRequest("%s", err)
	}
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request, arg string) error {
	if s.Profiles == nil {
		return &apiError{status: http.StatusServiceUnavailable, msg: "机房配置管理器未初始化"}
	}
	arg = strings.Trim(arg, "/")
	if arg == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, s.Profiles.List())
			return nil
		case http.MethodPost:
			var req struct {
				Name        string `json:"name"`
				CopyCurrent bool   `json:"copy_current"`
				SourceID    string `json:"source_id"`
			}
			if err := decode(r, &req); err != nil {
				return err
			}
			info, err := s.Profiles.Create(req.Name, req.CopyCurrent, req.SourceID)
			if err != nil {
				return mapProfileErr(err)
			}
			s.okRev(w, map[string]interface{}{"id": info.ID, "profile": info})
			return nil
		default:
			return &apiError{status: http.StatusMethodNotAllowed, msg: "不支持的请求方法"}
		}
	}

	parts := strings.Split(arg, "/")
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodPut:
			var req struct {
				Name string `json:"name"`
			}
			if err := decode(r, &req); err != nil {
				return err
			}
			if err := s.Profiles.Rename(id, req.Name); err != nil {
				return mapProfileErr(err)
			}
			s.okRev(w, map[string]interface{}{"id": id})
			return nil
		case http.MethodDelete:
			if err := s.Profiles.Delete(id); err != nil {
				return mapProfileErr(err)
			}
			s.okRev(w, map[string]interface{}{"id": id})
			return nil
		case http.MethodPost:
			var req struct {
				Action string `json:"action"`
			}
			if err := decode(r, &req); err != nil {
				return err
			}
			if req.Action != "activate" {
				return badRequest("操作必须为 activate")
			}
			return s.activateProfile(w, id)
		default:
			return &apiError{status: http.StatusMethodNotAllowed, msg: "不支持的请求方法"}
		}

	case len(parts) == 2 && sub == "activate" && r.Method == http.MethodPost:
		return s.activateProfile(w, id)

	case len(parts) == 2 && sub == "preview" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		probe := r.URL.Query().Get("probe") != "0" && r.URL.Query().Get("probe") != "false"
		res, err := s.Profiles.Preview(id, probe)
		if err != nil {
			return mapProfileErr(err)
		}
		writeJSON(w, res)
		return nil

	case len(parts) == 2 && sub == "settings":
		switch r.Method {
		case http.MethodGet:
			rs, meta, active, err := s.Profiles.GetSettings(id)
			if err != nil {
				return mapProfileErr(err)
			}
			writeJSON(w, map[string]interface{}{
				"id":       meta.ID,
				"name":     meta.Name,
				"active":   active,
				"settings": config.MaskRoomSettings(rs),
			})
			return nil
		case http.MethodPut:
			raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				return err
			}
			rs, err := s.Profiles.UpdateSettings(id, raw)
			if err != nil {
				return mapProfileErr(err)
			}
			s.okRev(w, map[string]interface{}{"id": id, "settings": config.MaskRoomSettings(rs)})
			return nil
		default:
			return &apiError{status: http.StatusMethodNotAllowed, msg: "不支持的请求方法"}
		}

	case len(parts) == 2 && sub == "versions":
		switch r.Method {
		case http.MethodGet:
			list, err := s.Profiles.ListVersions(id)
			if err != nil {
				return mapProfileErr(err)
			}
			writeJSON(w, list)
			return nil
		case http.MethodPost:
			var req struct {
				Note string `json:"note"`
			}
			if r.ContentLength != 0 {
				if err := decode(r, &req); err != nil {
					return err
				}
			}
			meta, err := s.Profiles.CreateVersion(id, "manual", req.Note)
			if err != nil {
				return mapProfileErr(err)
			}
			writeJSON(w, map[string]interface{}{"ok": true, "version": meta})
			return nil
		default:
			return &apiError{status: http.StatusMethodNotAllowed, msg: "不支持的请求方法"}
		}

	case len(parts) == 3 && sub == "versions" && r.Method == http.MethodGet:
		verID := parts[2]
		snap, err := s.Profiles.GetVersion(id, verID)
		if err != nil {
			return mapProfileErr(err)
		}
		writeJSON(w, map[string]interface{}{
			"meta":     snap.Meta,
			"topology": snap.Topology,
			"settings": config.MaskRoomSettings(snap.Settings),
		})
		return nil

	case len(parts) == 4 && sub == "versions" && parts[3] == "rollback" && r.Method == http.MethodPost:
		return s.rollbackProfileVersion(w, id, parts[2])

	case len(parts) == 2 && sub == "rollback" && r.Method == http.MethodPost:
		var req struct {
			VersionID string `json:"version_id"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if req.VersionID == "" {
			return badRequest("需要提供 version_id")
		}
		return s.rollbackProfileVersion(w, id, req.VersionID)
	}

	return notFound("接口")
}

func (s *Server) activateProfile(w http.ResponseWriter, id string) error {
	if err := s.Profiles.Activate(id); err != nil {
		return mapProfileErr(err)
	}
	s.okRev(w, map[string]interface{}{
		"id":             id,
		"active_profile": s.activeProfileID(),
	})
	return nil
}

func (s *Server) rollbackProfileVersion(w http.ResponseWriter, id, verID string) error {
	restored, backup, err := s.Profiles.RollbackVersion(id, verID)
	if err != nil {
		return mapProfileErr(err)
	}
	s.okRev(w, map[string]interface{}{
		"id":               id,
		"restored_version": restored.ID,
		"backup_version":   backup.ID,
		"active_profile":   s.activeProfileID(),
	})
	return nil
}
