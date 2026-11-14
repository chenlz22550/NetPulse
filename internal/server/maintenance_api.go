package server

import (
	"net/http"
	"netping/internal/alert"
	"os"
	"strings"
	"time"
)

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request, arg string) error {
	id := strings.TrimPrefix(arg, "/")
	switch {
	case r.Method == http.MethodGet && id == "":
		writeJSON(w, s.Alerts.MaintenanceWindows(time.Now()))
		return nil
	case r.Method == http.MethodPost && id == "":
		var req alert.MaintenanceWindow
		if err := decode(r, &req); err != nil {
			return err
		}
		created, err := s.Alerts.AddMaintenance(req)
		if err != nil {
			return badRequest(err.Error())
		}
		writeJSON(w, created)
		return nil
	case r.Method == http.MethodDelete && id != "":
		if err := s.Alerts.CancelMaintenance(id); err != nil {
			if os.IsNotExist(err) {
				return notFound("维护窗口")
			}
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	}
	return notFound("接口")
}
