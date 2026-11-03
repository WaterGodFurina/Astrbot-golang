package dashboard

import (
	"encoding/json"
	"net/http"
)

// handleStorage serves storage cleanup endpoints (4.29.2):
//
//	GET  /api/v1/storage/status            -> per-target size status
//	POST /api/v1/storage/cleanup {target}  -> run a cleanup
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request, rest []string) {
	if s.storageCleaner == nil {
		writeJSON(w, http.StatusOK, apiError("storage cleaner unavailable"))
		return
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch {
	case r.Method == http.MethodGet && (sub == "" || sub == "status"):
		writeJSON(w, http.StatusOK, apiOK(s.storageCleaner.GetStatus()))
	case r.Method == http.MethodPost && sub == "cleanup":
		var body struct {
			Target string `json:"target"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		result, err := s.storageCleaner.Cleanup(body.Target)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(result))
	default:
		writeJSON(w, http.StatusNotFound, apiError("unknown storage endpoint"))
	}
}
