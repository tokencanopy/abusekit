package serve

import "net/http"

// handleHealthz is GET /healthz: unauthenticated liveness, matching this
// repo's design table calling `serve` "shallow by design" — it proves the
// process is up and answering HTTP, nothing more. It does not touch the
// database: a DB-down condition is the worker/store's own concern
// (design's `/healthz` "config_error" reporting, S1's config package, is
// a separate concept from this liveness check and isn't wired here — no
// hot-reload loop exists yet in S3's scope to report against).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
