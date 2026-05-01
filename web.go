// web.go — HTTP server, SSE live feed, REST API for ubersdr_doppler
package main

import (
	"embed"
	"encoding/json"
	"fmt"
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
	c := &sseClient{ch: make(chan string, 32), label: label}
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
		Station string        `json:"station"`
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
) error {
	mux := http.NewServeMux()

	// Static files (embedded).
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

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
				fmt.Fprint(w, ": heartbeat\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// ── Global settings ────────────────────────────────────────────────────
	// GET /api/settings — return current global settings
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			settingsMu.RLock()
			s := *settings
			settingsMu.RUnlock()
			jsonResponse(w, s)

		case http.MethodPost:
			var s globalSettings
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			// Validate frequency_reference value
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
			// Persist to disk
			if err := saveGlobalSettings(settingsPath, s); err != nil {
				log.Printf("[web] save settings: %v", err)
				http.Error(w, "failed to save settings", http.StatusInternalServerError)
				return
			}
			// Update CSV writer so new rows use the updated settings
			cw.UpdateSettings(s)
			log.Printf("[web] settings updated: freq_ref=%s callsign=%s grid=%s", s.FrequencyReference, s.Callsign, s.Grid)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// ── Station list ───────────────────────────────────────────────────────
	// GET /api/stations — list all stations with current reading, 1-hour baseline,
	// and reference correction (if a reference station is configured).
	mux.HandleFunc("/api/stations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		type stationStatus struct {
			Config           stationConfig  `json:"config"`
			Current          DopplerReading `json:"current"`
			BaselineMean     *float64       `json:"baseline_mean_hz"`     // 1-hour mean; nil if insufficient data
			BaselineN        int            `json:"baseline_n"`           // number of minute-means in baseline
			CorrectedDoppler *float64       `json:"corrected_doppler_hz"` // current - reference; nil if no reference
		}
		refHz, refValid := mgr.referenceCorrection()
		stations := mgr.list()
		out := make([]stationStatus, 0, len(stations))
		for _, ds := range stations {
			mean, n := ds.BaselineMean(60)
			var meanPtr *float64
			if n >= 5 {
				meanPtr = &mean
			}
			cur := ds.CurrentReading()
			var corrPtr *float64
			if refValid && cur.Valid && !ds.cfg.IsReference {
				corr := cur.DopplerHz - refHz
				corrPtr = &corr
			}
			out = append(out, stationStatus{
				Config:           ds.cfg,
				Current:          cur,
				BaselineMean:     meanPtr,
				BaselineN:        n,
				CorrectedDoppler: corrPtr,
			})
		}
		jsonResponse(w, out)
	})

	// ── History ────────────────────────────────────────────────────────────
	// GET /api/history?station=<label>
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
		for _, ds := range mgr.list() {
			if ds.cfg.Label == label {
				jsonResponse(w, ds.History())
				return
			}
		}
		http.Error(w, "station not found", http.StatusNotFound)
	})

	// ── Station CRUD ───────────────────────────────────────────────────────
	// POST /api/stations/add
	mux.HandleFunc("/api/stations/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	// POST /api/stations/remove  {"label":"WWV-10"}
	mux.HandleFunc("/api/stations/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	// POST /api/stations/update  — full stationConfig body (id required)
	mux.HandleFunc("/api/stations/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		path := filepath.Join(mgr.dataDir,
			fmt.Sprintf("%04d", t.Year()),
			fmt.Sprintf("%02d", t.Month()),
			fmt.Sprintf("%02d", t.Day()),
			label+".csv",
		)
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, "no data for that station/date", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_%s.csv"`, label, date))
		http.ServeContent(w, r, label+".csv", t, f)
	})

	log.Printf("[web] listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

// jsonResponse writes v as JSON with Content-Type application/json.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[web] json encode: %v", err)
	}
}
