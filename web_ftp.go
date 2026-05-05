// web_ftp.go — HTTP endpoints for FTP settings management
//
// All three endpoints require authentication (same requiresAuth check used
// throughout web.go).  FTP credentials are never exposed to unauthenticated
// callers.
//
// Endpoints:
//
//	GET  /api/ftp/settings  — return current FTP config (auth required)
//	POST /api/ftp/settings  — save FTP config (auth required)
//	POST /api/ftp/test      — test FTP connection using supplied config (auth required)
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
)

// registerFTPHandlers wires the /api/ftp/* endpoints into the given ServeMux.
// It is called from startHTTPServer in web.go.
func registerFTPHandlers(
	mux *http.ServeMux,
	ftpCfg *ftpSettings,
	ftpMu *sync.RWMutex,
	ftpCfgPath string,
	uiPassword string,
	sessions *sessionStore,
) {
	// ── GET / POST /api/ftp/settings ─────────────────────────────────────────
	mux.HandleFunc("/api/ftp/settings", func(w http.ResponseWriter, r *http.Request) {
		// Both GET and POST require authentication — FTP credentials must never
		// be returned to unauthenticated callers.
		if !requiresAuth(w, r, uiPassword, sessions) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			ftpMu.RLock()
			cfg := *ftpCfg
			ftpMu.RUnlock()
			jsonResponse(w, cfg)

		case http.MethodPost:
			var cfg ftpSettings
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			// Apply defaults for zero values
			if cfg.IntervalMins <= 0 {
				cfg.IntervalMins = 15
			}
			if cfg.WindowMins <= 0 {
				cfg.WindowMins = 15
			}
			ftpMu.Lock()
			*ftpCfg = cfg
			ftpMu.Unlock()
			if err := saveFTPSettings(ftpCfgPath, cfg); err != nil {
				log.Printf("[web/ftp] save settings: %v", err)
				http.Error(w, "failed to save FTP settings", http.StatusInternalServerError)
				return
			}
			log.Printf("[web/ftp] settings updated: host=%s enabled=%v interval=%dm window=%dm tls=%v",
				cfg.Host, cfg.Enabled, cfg.IntervalMins, cfg.WindowMins, cfg.TLS)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// ── POST /api/ftp/test ───────────────────────────────────────────────────
	// Accepts the FTP config as the request body (uses the form values, not the
	// saved settings, so the user can test before saving).
	mux.HandleFunc("/api/ftp/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requiresAuth(w, r, uiPassword, sessions) {
			return
		}
		var cfg ftpSettings
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		result := testFTPConnection(cfg)
		if result.OK {
			log.Printf("[ftp/test] connection test passed: host=%s port=%d tls=%v user=%s",
				cfg.Host, cfg.Port, cfg.TLS, cfg.Username)
		} else {
			// Find the first failed step for a concise error message
			failDetail := "unknown"
			for _, s := range result.Steps {
				if s.Status == "error" {
					failDetail = s.Step + ": " + s.Detail
					break
				}
			}
			log.Printf("[ftp/test] connection test FAILED: host=%s port=%d tls=%v user=%s — %s",
				cfg.Host, cfg.Port, cfg.TLS, cfg.Username, failDetail)
		}
		jsonResponse(w, result)
	})
}
