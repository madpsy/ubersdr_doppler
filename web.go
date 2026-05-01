// web.go — HTTP server, SSE live feed, REST API for ubersdr_doppler
package main

import (
	"crypto/rand"
	"embed"
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

type sseClient struct {
	ch    chan string
	label string // "" = subscribe to all stations
}

type sseHub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[*sseClient]struct{})}
}

func (h *sseHub) subscribe(label string) *sseClient {
	c := &sseClient{ch: make(chan string, 64), label: label}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *sseHub) unsubscribe(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.ch)
}

// broadcast sends a live reading to all subscribed SSE clients.
func (h *sseHub) broadcast(label string, r DopplerReading) {
	type payload struct {
		Station string         `json:"station"`
		Reading DopplerReading `json:"reading"`
	}
	data, err := json.Marshal(payload{Station: label, Reading: r})
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

// broadcastSpectrum sends a spectrum update to all subscribed SSE clients.
// Uses a named "spectrum" event so the client can handle it separately from
// the default onmessage (Doppler reading) handler.
func (h *sseHub) broadcastSpectrum(label string, bins []float32, peakBin int, binBW float64) {
	type payload struct {
		Station      string    `json:"station"`
		SpectrumData []float32 `json:"spectrum_data"`
		PeakBin      int       `json:"peak_bin"`
		BinBandwidth float64   `json:"bin_bandwidth"`
	}
	data, err := json.Marshal(payload{
		Station:      label,
		SpectrumData: bins,
		PeakBin:      peakBin,
		BinBandwidth: binBW,
	})
	if err != nil {
		return
	}
	msg := "event: spectrum\ndata: " + string(data) + "\n\n"

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
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		client := hub.subscribe(label)
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
			log.Printf("[web] settings updated: freq_ref=%s callsign=%s grid=%s", s.FrequencyReference, s.Callsign, s.Grid)
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
				jsonResponse(w, history)
			} else {
				jsonResponse(w, ds.History())
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
	mux.HandleFunc("/api/csv", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		label := r.URL.Query().Get("station")
		date := r.URL.Query().Get("date")
		if label == "" || date == "" {
			http.Error(w, "station and date parameters required", http.StatusBadRequest)
			return
		}
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			http.Error(w, "invalid date format (use YYYY-MM-DD)", http.StatusBadRequest)
			return
		}
		if strings.ContainsAny(label, "/\\.") {
			http.Error(w, "invalid station label", http.StatusBadRequest)
			return
		}
		// Find the Grape-format filename for this station/date
		dir := filepath.Join(mgr.dataDir,
			fmt.Sprintf("%04d", t.Year()),
			fmt.Sprintf("%02d", t.Month()),
			fmt.Sprintf("%02d", t.Day()),
		)
		// List files in the directory matching the station label
		entries, err := os.ReadDir(dir)
		if err != nil {
			http.Error(w, "no data for that station/date", http.StatusNotFound)
			return
		}
		var matchPath string
		for _, e := range entries {
			if strings.Contains(e.Name(), "_FRQ_"+label+".csv") {
				matchPath = filepath.Join(dir, e.Name())
				break
			}
		}
		if matchPath == "" {
			http.Error(w, "no data for that station/date", http.StatusNotFound)
			return
		}
		f, err := os.Open(matchPath)
		if err != nil {
			http.Error(w, "no data for that station/date", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(matchPath)))
		http.ServeContent(w, r, filepath.Base(matchPath), t, f)
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
