// web.go — HTTP server, SSE live feed, REST API for ubersdr_doppler
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFiles embed.FS

// ---------------------------------------------------------------------------
// Session store — in-memory set of valid session tokens
// ---------------------------------------------------------------------------

type sessionStore struct {
	mu     sync.RWMutex
	tokens map[string]struct{}
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: make(map[string]struct{})}
}

func (s *sessionStore) create() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("session token generation failed: " + err.Error())
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = struct{}{}
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.tokens[tok]
	s.mu.RUnlock()
	return ok
}

const sessionCookieName = "ui_session"

// requiresAuth checks that the request carries a valid session cookie.
// If uiPassword is empty, the UI is open/unprotected and all write actions are allowed.
func requiresAuth(w http.ResponseWriter, r *http.Request, uiPassword string, sessions *sessionStore) bool {
	if uiPassword == "" {
		// No password configured — open access, allow all writes.
		return true
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || !sessions.valid(cookie.Value) {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// SSE hub — fan-out of live DopplerReading events to browser clients
// ---------------------------------------------------------------------------

// allowedSpecIntervals is the set of valid spectrum update intervals (seconds).
var allowedSpecIntervals = map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}

type sseClient struct {
	ch           chan string
	label        string        // "" = subscribe to all stations
	clientToken  string        // random token used to identify this SSE connection
	specInterval time.Duration // how often to push spectrum frames (default 2s)
	lastSpec     time.Time     // last time a spectrum frame was sent to this client
}

type sseHub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
	byToken map[string]*sseClient // clientToken → client
}

func newSSEHub() *sseHub {
	return &sseHub{
		clients: make(map[*sseClient]struct{}),
		byToken: make(map[string]*sseClient),
	}
}

func (h *sseHub) subscribe(label, clientToken string) *sseClient {
	c := &sseClient{
		ch:           make(chan string, 64),
		label:        label,
		clientToken:  clientToken,
		specInterval: 2 * time.Second,
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	if clientToken != "" {
		h.byToken[clientToken] = c
	}
	h.mu.Unlock()
	return c
}

func (h *sseHub) unsubscribe(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	if c.clientToken != "" {
		delete(h.byToken, c.clientToken)
	}
	h.mu.Unlock()
	close(c.ch)
}

// broadcast sends a live reading to all subscribed SSE clients.
func (h *sseHub) broadcast(label string, r DopplerReading) {
	type payload struct {
		Station    string         `json:"station"`
		Reading    DopplerReading `json:"reading"`
		ServerTime time.Time      `json:"server_time"` // wall-clock time of this broadcast; used by the frontend to detect backend→UberSDR staleness
	}
	// Note: r.CorrectedDopplerHz is already set by runSpectrumLoop before calling broadcast.
	data, err := json.Marshal(payload{Station: label, Reading: r, ServerTime: time.Now().UTC()})
	if err != nil {
		return
	}
	msg := "data: " + string(data) + "\n\n"

	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.label == "" || c.label == label {
			select {
			case c.ch <- msg:
			default:
				// Slow client — drop frame.
			}
		}
	}
}

// broadcastSpectrum sends a spectrum update to SSE clients that are due for
// their next frame based on their individual specInterval preference.
//
// Encoding pipeline (minimises payload size):
//  1. float32 dBFS → uint8  (value = dBFS + 256; -256→0, 0→255)
//  2. gzip compress          (spectrum data compresses very well, ~3–5×)
//  3. base64-encode          (makes binary safe for SSE text transport)
func (h *sseHub) broadcastSpectrum(label string, bins []float32, peakBin int, binBW float64) {
	// Step 1: float32 dBFS → uint8
	u8 := make([]byte, len(bins))
	for i, v := range bins {
		if v < -255 {
			v = -255
		} else if v > 0 {
			v = 0
		}
		u8[i] = byte(int(v) + 256)
	}

	// Step 2: gzip compress
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = gz.Write(u8)
	_ = gz.Close()

	// Step 3: base64-encode
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	type payload struct {
		Station      string  `json:"station"`
		SpectrumGZ   string  `json:"spectrum_gz"` // base64(gzip(uint8 bins))
		PeakBin      int     `json:"peak_bin"`
		BinBandwidth float64 `json:"bin_bandwidth"`
	}
	data, err := json.Marshal(payload{
		Station:      label,
		SpectrumGZ:   b64,
		PeakBin:      peakBin,
		BinBandwidth: binBW,
	})
	if err != nil {
		return
	}
	msg := "event: spectrum\ndata: " + string(data) + "\n\n"

	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.label != "" && c.label != label {
			continue
		}
		// Throttle per client based on their chosen interval.
		if now.Sub(c.lastSpec) < c.specInterval {
			continue
		}
		c.lastSpec = now
		select {
		case c.ch <- msg:
		default:
			// Slow client — drop frame.
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

func startHTTPServer(
	addr string,
	mgr *stationManager,
	hub *sseHub,
	settingsPath string,
	settings *globalSettings,
	settingsMu *sync.RWMutex,
	cw *csvWriter,
	uiPassword string,
	ftpCfg *ftpSettings,
	ftpMu *sync.RWMutex,
	ftpCfgPath string,
	descCache *receiverDescCache,
	ubersdrHTTPBase string,
) error {
	sessions := newSessionStore()
	mux := http.NewServeMux()

	// index.html served as a Go template so BASE_PATH can be injected.
	// BASE_PATH is read from the X-Forwarded-Prefix header set by UberSDR's
	// addon proxy (strip_prefix: true sends this header).
	indexTmpl, indexTmplErr := func() (*template.Template, error) {
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			return nil, err
		}
		return template.New("index").Parse(string(data))
	}()

	basePath := func(r *http.Request) string {
		return strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
	}

	// Static files (embedded) — all except index.html.
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	staticHandler := http.FileServer(http.FS(sub))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			if indexTmplErr != nil {
				http.Error(w, "template error: "+indexTmplErr.Error(), http.StatusInternalServerError)
				return
			}
			bp := basePath(r)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			indexTmpl.Execute(w, map[string]string{"BasePath": bp}) //nolint:errcheck
			return
		}
		staticHandler.ServeHTTP(w, r)
	})

	// ── Auth endpoints ─────────────────────────────────────────────────────
	// GET /api/auth/status
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		configured := uiPassword != ""
		authed := false
		if configured {
			if cookie, err := r.Cookie(sessionCookieName); err == nil {
				authed = sessions.valid(cookie.Value)
			}
		}
		jsonResponse(w, map[string]interface{}{
			"password_configured": configured,
			"authenticated":       authed,
		})
	})

	// POST /api/auth/login  {"password":"..."}
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if uiPassword == "" {
			http.Error(w, `{"error":"no password configured"}`, http.StatusForbidden)
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.Password != uiPassword {
			http.Error(w, `{"error":"incorrect password"}`, http.StatusUnauthorized)
			return
		}
		tok := sessions.create()
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		jsonResponse(w, map[string]interface{}{"ok": true})
	})

	// POST /api/auth/logout
	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:    sessionCookieName,
			Value:   "",
			Path:    "/",
			Expires: time.Unix(0, 0),
			MaxAge:  -1,
		})
		w.WriteHeader(http.StatusNoContent)
	})

	// ── Live SSE feed ──────────────────────────────────────────────────────
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("station")
		// client_token is a random token generated by the browser and passed
		// as a query parameter so the /api/spectrum-interval endpoint can
		// look up this specific SSE connection and update its push rate.
		clientToken := r.URL.Query().Get("client_token")

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		client := hub.subscribe(label, clientToken)
		defer hub.unsubscribe(client)

		// Send an immediate named "connected" event so the client knows the
		// SSE connection is live even before any station readings arrive.
		// Named events (event: connected) trigger addEventListener listeners
		// and also onmessage in browsers that support it.
		fmt.Fprint(w, "event: connected\ndata: {}\n\n")
		flusher.Flush()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case msg, ok := <-client.ch:
				if !ok {
					return
				}
				fmt.Fprint(w, msg)
				flusher.Flush()
			case <-ticker.C:
				// Named heartbeat event (not a comment) so the client can
				// use addEventListener('heartbeat') to confirm liveness.
				fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// ── Spectrum update interval ────────────────────────────────────────────
	// POST /api/spectrum-interval  {"interval_s": 2, "client_token": "<token>"}
	// Sets how often spectrum frames are pushed to the requesting SSE client.
	// Accepted values: 1, 2, 3, 4, 5 (seconds).
	mux.HandleFunc("/api/spectrum-interval", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			IntervalS   int    `json:"interval_s"`
			ClientToken string `json:"client_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if !allowedSpecIntervals[body.IntervalS] {
			http.Error(w, "interval_s must be 1, 2, 3, 4, or 5", http.StatusBadRequest)
			return
		}
		if body.ClientToken == "" {
			http.Error(w, "client_token is required", http.StatusBadRequest)
			return
		}
		hub.mu.Lock()
		c, ok := hub.byToken[body.ClientToken]
		if ok {
			c.specInterval = time.Duration(body.IntervalS) * time.Second
			c.lastSpec = time.Time{} // reset so next frame fires immediately
		}
		hub.mu.Unlock()
		if !ok {
			http.Error(w, "client_token not found — SSE connection may have dropped", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ── Global settings ────────────────────────────────────────────────────
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			settingsMu.RLock()
			s := *settings
			settingsMu.RUnlock()
			jsonResponse(w, s)

		case http.MethodPost:
			// Write action — requires authentication
			if !requiresAuth(w, r, uiPassword, sessions) {
				return
			}
			var s globalSettings
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			switch s.FrequencyReference {
			case FreqRefNone, FreqRefGPSDO, FreqRefReferenceStation:
				// valid
			default:
				http.Error(w, `frequency_reference must be "none", "gpsdo", or "reference_station"`, http.StatusBadRequest)
				return
			}
			settingsMu.Lock()
			*settings = s
			settingsMu.Unlock()
			if err := saveGlobalSettings(settingsPath, s); err != nil {
				log.Printf("[web] save settings: %v", err)
				http.Error(w, "failed to save settings", http.StatusInternalServerError)
				return
			}
			cw.UpdateSettings(s)
			log.Printf("[web] settings updated: freq_ref=%s node=%s", s.FrequencyReference, s.NodeNumber)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// ── Station list ───────────────────────────────────────────────────────
	mux.HandleFunc("/api/stations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		type stationStatus struct {
			Config           stationConfig  `json:"config"`
			Current          DopplerReading `json:"current"`
			BaselineMean     *float64       `json:"baseline_mean_hz"`
			BaselineN        int            `json:"baseline_n"`
			CorrectedDoppler *float64       `json:"corrected_doppler_hz"`
			SpectrumData     []float32      `json:"spectrum_data"` // latest unwrapped FFT bins for mini-spectrum display
			PeakBin          int            `json:"peak_bin"`      // peak bin index (-1 if no valid signal)
			BinBandwidth     float64        `json:"bin_bandwidth"` // actual Hz per bin (from server config message)
		}
		refPPM, refValid := mgr.referenceCorrection()
		settingsMu.RLock()
		manualOffset := settings.ManualOffsetHz
		settingsMu.RUnlock()

		stations := mgr.list()
		out := make([]stationStatus, 0, len(stations))
		for _, ds := range stations {
			mean, n := ds.BaselineMean(60)
			var meanPtr *float64
			if n >= 5 && !ds.cfg.IsReference {
				// Apply manual offset (reference correction is already baked into
				// each MinuteMean.CorrectedDopplerHz by BaselineMean).
				mean -= manualOffset
				meanPtr = &mean
			}
			cur := ds.CurrentReading()
			var corrPtr *float64
			if cur.Valid && !ds.cfg.IsReference {
				corr := cur.DopplerHz
				if refValid {
					// Scale reference ppm error to this station's frequency
					corr -= refPPM * float64(ds.cfg.FreqHz) / 1e6
				}
				corr -= manualOffset
				if refValid || manualOffset != 0 {
					corrPtr = &corr
				}
			}
			specBins, peakBin, binBW := ds.LatestSpectrum()
			out = append(out, stationStatus{
				Config:           ds.cfg,
				Current:          cur,
				BaselineMean:     meanPtr,
				BaselineN:        n,
				CorrectedDoppler: corrPtr,
				SpectrumData:     specBins,
				PeakBin:          peakBin,
				BinBandwidth:     binBW,
			})
		}
		jsonResponse(w, out)
	})

	// ── History ────────────────────────────────────────────────────────────
	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		label := r.URL.Query().Get("station")
		if label == "" {
			http.Error(w, "station parameter required", http.StatusBadRequest)
			return
		}
		// Optional smoothing window: ?smooth=N (integer minutes, 1–10; 1 = no smoothing)
		smoothN := 1
		if s := r.URL.Query().Get("smooth"); s != "" {
			var n int
			if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 || n > 10 {
				http.Error(w, "smooth must be an integer between 1 and 10", http.StatusBadRequest)
				return
			}
			smoothN = n
		}
		// Optional date filter: ?date=YYYY-MM-DD (UTC day)
		// When present, read directly from the per-date history file on disk.
		// When absent, return the in-memory rolling 24h window.
		dateStr := r.URL.Query().Get("date")
		for _, ds := range mgr.list() {
			if ds.cfg.Label != label {
				continue
			}
			if dateStr != "" {
				filterDate, err := time.ParseInLocation("2006-01-02", dateStr, time.UTC)
				if err != nil {
					http.Error(w, "invalid date format (expected YYYY-MM-DD)", http.StatusBadRequest)
					return
				}
				// Read from the per-date disk file — may be any historical day
				history := ds.HistoryForDate(filterDate)
				if history == nil {
					history = []MinuteMean{} // return empty array, not null
				}
				jsonResponse(w, smoothMinuteMeans(history, smoothN))
			} else {
				jsonResponse(w, smoothMinuteMeans(ds.History(), smoothN))
			}
			return
		}
		http.Error(w, "station not found", http.StatusNotFound)
	})

	// ── Station CRUD (all write — require auth) ────────────────────────────
	// POST /api/stations/add
	mux.HandleFunc("/api/stations/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requiresAuth(w, r, uiPassword, sessions) {
			return
		}
		var cfg stationConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if cfg.Label == "" {
			http.Error(w, "label is required", http.StatusBadRequest)
			return
		}
		if cfg.FreqHz <= 0 {
			http.Error(w, "freq_hz must be positive", http.StatusBadRequest)
			return
		}
		ds, err := mgr.add(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonResponse(w, ds.cfg)
	})

	// POST /api/stations/remove
	mux.HandleFunc("/api/stations/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requiresAuth(w, r, uiPassword, sessions) {
			return
		}
		var req struct {
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := mgr.remove(req.Label); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /api/stations/update
	mux.HandleFunc("/api/stations/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requiresAuth(w, r, uiPassword, sessions) {
			return
		}
		var cfg stationConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if cfg.ID == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := mgr.update(cfg.ID, cfg); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ── CSV download ───────────────────────────────────────────────────────
	// GET /api/csv?station=<label>&date=YYYY-MM-DD
	//   Full-day download (original behaviour — unchanged).
	//
	// GET /api/csv?station=<label>&last=<duration>
	//   Download the last <duration> of 1-second data.
	//   Accepted units: s (seconds), m (minutes), h (hours).
	//   Examples: last=15m  last=1h  last=3600s
	//
	// GET /api/csv?station=<label>&start=<RFC3339>[&end=<RFC3339>]
	//   Download data between two absolute UTC timestamps.
	//   end defaults to now when omitted.
	//
	// All forms return Grape-format CSV identical to the full-day download
	// (same # metadata header + UTC,Freq,Vpk data rows).
	mux.HandleFunc("/api/csv", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		label := r.URL.Query().Get("station")
		if label == "" {
			http.Error(w, "station parameter required", http.StatusBadRequest)
			return
		}
		if strings.ContainsAny(label, "/\\.") {
			http.Error(w, "invalid station label", http.StatusBadRequest)
			return
		}

		q := r.URL.Query()
		dateStr := q.Get("date")
		lastStr := q.Get("last")
		startStr := q.Get("start")
		endStr := q.Get("end")

		// ── Determine the time window ──────────────────────────────────────
		// rangeStart/rangeEnd are used for row-level filtering when fullDay is false.
		var rangeStart, rangeEnd time.Time
		var fullDay bool

		switch {
		case dateStr != "" && lastStr == "" && startStr == "" && endStr == "":
			// Legacy full-day mode — no row filtering needed.
			fullDay = true

		case lastStr != "":
			// Relative window: last=15m / last=1h / last=3600s
			dur, err := parseLastDuration(lastStr)
			if err != nil {
				http.Error(w, "invalid last parameter: "+err.Error(), http.StatusBadRequest)
				return
			}
			rangeEnd = time.Now().UTC()
			rangeStart = rangeEnd.Add(-dur)

		case startStr != "":
			// Absolute window.
			var err error
			rangeStart, err = time.Parse(time.RFC3339, startStr)
			if err != nil {
				http.Error(w, "invalid start (use RFC3339, e.g. 2006-01-02T15:04:05Z)", http.StatusBadRequest)
				return
			}
			rangeStart = rangeStart.UTC()
			if endStr != "" {
				rangeEnd, err = time.Parse(time.RFC3339, endStr)
				if err != nil {
					http.Error(w, "invalid end (use RFC3339)", http.StatusBadRequest)
					return
				}
				rangeEnd = rangeEnd.UTC()
			} else {
				rangeEnd = time.Now().UTC()
			}
			if !rangeEnd.After(rangeStart) {
				http.Error(w, "end must be after start", http.StatusBadRequest)
				return
			}

		default:
			http.Error(w, "provide date, last, or start/end parameters", http.StatusBadRequest)
			return
		}

		// ── Collect the set of UTC days we need to read ────────────────────
		var days []time.Time
		if fullDay {
			t, err := time.Parse("2006-01-02", dateStr)
			if err != nil {
				http.Error(w, "invalid date format (use YYYY-MM-DD)", http.StatusBadRequest)
				return
			}
			days = []time.Time{t.UTC()}
		} else {
			// The window may span midnight, so include every UTC day it touches.
			d := time.Date(rangeStart.Year(), rangeStart.Month(), rangeStart.Day(), 0, 0, 0, 0, time.UTC)
			endDay := time.Date(rangeEnd.Year(), rangeEnd.Month(), rangeEnd.Day(), 0, 0, 0, 0, time.UTC)
			for !d.After(endDay) {
				days = append(days, d)
				d = d.Add(24 * time.Hour)
			}
		}

		// Gather matching CSV file paths across all required days.
		// Each app restart creates a new timestamped file; os.ReadDir returns
		// entries alphabetically which is chronological for our filenames.
		suffix := "_FRQ_" + label + ".csv"
		var matchPaths []string
		for _, day := range days {
			dir := filepath.Join(mgr.dataDir,
				fmt.Sprintf("%04d", day.Year()),
				fmt.Sprintf("%02d", day.Month()),
				fmt.Sprintf("%02d", day.Day()),
			)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue // day directory may not exist yet
			}
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), suffix) {
					matchPaths = append(matchPaths, filepath.Join(dir, e.Name()))
				}
			}
		}
		if len(matchPaths) == 0 {
			http.Error(w, "no data for that station/date", http.StatusNotFound)
			return
		}

		// Stream response.
		dlName := filepath.Base(matchPaths[0])
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, dlName))
		w.Header().Set("Cache-Control", "no-cache")

		// firstFile tracks whether we have emitted a Grape header yet.
		// We always emit the header from the first matching file; subsequent
		// files have their headers stripped so the output is one clean CSV.
		firstFile := true
		for _, p := range matchPaths {
			// Determine the UTC date for this file from its directory path so
			// we can reconstruct full timestamps for range filtering.
			fileDay := csvFileDay(p)

			f, err := os.Open(p)
			if err != nil {
				continue
			}

			scanner := bufio.NewScanner(f)
			pastHeader := false
			var headerLines []string // buffer header lines until we see "UTC,Freq,Vpk"

			for scanner.Scan() {
				line := scanner.Text()

				if !pastHeader {
					if line == "UTC,Freq,Vpk" {
						pastHeader = true
						if firstFile {
							// Emit buffered header + column line.
							for _, hl := range headerLines {
								fmt.Fprintln(w, hl)
							}
							fmt.Fprintln(w, line)
							firstFile = false
						}
						// Subsequent files: skip their header entirely.
					} else {
						if firstFile {
							headerLines = append(headerLines, line)
						}
					}
					continue
				}

				// Data row handling.
				if fullDay {
					fmt.Fprintln(w, line)
					continue
				}

				// Range filtering: parse the HH:MM:SS field and combine with
				// the file's UTC date to get a full timestamp for comparison.
				ts, ok := csvRowTime(line, fileDay)
				if !ok {
					continue // malformed row — skip
				}
				if (ts.Equal(rangeStart) || ts.After(rangeStart)) && ts.Before(rangeEnd) {
					fmt.Fprintln(w, line)
				}
			}
			f.Close()
		}
	})

	// ── Audio info ─────────────────────────────────────────────────────────
	// GET /api/audio/info?station=<label>
	// Returns the sample rate and dial frequency for the audio preview stream.
	// The dial frequency is carrier - 1000 Hz (USB mode); the carrier tone
	// appears at 1000 Hz in the audio passband.
	mux.HandleFunc("/api/audio/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		label := r.URL.Query().Get("station")
		if label == "" {
			http.Error(w, "station parameter required", http.StatusBadRequest)
			return
		}
		var target *DopplerStation
		for _, ds := range mgr.list() {
			if ds.cfg.Label == label {
				target = ds
				break
			}
		}
		if target == nil {
			http.Error(w, "station not found", http.StatusNotFound)
			return
		}
		target.streamMu.RLock()
		sr := target.streamSampleRate
		target.streamMu.RUnlock()
		if sr == 0 {
			sr = 12000
		}
		dialFreq := target.cfg.FreqHz - 1000
		jsonResponse(w, map[string]interface{}{
			"sample_rate":     sr,
			"dial_freq_hz":    dialFreq,
			"carrier_freq_hz": target.cfg.FreqHz,
			"label":           label,
		})
	})

	// ── Audio preview ──────────────────────────────────────────────────────
	// GET /api/audio/preview?station=<label>
	// Streams a live WAV audio preview of the station's carrier frequency.
	// The audio connection is established on demand and dropped when the
	// client disconnects. Useful for verifying the carrier is receivable.
	mux.HandleFunc("/api/audio/preview", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		label := r.URL.Query().Get("station")
		if label == "" {
			http.Error(w, "station parameter required", http.StatusBadRequest)
			return
		}
		var target *DopplerStation
		for _, ds := range mgr.list() {
			if ds.cfg.Label == label {
				target = ds
				break
			}
		}
		if target == nil {
			http.Error(w, "station not found", http.StatusNotFound)
			return
		}

		// Get sample rate (default 12000 if not yet known)
		target.streamMu.RLock()
		sr := target.streamSampleRate
		target.streamMu.RUnlock()
		if sr == 0 {
			sr = 12000
		}

		writeStreamingWAVHeader(w, sr, 1)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}

		audioCh := target.audioHub.subscribe()
		defer target.audioHub.unsubscribe(audioCh)

		for {
			select {
			case <-r.Context().Done():
				return
			case chunk, ok := <-audioCh:
				if !ok {
					return
				}
				if _, err := w.Write(chunk); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	})

	// ── Receiver description proxy ─────────────────────────────────────────
	// GET /api/description — proxies UberSDR's /api/description and caches the
	// result so the frontend can call BASE+/api/description regardless of whether
	// it is running behind the addon proxy (where window.location.origin would
	// point to the doppler addon, not the UberSDR root).
	mux.HandleFunc("/api/description", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Try to refresh the cache on every request (fast — UberSDR is local).
		// If the refresh fails, serve the stale cached value.
		if desc, err := fetchReceiverDescription(ubersdrHTTPBase); err == nil {
			descCache.Store(desc)
		}
		desc, ok := descCache.Load()
		if !ok {
			http.Error(w, "receiver description not yet available", http.StatusServiceUnavailable)
			return
		}
		jsonResponse(w, desc)
	})

	// ── FTP settings endpoints (all auth-gated) ────────────────────────────
	registerFTPHandlers(mux, ftpCfg, ftpMu, ftpCfgPath, uiPassword, sessions)

	log.Printf("[web] listening on %s (write actions: %s)", addr, func() string {
		if uiPassword == "" {
			return "disabled — set UI_PASSWORD"
		}
		return "password protected"
	}())
	return http.ListenAndServe(addr, mux)
}

// jsonResponse writes v as JSON with Content-Type application/json.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[web] json encode: %v", err)
	}
}

// ---------------------------------------------------------------------------
// WAV streaming helpers (for live audio preview)
// ---------------------------------------------------------------------------

// writeStreamingWAVHeader writes a streaming WAV header with a near-infinite
// data size so the browser can play it as a live stream.
func writeStreamingWAVHeader(w http.ResponseWriter, sampleRate, channels int) {
	const maxSize = 0x7FFFFFFF
	bitsPerSample := 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := uint32(maxSize - 36)

	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(maxSize))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1) // PCM
	binary.LittleEndian.PutUint16(hdr[22:], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:], uint16(bitsPerSample))
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:], dataSize)

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(hdr)
}

// ---------------------------------------------------------------------------
// CSV time-range helpers
// ---------------------------------------------------------------------------

// parseLastDuration parses a "last" query parameter value into a time.Duration.
// Accepted formats: <N>s (seconds), <N>m (minutes), <N>h (hours).
// Examples: "15m", "1h", "3600s".
func parseLastDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("must be a number followed by s, m, or h (e.g. 15m)")
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive integer followed by s, m, or h (e.g. 15m)")
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown unit %q — use s, m, or h", string(unit))
	}
}

// csvFileDay extracts the UTC date from a CSV file path.
// The path is expected to contain .../YYYY/MM/DD/... directory components.
// Returns the zero time if the path cannot be parsed.
func csvFileDay(path string) time.Time {
	parts := strings.Split(filepath.ToSlash(path), "/")
	// Walk backwards looking for DD, MM, YYYY components.
	for i := len(parts) - 2; i >= 2; i-- {
		dd, err1 := strconv.Atoi(parts[i])
		mm, err2 := strconv.Atoi(parts[i-1])
		yyyy, err3 := strconv.Atoi(parts[i-2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if yyyy < 2000 || yyyy > 2100 || mm < 1 || mm > 12 || dd < 1 || dd > 31 {
			continue
		}
		return time.Date(yyyy, time.Month(mm), dd, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

// csvRowTime parses the UTC timestamp from a Grape CSV data row.
// Supports two formats:
//   - New format (RFC3339):  "2006-01-02T15:04:05Z, freq, vpk"
//   - Old format (HH:MM:SS): "15:04:05, freq, vpk" — combined with fileDay
//
// Returns (time, true) on success, (zero, false) if the row is malformed.
func csvRowTime(row string, fileDay time.Time) (time.Time, bool) {
	// The UTC field is the first comma-separated token; trim whitespace.
	idx := strings.IndexByte(row, ',')
	if idx < 0 {
		return time.Time{}, false
	}
	field := strings.TrimSpace(row[:idx])

	// Try RFC3339 first (new format).
	if t, err := time.Parse(time.RFC3339, field); err == nil {
		return t.UTC(), true
	}

	// Fall back to legacy HH:MM:SS format — requires fileDay.
	if fileDay.IsZero() {
		return time.Time{}, false
	}
	t, err := time.Parse("15:04:05", field)
	if err != nil {
		return time.Time{}, false
	}
	full := time.Date(fileDay.Year(), fileDay.Month(), fileDay.Day(),
		t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	return full, true
}
