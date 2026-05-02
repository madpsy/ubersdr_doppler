// doppler.go — DopplerStation and stationManager
//
// Each DopplerStation opens TWO connections to UberSDR:
//
//  1. SPECTRUM connection (mode=spectrum) — receives pre-computed FFT bins from
//     radiod via the SPEC binary protocol. The carrier appears as a sharp spike
//     in the spectrum. Peak detection gives sub-Hz Doppler precision with no
//     demodulation artifacts.
//
//  2. AUDIO connection (mode=am) — receives demodulated PCM audio. Used only
//     for the live audio preview feature (streaming WAV to the browser). Not
//     used for Doppler measurement.
//
// The spectrum connection uses 200 bins × 0.5 Hz/bin = 100 Hz window centred
// on the carrier frequency. Sub-bin frequency estimation uses parabolic
// interpolation (always applied) blended with a power-weighted centroid for
// wider signals, giving effective precision of ~0.01 Hz or better.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// DopplerReading — one instantaneous measurement
// ---------------------------------------------------------------------------

// DopplerReading is a single 1-second Doppler measurement for one station.
type DopplerReading struct {
	Timestamp          time.Time `json:"timestamp"`
	DopplerHz          float64   `json:"doppler_hz"`                     // measured carrier offset from nominal (Hz)
	CorrectedDopplerHz *float64  `json:"corrected_doppler_hz,omitempty"` // nil if no reference station
	SNR                float32   `json:"snr_db"`                         // signal-to-noise ratio (dB)
	SignalDBFS         float32   `json:"signal_dbfs"`                    // peak signal power (dBFS)
	NoiseDBFS          float32   `json:"noise_dbfs"`                     // noise floor (dBFS)
	Valid              bool      `json:"valid"`                          // false if SNR < minSNR or no signal
}

// MinuteMean is the 1-minute mean of valid DopplerReadings.
type MinuteMean struct {
	Timestamp          time.Time `json:"timestamp"`
	DopplerHz          float64   `json:"doppler_hz"`
	CorrectedDopplerHz *float64  `json:"corrected_doppler_hz,omitempty"` // nil if no reference station
	MinDopplerHz       float64   `json:"min_doppler_hz"`                 // minimum raw Doppler in the minute
	MaxDopplerHz       float64   `json:"max_doppler_hz"`                 // maximum raw Doppler in the minute
	StdDevHz           float64   `json:"std_dev_hz"`                     // standard deviation of raw Doppler (jitter)
	SNR                float32   `json:"snr_db"`
	SignalDBFS         float32   `json:"signal_dbfs"`
	NoiseDBFS          float32   `json:"noise_dbfs"`
	Count              int       `json:"count"` // number of valid samples averaged
}

// ---------------------------------------------------------------------------
// SPEC binary protocol constants
// ---------------------------------------------------------------------------

const (
	specMagic0 = 0x53 // 'S'
	specMagic1 = 0x50 // 'P'
	specMagic2 = 0x45 // 'E'
	specMagic3 = 0x43 // 'C'

	specHeaderSize = 22

	specFlagFullFloat32  = 0x01 // full frame, float32 bins
	specFlagDeltaFloat32 = 0x02 // delta frame, float32 changes
	specFlagFullUint8    = 0x03 // full frame, uint8 bins
	specFlagDeltaUint8   = 0x04 // delta frame, uint8 changes
)

// ---------------------------------------------------------------------------
// spectrumDecoder — maintains state for SPEC binary protocol decoding
// ---------------------------------------------------------------------------

type spectrumDecoder struct {
	bins []float32 // current full spectrum (updated by full and delta frames)
}

func newSpectrumDecoder(binCount int) *spectrumDecoder {
	return &spectrumDecoder{bins: make([]float32, binCount)}
}

// decode parses a SPEC binary frame and updates the internal bin state.
// Returns the updated bins slice (same backing array) and true on success.
func (d *spectrumDecoder) decode(data []byte) ([]float32, bool) {
	if len(data) < specHeaderSize {
		return nil, false
	}
	// Validate magic
	if data[0] != specMagic0 || data[1] != specMagic1 ||
		data[2] != specMagic2 || data[3] != specMagic3 {
		return nil, false
	}
	// version := data[4]  // currently 0x01
	flags := data[5]
	// timestamp := binary.LittleEndian.Uint64(data[6:14])  // ms
	// frequency := binary.LittleEndian.Uint64(data[14:22]) // Hz

	payload := data[specHeaderSize:]

	switch flags {
	case specFlagFullFloat32:
		// Full frame: payload is binCount × float32 (little-endian)
		n := len(payload) / 4
		if n == 0 {
			return nil, false
		}
		if len(d.bins) != n {
			d.bins = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			bits := binary.LittleEndian.Uint32(payload[i*4 : i*4+4])
			d.bins[i] = math.Float32frombits(bits)
		}

	case specFlagDeltaFloat32:
		// Delta frame: uint16 changeCount + [uint16 index, float32 value] pairs
		if len(payload) < 2 {
			return nil, false
		}
		changeCount := int(binary.LittleEndian.Uint16(payload[0:2]))
		payload = payload[2:]
		if len(payload) < changeCount*6 {
			return nil, false
		}
		for i := 0; i < changeCount; i++ {
			idx := int(binary.LittleEndian.Uint16(payload[i*6 : i*6+2]))
			bits := binary.LittleEndian.Uint32(payload[i*6+2 : i*6+6])
			if idx < len(d.bins) {
				d.bins[idx] = math.Float32frombits(bits)
			}
		}

	case specFlagFullUint8:
		// Full frame: payload is binCount × uint8
		// uint8 encoding: 0 = -256 dBFS, 255 = -1 dBFS → dBFS = uint8 - 256
		n := len(payload)
		if n == 0 {
			return nil, false
		}
		if len(d.bins) != n {
			d.bins = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			d.bins[i] = float32(int(payload[i]) - 256)
		}

	case specFlagDeltaUint8:
		// Delta frame: uint16 changeCount + [uint16 index, uint8 value] pairs
		if len(payload) < 2 {
			return nil, false
		}
		changeCount := int(binary.LittleEndian.Uint16(payload[0:2]))
		payload = payload[2:]
		if len(payload) < changeCount*3 {
			return nil, false
		}
		for i := 0; i < changeCount; i++ {
			idx := int(binary.LittleEndian.Uint16(payload[i*3 : i*3+2]))
			val := float32(int(payload[i*3+2]) - 256)
			if idx < len(d.bins) {
				d.bins[idx] = val
			}
		}

	default:
		return nil, false
	}

	return d.bins, true
}

// ---------------------------------------------------------------------------
// audioBroadcastHub — fan-out of raw PCM chunks to preview listeners
// ---------------------------------------------------------------------------

type audioBroadcastHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	// drainCh is closed (and replaced) whenever the last listener unsubscribes.
	// runAudioLoop selects on this to disconnect the WebSocket immediately
	// rather than waiting for the 500 ms poll interval.
	drainCh chan struct{}
}

func newAudioBroadcastHub() *audioBroadcastHub {
	return &audioBroadcastHub{
		clients: make(map[chan []byte]struct{}),
		drainCh: make(chan struct{}),
	}
}

func (h *audioBroadcastHub) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *audioBroadcastHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	empty := len(h.clients) == 0
	drainCh := h.drainCh
	if empty {
		// Replace drainCh so the next subscribe/unsubscribe cycle gets a fresh channel.
		h.drainCh = make(chan struct{})
	}
	h.mu.Unlock()
	close(ch)
	if empty {
		// Signal runAudioLoop to disconnect immediately.
		close(drainCh)
	}
}

// drained returns a channel that is closed when the last listener unsubscribes.
// The caller must re-read this after each subscribe/unsubscribe cycle because
// the channel is replaced on every drain event.
func (h *audioBroadcastHub) drained() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.drainCh
}

func (h *audioBroadcastHub) broadcast(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	buf := make([]byte, len(pcm))
	copy(buf, pcm)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- buf:
		default:
			// Slow client — drop frame.
		}
	}
}

func (h *audioBroadcastHub) hasListeners() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// ---------------------------------------------------------------------------
// DopplerStation — one monitored time/frequency station
// ---------------------------------------------------------------------------

const (
	// Spectrum channel parameters — 200 bins × 0.5 Hz/bin = 100 Hz window.
	// 0.5 Hz/bin gives ~0.01 Hz centroid precision at SNR 20 dB (4× better than 2 Hz/bin).
	// radiod FFT mode: fft_size ≈ 1500 (goodchoice, samprate = 750 Hz), frame time ≈ 2 s.
	// Measurements update every ~2 seconds; minute means still get ~30 samples.
	// We always read the actual binBandwidth back from the server's "config"
	// message in case it snaps to a different value.
	specBinCount     = 200
	specBinBandwidth = 0.5 // Hz/bin — 4× finer than UberSDR FrequencyReferenceMonitor default

	// History depth for minute-means (24 hours × 60 minutes).
	historyDepth = 24 * 60

	// signalRecoveryDuration is the minimum continuous valid-signal duration
	// required before a station exits the no-signal state.  This prevents
	// brief SNR spikes from prematurely clearing a no-signal condition.
	signalRecoveryDuration = 60 * time.Second
)

// DopplerStation monitors one standard time/frequency station.
type DopplerStation struct {
	cfg stationConfig

	ubersdrURL       string
	historyDataDir   string // base data directory for per-date history files (empty = no persistence)
	historySafeLabel string // sanitised label safe for use in filenames
	minSNR           float64
	maxDriftHz       float64

	hub       *sseHub
	csvWriter *csvWriter

	// refProvider returns the current reference station clock error in ppm
	// (dopplerHz / refFreqHz * 1e6) and whether it is valid.
	// Callers must scale by their own station frequency: corrected = raw - ppm*stationFreq/1e6
	// Set by stationManager after creation.
	refProvider func() (ppm float64, valid bool)

	// audioHub fans out PCM audio to preview listeners.
	audioHub *audioBroadcastHub

	// sessionID is the active user_session_id shared between the spectrum and
	// audio WebSocket connections. Set by runSpectrumLoop after a successful
	// /connection call; read by runAudioLoop so both connections share the same
	// UUID (the server links them by user_session_id).
	sessionMu sync.RWMutex
	sessionID string

	// streamSampleRate is set from the first audio packet header.
	streamMu         sync.RWMutex
	streamSampleRate int

	// Measurement state — protected by mu.
	mu          sync.RWMutex
	current     DopplerReading
	history     []MinuteMean
	latestBins  []float32 // latest unwrapped spectrum bins for display
	latestPeak  int       // peak bin index in latestBins (-1 if no valid signal)
	latestBinBW float64   // actual bin bandwidth (Hz) reported by server

	// Signal-recovery debounce: once a station drops to no-signal it must
	// sustain a continuously valid signal for signalRecoveryDuration before
	// Valid is reported as true again.  Protected by mu.
	noSignal    bool      // true while in the debounced no-signal state
	signalSince time.Time // wall-clock time the current valid-signal run started

	// 1-second sample accumulator for minute-mean calculation.
	sampleMu sync.Mutex
	samples  []DopplerReading
}

// newDopplerStation creates a DopplerStation from a stationConfig.
// dataDir is the data directory for history persistence; pass "" to disable.
func newDopplerStation(cfg stationConfig, ubersdrURL, dataDir string, hub *sseHub, cw *csvWriter) *DopplerStation {
	safe := ""
	if dataDir != "" {
		// Sanitise label for use as a filename component
		safe = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, cfg.Label)
	}
	return &DopplerStation{
		cfg:              cfg,
		ubersdrURL:       ubersdrURL,
		historyDataDir:   dataDir,
		historySafeLabel: safe,
		minSNR:           cfg.MinSNR,
		maxDriftHz:       cfg.MaxDriftHz,
		hub:              hub,
		csvWriter:        cw,
		audioHub:         newAudioBroadcastHub(),
	}
}

// historyDayPath returns the path to the history JSON file for a given UTC day.
// Format: <dataDir>/YYYY/MM/DD/history-<label>.json
func (ds *DopplerStation) historyDayPath(t time.Time) string {
	if ds.historyDataDir == "" || ds.historySafeLabel == "" {
		return ""
	}
	d := t.UTC()
	return fmt.Sprintf("%s/%04d/%02d/%02d/history-%s.json",
		ds.historyDataDir, d.Year(), d.Month(), d.Day(), ds.historySafeLabel)
}

// HistoryForDate reads the history JSON file for a specific UTC day from disk.
// Returns nil if the file doesn't exist or can't be parsed.
func (ds *DopplerStation) HistoryForDate(date time.Time) []MinuteMean {
	path := ds.historyDayPath(date)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[%s] history: read %s: %v", ds.cfg.Label, path, err)
		}
		return nil
	}
	var h []MinuteMean
	if err := json.Unmarshal(data, &h); err != nil {
		log.Printf("[%s] history: parse %s: %v", ds.cfg.Label, path, err)
		return nil
	}
	return h
}

// saveHistory rewrites today's history day-file with all entries for today.
// Must be called with ds.mu held.
func (ds *DopplerStation) saveHistory() {
	if ds.historyDataDir == "" || len(ds.history) == 0 {
		return
	}
	// Determine today's UTC date from the last entry
	today := ds.history[len(ds.history)-1].Timestamp.UTC()
	path := ds.historyDayPath(today)
	if path == "" {
		return
	}
	// Collect only today's entries
	dayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	var todayEntries []MinuteMean
	for _, m := range ds.history {
		if !m.Timestamp.Before(dayStart) && m.Timestamp.Before(dayEnd) {
			todayEntries = append(todayEntries, m)
		}
	}
	if len(todayEntries) == 0 {
		return
	}
	// Ensure directory exists
	dir := fmt.Sprintf("%s/%04d/%02d/%02d",
		ds.historyDataDir, today.Year(), today.Month(), today.Day())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[%s] history: mkdir %s: %v", ds.cfg.Label, dir, err)
		return
	}
	data, err := json.Marshal(todayEntries)
	if err != nil {
		log.Printf("[%s] history: marshal error: %v", ds.cfg.Label, err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("[%s] history: write error: %v", ds.cfg.Label, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[%s] history: rename error: %v", ds.cfg.Label, err)
	}
}

// loadHistoryFromDisk loads the last 24 hours of history from per-date JSON files.
// Reads today's and yesterday's files to cover the rolling 24h window.
// Safe to call before the station goroutine starts.
func (ds *DopplerStation) loadHistoryFromDisk() {
	if ds.historyDataDir == "" {
		return
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(historyDepth) * time.Minute)

	var combined []MinuteMean
	// Load yesterday and today to cover the 24h window across midnight
	for _, d := range []time.Time{now.Add(-24 * time.Hour), now} {
		entries := ds.HistoryForDate(d)
		combined = append(combined, entries...)
	}
	// Filter to the rolling window and deduplicate by timestamp
	seen := make(map[time.Time]bool)
	var filtered []MinuteMean
	for _, m := range combined {
		if m.Timestamp.Before(cutoff) || seen[m.Timestamp] {
			continue
		}
		seen[m.Timestamp] = true
		filtered = append(filtered, m)
	}
	if len(filtered) == 0 {
		return
	}
	ds.mu.Lock()
	ds.history = filtered
	ds.mu.Unlock()
	log.Printf("[%s] history: loaded %d minute-means from disk", ds.cfg.Label, len(filtered))
}

// httpBaseURL converts any UberSDR URL (ws/wss/http/https) to an HTTP base URL.
func httpBaseURL(rawURL string) string {
	u, _ := url.Parse(rawURL)
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	path := strings.TrimRight(u.Path, "/")
	// Strip a bare "/ws" path — the base URL should not include it.
	if path == "/ws" {
		path = ""
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, path)
}

// spectrumWSURL builds the WebSocket URL for the /ws/user-spectrum endpoint.
// The server only accepts user_session_id as a query parameter; all other
// spectrum parameters (frequency, bin_count, bin_bandwidth) are sent as JSON
// messages after the connection is established.
func (ds *DopplerStation) spectrumWSURL(sessionID string) string {
	u, _ := url.Parse(ds.ubersdrURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}
	// Strip any existing path suffix so we always hit /ws/user-spectrum.
	host := u.Host
	q := url.Values{}
	q.Set("user_session_id", sessionID)
	return fmt.Sprintf("%s://%s/ws/user-spectrum?%s", wsScheme, host, q.Encode())
}

// audioWSURL builds the WebSocket URL for the /ws audio endpoint.
// dialFreqHz overrides the frequency used in the URL; pass 0 to use the
// station's nominal carrier frequency.
func (ds *DopplerStation) audioWSURL(mode string, extraParams url.Values, sessionID string, dialFreqHz int) string {
	u, _ := url.Parse(ds.ubersdrURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}
	freq := ds.cfg.FreqHz
	if dialFreqHz > 0 {
		freq = dialFreqHz
	}
	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", freq))
	q.Set("mode", mode)
	q.Set("user_session_id", sessionID)
	for k, vs := range extraParams {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	return fmt.Sprintf("%s://%s%s?%s", wsScheme, u.Host, path, q.Encode())
}

// checkConnection registers a session with UberSDR's /connection endpoint.
// A non-empty User-Agent header is required: the server stores it and later
// checks that it is present before allowing the WebSocket upgrade.
func (ds *DopplerStation) checkConnection(sessionID string) error {
	base := httpBaseURL(ds.ubersdrURL)
	endpoint := base + "/connection"
	body := fmt.Sprintf(`{"user_session_id":"%s"}`, sessionID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ubersdr_doppler/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("connection rejected (password required or IP banned)")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("connection rejected (server full)")
	default:
		return fmt.Errorf("connection check returned HTTP %d", resp.StatusCode)
	}
}

// run starts both the spectrum measurement loop and the audio loop.
// Blocks until ctx is cancelled.
func (ds *DopplerStation) run(ctx context.Context) {
	log.Printf("[%s] starting — carrier %d Hz, min SNR %.1f dB, max drift ±%.1f Hz",
		ds.cfg.Label, ds.cfg.FreqHz, ds.minSNR, ds.maxDriftHz)

	var wg sync.WaitGroup

	// Spectrum loop — for Doppler measurement
	wg.Add(1)
	go func() {
		defer wg.Done()
		ds.runSpectrumLoop(ctx)
	}()

	// Audio loop — for preview only (only connects when there are listeners)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ds.runAudioLoop(ctx)
	}()

	wg.Wait()
}

// runSpectrumLoop connects to UberSDR's spectrum WebSocket and measures Doppler.
func (ds *DopplerStation) runSpectrumLoop(ctx context.Context) {
	dec := newSpectrumDecoder(specBinCount)

	measureTicker := time.NewTicker(1 * time.Second)
	defer measureTicker.Stop()
	minuteTicker := time.NewTicker(1 * time.Minute)
	defer minuteTicker.Stop()

	// Latest spectrum bins and the actual bin bandwidth reported by the server
	// (updated by the WebSocket goroutine; read by the measurement ticker).
	var specMu sync.Mutex
	var latestBins []float32
	actualBinBW := specBinBandwidth // updated from server "config" message

	// Rate-limit spectrum SSE broadcasts to at most 10 Hz.
	var lastSpecBroadcast time.Time
	const specBroadcastInterval = 100 * time.Millisecond

	// WebSocket reconnect loop
	connCh := make(chan struct{}, 1)
	connCh <- struct{}{} // trigger first connect

	var wsConn *websocket.Conn
	var wsConnMu sync.Mutex

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-connCh:
			}

			sessionID := uuid.New().String()
			if err := ds.checkConnection(sessionID); err != nil {
				log.Printf("[%s] spectrum: connection check failed: %v — retrying in 10s", ds.cfg.Label, err)
				time.Sleep(10 * time.Second)
				select {
				case connCh <- struct{}{}:
				default:
				}
				continue
			}
			// Store the active session UUID so the audio loop can reuse it,
			// linking both connections under the same user_session_id.
			ds.sessionMu.Lock()
			ds.sessionID = sessionID
			ds.sessionMu.Unlock()

			wsAddr := ds.spectrumWSURL(sessionID)

			hdr := http.Header{}
			hdr.Set("User-Agent", "ubersdr_doppler/1.0")
			conn, _, err := wsDialer.Dial(wsAddr, hdr)
			if err != nil {
				log.Printf("[%s] spectrum: dial failed: %v — retrying in 10s", ds.cfg.Label, err)
				time.Sleep(10 * time.Second)
				select {
				case connCh <- struct{}{}:
				default:
				}
				continue
			}

			wsConnMu.Lock()
			wsConn = conn
			wsConnMu.Unlock()

			log.Printf("[%s] spectrum connected", ds.cfg.Label)

			// Send zoom/pan config to request our desired window:
			// 500 bins × 2 Hz = 1 kHz centred on the carrier frequency.
			// The server ignores unknown fields so this is safe to send immediately.
			if err := conn.WriteJSON(map[string]interface{}{
				"type":         "zoom",
				"frequency":    ds.cfg.FreqHz,
				"binBandwidth": specBinBandwidth,
			}); err != nil {
				log.Printf("[%s] spectrum: failed to send zoom config: %v — reconnecting", ds.cfg.Label, err)
				conn.Close()
				time.Sleep(5 * time.Second)
				select {
				case connCh <- struct{}{}:
				default:
				}
				continue
			}

			// Keepalive
			localCtx, localCancel := context.WithCancel(ctx)
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-localCtx.Done():
						return
					case <-ticker.C:
						if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
							return
						}
					}
				}
			}()

			// Read loop — the server sends:
			//   • text JSON frames: "config" (session params), "pong", "error" — skip these
			//   • binary SPEC frames: spectrum data — decode with dec.decode()
			for {
				if ctx.Err() != nil {
					localCancel()
					conn.Close()
					return
				}
				msgType, msg, err := conn.ReadMessage()
				if err != nil {
					localCancel()
					conn.Close()
					log.Printf("[%s] spectrum: read error: %v — reconnecting", ds.cfg.Label, err)
					break
				}
				// Text (JSON) control messages from the server.
				// Parse "config" to learn the actual binBandwidth the server is using.
				if msgType != websocket.BinaryMessage {
					if msgType == websocket.TextMessage {
						var cfg struct {
							Type         string  `json:"type"`
							BinBandwidth float64 `json:"binBandwidth"`
						}
						if err2 := json.Unmarshal(msg, &cfg); err2 == nil &&
							cfg.Type == "config" && cfg.BinBandwidth > 0 {
							specMu.Lock()
							actualBinBW = cfg.BinBandwidth
							specMu.Unlock()
							log.Printf("[%s] spectrum: server binBandwidth=%.2f Hz", ds.cfg.Label, cfg.BinBandwidth)
						}
					}
					continue
				}
				bins, ok := dec.decode(msg)
				if !ok {
					continue
				}
				// Unwrap FFT: radiod sends [positive, negative]; we need [negative, positive]
				n := len(bins)
				half := n / 2
				unwrapped := make([]float32, n)
				copy(unwrapped[0:half], bins[half:n])
				copy(unwrapped[half:n], bins[0:half])

				specMu.Lock()
				latestBins = unwrapped
				bwNow := actualBinBW
				specMu.Unlock()

				// Broadcast spectrum to SSE clients at up to 10 Hz.
				now := time.Now()
				if now.Sub(lastSpecBroadcast) >= specBroadcastInterval {
					lastSpecBroadcast = now
					// Use the current peak bin from the last measurement (best effort).
					ds.mu.RLock()
					peakBin := ds.latestPeak
					ds.mu.RUnlock()
					// Send a copy so the goroutine doesn't race with the next decode.
					specCopy := make([]float32, len(unwrapped))
					copy(specCopy, unwrapped)
					ds.hub.broadcastSpectrum(ds.cfg.Label, specCopy, peakBin, bwNow)
				}
			}

			localCancel()
			time.Sleep(5 * time.Second)
			select {
			case connCh <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			wsConnMu.Lock()
			if wsConn != nil {
				wsConn.Close()
			}
			wsConnMu.Unlock()
			return

		case <-measureTicker.C:
			specMu.Lock()
			bins := latestBins
			binBW := actualBinBW
			specMu.Unlock()

			if len(bins) == 0 {
				continue
			}

			reading, peakBin := detectDopplerWithPeak(bins, binBW, ds.minSNR, ds.maxDriftHz)
			reading.Timestamp = time.Now().UTC()

			// ── Signal-recovery debounce ──────────────────────────────────────
			// Once a station drops to no-signal it must sustain a continuously
			// valid signal for signalRecoveryDuration (60 s) before Valid is
			// reported as true again.  This prevents brief SNR spikes from
			// prematurely clearing a no-signal condition.
			now := time.Now()
			ds.mu.Lock()
			if !reading.Valid {
				// Signal lost (or still absent): enter/stay in no-signal state
				// and reset the recovery timer.
				ds.noSignal = true
				ds.signalSince = time.Time{} // zero = no valid run in progress
			} else if ds.noSignal {
				// Raw signal is valid but we are still in the no-signal state.
				if ds.signalSince.IsZero() {
					// First valid reading after a no-signal period — start timer.
					ds.signalSince = now
				}
				if now.Sub(ds.signalSince) < signalRecoveryDuration {
					// Not yet recovered: suppress Valid so the station stays in
					// no-signal state for the broadcast and CSV/history.
					reading.Valid = false
				} else {
					// 60 s of continuous valid signal — exit no-signal state.
					ds.noSignal = false
					log.Printf("[%s] signal recovered after %.0f s", ds.cfg.Label,
						now.Sub(ds.signalSince).Seconds())
				}
			}
			// (If ds.noSignal is false and reading.Valid is true, the station
			// is already in a healthy state — no action needed.)

			// Store spectrum snapshot for the mini-spectrum display
			binsCopy := make([]float32, len(bins))
			copy(binsCopy, bins)

			ds.current = reading
			ds.latestBins = binsCopy
			ds.latestPeak = peakBin
			ds.latestBinBW = binBW
			ds.mu.Unlock()

			// Attach reference correction. Must be done before accumulating the
			// sample so that aggregateMinute() sees per-sample corrected values
			// and can compute a proper corrected mean/min/max rather than a
			// single instantaneous snapshot at aggregation time.
			if reading.Valid && ds.refProvider != nil && !ds.cfg.IsReference {
				if refPPM, ok := ds.refProvider(); ok {
					c := reading.DopplerHz - refPPM*float64(ds.cfg.FreqHz)/1e6
					reading.CorrectedDopplerHz = &c
				}
			}

			// Write 1-second reading to Grape CSV
			if reading.Valid {
				ds.csvWriter.writeReading(ds.cfg, reading)
			}

			// Accumulate for minute-mean (correction already attached above)
			if reading.Valid {
				ds.sampleMu.Lock()
				ds.samples = append(ds.samples, reading)
				ds.sampleMu.Unlock()
			}

			// Push live update to SSE clients
			ds.hub.broadcast(ds.cfg.Label, reading)

		case <-minuteTicker.C:
			ds.aggregateMinute()
		}
	}
}

// runAudioLoop connects to UberSDR's audio WebSocket for preview streaming.
// Only maintains the connection when there are active preview listeners.
// Disconnects immediately when the last listener unsubscribes (via audioHub.drained()).
func (ds *DopplerStation) runAudioLoop(ctx context.Context) {
	dec, err := newPCMDecoder()
	if err != nil {
		log.Printf("[%s] audio: decoder init failed: %v", ds.cfg.Label, err)
		return
	}
	defer dec.close()

	for {
		if ctx.Err() != nil {
			return
		}

		// Wait until there is at least one preview listener.
		// Poll with a short sleep; the drained() channel handles the fast-path
		// disconnect once we are connected.
		if !ds.audioHub.hasListeners() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		// Snapshot the drained channel before connecting so we can detect
		// the last-listener-gone event while the WebSocket is live.
		drainedCh := ds.audioHub.drained()

		// Reuse the spectrum session UUID so both connections share the same
		// user_session_id. If the spectrum loop hasn't connected yet, fall back
		// to a fresh UUID (the /connection call will register it).
		ds.sessionMu.RLock()
		sessionID := ds.sessionID
		ds.sessionMu.RUnlock()
		if sessionID == "" {
			sessionID = uuid.New().String()
		}
		if err := ds.checkConnection(sessionID); err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}

		audioParams := url.Values{}
		audioParams.Set("format", "pcm-zstd")
		audioParams.Set("version", "2")
		// USB mode with dial 1 kHz below the carrier so the carrier tone
		// appears at 1000 Hz in the audio passband (standard practice).
		// Filter: 300–1500 Hz passband centred on the 1 kHz tone.
		audioParams.Set("bandwidthLow", "300")
		audioParams.Set("bandwidthHigh", "1500")
		// Dial frequency = carrier - 1000 Hz
		dialFreq := ds.cfg.FreqHz - 1000
		wsAddr := ds.audioWSURL("usb", audioParams, sessionID, dialFreq)

		hdr := http.Header{}
		hdr.Set("User-Agent", "ubersdr_doppler/1.0")
		conn, _, err := wsDialer.Dial(wsAddr, hdr)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}

		log.Printf("[%s] audio preview connected", ds.cfg.Label)

		// connCtx is cancelled to stop the keepalive goroutine and the read
		// goroutine whenever we decide to close this connection.
		connCtx, connCancel := context.WithCancel(ctx)

		// Keepalive pings
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-connCtx.Done():
					return
				case <-ticker.C:
					conn.WriteJSON(map[string]string{"type": "ping"}) //nolint:errcheck
				}
			}
		}()

		// Read loop runs in its own goroutine so we can select on drainedCh
		// without blocking on conn.ReadMessage().
		type readResult struct {
			msgType int
			msg     []byte
			err     error
		}
		readCh := make(chan readResult, 4)
		go func() {
			for {
				mt, m, e := conn.ReadMessage()
				readCh <- readResult{mt, m, e}
				if e != nil {
					return
				}
			}
		}()

	readLoop:
		for {
			select {
			case <-ctx.Done():
				// Server shutting down — close and exit.
				connCancel()
				conn.Close()
				return

			case <-drainedCh:
				// Last listener disconnected — close WebSocket immediately.
				log.Printf("[%s] audio preview: no listeners, disconnecting", ds.cfg.Label)
				connCancel()
				conn.Close()
				break readLoop

			case res := <-readCh:
				if res.err != nil {
					connCancel()
					conn.Close()
					break readLoop
				}
				if res.msgType != websocket.BinaryMessage {
					continue
				}
				pkt, err := dec.decode(res.msg, true)
				if err != nil || len(pkt.pcm) == 0 {
					continue
				}
				// Update sample rate from full-header packets
				if pkt.sampleRate > 0 {
					ds.streamMu.Lock()
					ds.streamSampleRate = pkt.sampleRate
					ds.streamMu.Unlock()
				}
				ds.audioHub.broadcast(pkt.pcm)
			}
		}

		connCancel()
		// Brief pause before reconnecting (avoids hammering UberSDR on errors).
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// wsDialer is the gorilla WebSocket dialer (reused from ubersdr.go pattern).
var wsDialer = &websocket.Dialer{
	HandshakeTimeout: 10 * time.Second,
}

// ---------------------------------------------------------------------------
// detectDoppler — peak detection on unwrapped spectrum bins
// ---------------------------------------------------------------------------

// detectDoppler finds the carrier peak in the unwrapped spectrum and returns
// a DopplerReading. The spectrum must already be unwrapped (negative freqs
// first, then positive freqs). The returned DopplerHz is the offset from the
// centre bin (i.e. from the nominal carrier frequency).
// Uses the same algorithm as UberSDR's FrequencyReferenceMonitor:
//   - P5 noise floor (same as UberSDR)
//   - power-weighted centroid over the contiguous 3 dB range around the peak
//   - prefer peak near centre if within 30 dB of global max
func detectDoppler(bins []float32, binBandwidth, minSNR, maxDriftHz float64) DopplerReading {
	r, _ := detectDopplerWithPeak(bins, binBandwidth, minSNR, maxDriftHz)
	return r
}

// detectDopplerWithPeak is like detectDoppler but also returns the integer peak bin index.
// Returns -1 for peakBin when no valid signal is found.
func detectDopplerWithPeak(bins []float32, binBandwidth, minSNR, maxDriftHz float64) (DopplerReading, int) {
	n := len(bins)
	if n == 0 {
		return DopplerReading{}, -1
	}

	// Noise floor: P5 percentile (same as UberSDR)
	noiseFloor := percentileFloat32(bins, 5)

	// Search range: centre bin ± maxDriftHz
	centerBin := n / 2
	driftBins := int(maxDriftHz / binBandwidth)
	searchLow := centerBin - driftBins
	searchHigh := centerBin + driftBins
	if searchLow < 0 {
		searchLow = 0
	}
	if searchHigh >= n {
		searchHigh = n - 1
	}

	// Find global peak bin in search range
	peakBin := searchLow
	peakPower := bins[searchLow]
	for i := searchLow + 1; i <= searchHigh; i++ {
		if bins[i] > peakPower {
			peakPower = bins[i]
			peakBin = i
		}
	}

	snr := peakPower - noiseFloor
	if float64(snr) < minSNR {
		return DopplerReading{SNR: snr, SignalDBFS: peakPower, NoiseDBFS: noiseFloor, Valid: false}, -1
	}

	// Prefer peak near centre if within 30 dB of global max (same as UberSDR)
	centerRegionStart := centerBin - 5
	centerRegionEnd := centerBin + 5
	centerPeakPower := float32(-999)
	centerPeakBin := -1
	for i := centerRegionStart; i <= centerRegionEnd; i++ {
		if i >= 0 && i < n && bins[i] > centerPeakPower {
			centerPeakPower = bins[i]
			centerPeakBin = i
		}
	}
	if centerPeakBin >= 0 && (centerPeakPower-noiseFloor) >= float32(minSNR) &&
		(peakPower-centerPeakPower) <= 30.0 {
		peakBin = centerPeakBin
		peakPower = centerPeakPower
	}

	// Sub-bin frequency estimation: parabolic interpolation on the peak and its
	// two neighbours. This always gives true sub-bin precision for narrow CW
	// carriers regardless of how many bins fall within the 3 dB bandwidth.
	// For wider signals (multiple bins above threshold) we additionally compute
	// the power-weighted centroid and average the two estimates, which improves
	// accuracy when the signal spans several bins.
	var centroidBin float64

	// Step 1: parabolic interpolation (always applied)
	parabolicBin := float64(peakBin)
	if peakBin > 0 && peakBin < n-1 {
		alpha := float64(bins[peakBin-1])
		beta := float64(bins[peakBin])
		gamma := float64(bins[peakBin+1])
		denom := alpha - 2*beta + gamma
		if math.Abs(denom) > 0.001 {
			p := 0.5 * (alpha - gamma) / denom
			if p > 0.5 {
				p = 0.5
			} else if p < -0.5 {
				p = -0.5
			}
			parabolicBin = float64(peakBin) + p
		}
	}

	// Step 2: power-weighted centroid over contiguous 3 dB range
	threshold := peakPower - 3.0
	startBin := peakBin
	endBin := peakBin
	for i := peakBin - 1; i >= 0 && bins[i] >= threshold; i-- {
		startBin = i
	}
	for i := peakBin + 1; i < n && bins[i] >= threshold; i++ {
		endBin = i
	}

	var weightedSum, totalWeight float64
	for i := startBin; i <= endBin; i++ {
		linearPower := math.Pow(10.0, float64(bins[i])/10.0)
		weightedSum += float64(i) * linearPower
		totalWeight += linearPower
	}

	if totalWeight > 0 && (endBin-startBin) >= 2 {
		// Signal spans ≥3 bins: blend parabolic and centroid estimates equally.
		// The centroid is more accurate for wide signals; parabolic for narrow ones.
		centroidBin = 0.5*parabolicBin + 0.5*(weightedSum/totalWeight)
	} else {
		// Narrow signal (1–2 bins): parabolic interpolation is the best estimator.
		centroidBin = parabolicBin
	}

	// Offset from centre bin → Doppler Hz
	dopplerHz := (centroidBin - float64(n)/2.0) * binBandwidth

	// Final validation: reject if centroid drifted outside allowed range (same as UberSDR)
	if math.Abs(dopplerHz) > maxDriftHz {
		return DopplerReading{SNR: snr, SignalDBFS: peakPower, NoiseDBFS: noiseFloor, Valid: false}, -1
	}

	return DopplerReading{
		DopplerHz:  dopplerHz,
		SNR:        snr,
		SignalDBFS: peakPower,
		NoiseDBFS:  noiseFloor,
		Valid:      true,
	}, peakBin
}

// LatestSpectrum returns the latest unwrapped spectrum bins, peak bin index,
// and actual bin bandwidth (Hz) for the mini-spectrum display.
// Returns nil bins if no data yet.
func (ds *DopplerStation) LatestSpectrum() (bins []float32, peakBin int, binBW float64) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	if len(ds.latestBins) == 0 {
		return nil, -1, specBinBandwidth
	}
	out := make([]float32, len(ds.latestBins))
	copy(out, ds.latestBins)
	bw := ds.latestBinBW
	if bw <= 0 {
		bw = specBinBandwidth
	}
	return out, ds.latestPeak, bw
}

// percentileFloat32 returns the p-th percentile (0–100) of a float32 slice
// without modifying it. Uses sort.Slice (O(n log n)).
// Matches the method used by UberSDR's FrequencyReferenceMonitor (P5 noise floor).
func percentileFloat32(data []float32, p int) float32 {
	if len(data) == 0 {
		return 0
	}
	tmp := make([]float32, len(data))
	copy(tmp, data)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	idx := len(tmp) * p / 100
	if idx >= len(tmp) {
		idx = len(tmp) - 1
	}
	return tmp[idx]
}

// ---------------------------------------------------------------------------
// aggregateMinute — compute 1-minute mean and write to CSV
// ---------------------------------------------------------------------------

func (ds *DopplerStation) aggregateMinute() {
	ds.sampleMu.Lock()
	samples := ds.samples
	ds.samples = ds.samples[:0]
	ds.sampleMu.Unlock()

	if len(samples) == 0 {
		return
	}

	var sumDoppler, sumCorrected float64
	var sumSNR, sumSig, sumNoise float32
	correctedCount := 0
	minHz := samples[0].DopplerHz
	maxHz := samples[0].DopplerHz
	for _, s := range samples {
		sumDoppler += s.DopplerHz
		sumSNR += s.SNR
		sumSig += s.SignalDBFS
		sumNoise += s.NoiseDBFS
		if s.DopplerHz < minHz {
			minHz = s.DopplerHz
		}
		if s.DopplerHz > maxHz {
			maxHz = s.DopplerHz
		}
		// Each 1-second reading already has the per-sample correction applied
		if s.CorrectedDopplerHz != nil {
			sumCorrected += *s.CorrectedDopplerHz
			correctedCount++
		}
	}

	// If we have corrected values for the majority of samples, recompute min/max
	// from the corrected series so the band stays aligned with the corrected mean line.
	if correctedCount >= len(samples)/2 {
		// Seed min/max from the first non-nil corrected sample so we never mix
		// raw and corrected values (samples[0] may not have a correction yet).
		seeded := false
		for _, s := range samples {
			if s.CorrectedDopplerHz == nil {
				continue
			}
			v := *s.CorrectedDopplerHz
			if !seeded {
				minHz = v
				maxHz = v
				seeded = true
			} else {
				if v < minHz {
					minHz = v
				}
				if v > maxHz {
					maxHz = v
				}
			}
		}
	}
	n := float64(len(samples))
	meanDoppler := sumDoppler / n

	// Compute standard deviation (jitter) of raw Doppler
	var sumSqDiff float64
	for _, s := range samples {
		d := s.DopplerHz - meanDoppler
		sumSqDiff += d * d
	}
	stdDev := 0.0
	if n > 1 {
		stdDev = math.Sqrt(sumSqDiff / (n - 1)) // sample std dev
	}

	mean := MinuteMean{
		Timestamp:    time.Now().UTC(),
		DopplerHz:    meanDoppler,
		MinDopplerHz: minHz,
		MaxDopplerHz: maxHz,
		StdDevHz:     stdDev,
		SNR:          sumSNR / float32(n),
		SignalDBFS:   sumSig / float32(n),
		NoiseDBFS:    sumNoise / float32(n),
		Count:        len(samples),
	}

	// Use per-sample corrected mean if we have enough corrected samples,
	// otherwise fall back to instantaneous reference at aggregation time.
	if correctedCount >= len(samples)/2 {
		c := sumCorrected / float64(correctedCount)
		mean.CorrectedDopplerHz = &c
	} else if ds.refProvider != nil && !ds.cfg.IsReference {
		if refPPM, ok := ds.refProvider(); ok {
			c := mean.DopplerHz - refPPM*float64(ds.cfg.FreqHz)/1e6
			mean.CorrectedDopplerHz = &c
		}
	}

	ds.mu.Lock()
	ds.history = append(ds.history, mean)
	if len(ds.history) > historyDepth {
		ds.history = ds.history[len(ds.history)-historyDepth:]
	}
	ds.saveHistory() // persist to disk while lock is held
	ds.mu.Unlock()

	ds.csvWriter.write(ds.cfg, mean, mean.CorrectedDopplerHz)
}

// CurrentReading returns the latest 1-second reading (thread-safe).
func (ds *DopplerStation) CurrentReading() DopplerReading {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.current
}

// History returns a copy of the minute-mean history (thread-safe).
func (ds *DopplerStation) History() []MinuteMean {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	out := make([]MinuteMean, len(ds.history))
	copy(out, ds.history)
	return out
}

// smoothMinuteMeans applies a centred rolling average of width w over a
// []MinuteMean slice and returns the smoothed result.  w=1 is a no-op.
// The timestamp of each output point is taken from the corresponding input
// point so the time axis is unchanged.  All numeric fields are averaged;
// min/max are taken as the true min/max over the window so the band still
// reflects the actual spread.  CorrectedDopplerHz is averaged only when all
// points in the window have a non-nil value; otherwise it is set to nil.
func smoothMinuteMeans(in []MinuteMean, w int) []MinuteMean {
	if w <= 1 || len(in) == 0 {
		return in
	}
	out := make([]MinuteMean, len(in))
	half := w / 2
	for i := range in {
		start := i - half
		if start < 0 {
			start = 0
		}
		end := start + w - 1
		if end >= len(in) {
			end = len(in) - 1
		}
		window := in[start : end+1]
		n := float64(len(window))

		var sumDoppler, sumSNR, sumSig, sumNoise float64
		var sumCorr float64
		corrCount := 0
		minHz := window[0].MinDopplerHz
		maxHz := window[0].MaxDopplerHz
		for _, m := range window {
			sumDoppler += m.DopplerHz
			sumSNR += float64(m.SNR)
			sumSig += float64(m.SignalDBFS)
			sumNoise += float64(m.NoiseDBFS)
			if m.MinDopplerHz < minHz {
				minHz = m.MinDopplerHz
			}
			if m.MaxDopplerHz > maxHz {
				maxHz = m.MaxDopplerHz
			}
			if m.CorrectedDopplerHz != nil {
				sumCorr += *m.CorrectedDopplerHz
				corrCount++
			}
		}

		sm := MinuteMean{
			Timestamp:    in[i].Timestamp,
			DopplerHz:    sumDoppler / n,
			MinDopplerHz: minHz,
			MaxDopplerHz: maxHz,
			StdDevHz:     in[i].StdDevHz, // keep per-minute jitter unchanged
			SNR:          float32(sumSNR / n),
			SignalDBFS:   float32(sumSig / n),
			NoiseDBFS:    float32(sumNoise / n),
			Count:        in[i].Count,
		}
		if corrCount == len(window) {
			c := sumCorr / n
			sm.CorrectedDopplerHz = &c
		}
		out[i] = sm
	}
	return out
}

// BaselineMean returns the mean Doppler shift over the last n minute-means.
// It prefers CorrectedDopplerHz (reference-corrected) when available, falling
// back to raw DopplerHz. This ensures the value is consistent with what is
// shown in the corrected Doppler column and is correct after a restart (the
// correction is stored per-minute-mean on disk).
func (ds *DopplerStation) BaselineMean(n int) (mean float64, count int) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	h := ds.history
	if len(h) == 0 {
		return 0, 0
	}
	if len(h) < n {
		n = len(h)
	}
	recent := h[len(h)-n:]
	var sum float64
	for _, m := range recent {
		if m.CorrectedDopplerHz != nil {
			sum += *m.CorrectedDopplerHz
		} else {
			sum += m.DopplerHz
		}
	}
	return sum / float64(len(recent)), len(recent)
}

// ---------------------------------------------------------------------------
// stationManager — thread-safe registry of running DopplerStations
// ---------------------------------------------------------------------------

type stationManager struct {
	mu         sync.RWMutex
	stations   []*DopplerStation
	wg         sync.WaitGroup
	ubersdrURL string
	dataDir    string
	hub        *sseHub
	csvWriter  *csvWriter
	ctx        context.Context
}

func newStationManager(ubersdrURL, dataDir string, hub *sseHub, cw *csvWriter) *stationManager {
	return &stationManager{
		ubersdrURL: ubersdrURL,
		dataDir:    dataDir,
		hub:        hub,
		csvWriter:  cw,
	}
}

func (m *stationManager) configPath() string {
	return m.dataDir + "/stations.json"
}

// load reads stations.json and populates m.stations (without starting them).
func (m *stationManager) load() {
	cfgs, err := loadStations(m.configPath())
	if err != nil {
		log.Printf("[manager] load stations: %v", err)
		return
	}
	if len(cfgs) == 0 {
		log.Printf("[manager] no stations.json found — starting with no stations")
		return
	}
	m.mu.Lock()
	for _, cfg := range cfgs {
		if cfg.ID == "" {
			cfg.ID = uuid.New().String()
		}
		ds := newDopplerStation(cfg, m.ubersdrURL, m.dataDir, m.hub, m.csvWriter)
		ds.loadHistoryFromDisk()
		m.stations = append(m.stations, ds)
	}
	m.mu.Unlock()
	log.Printf("[manager] loaded %d station(s) from %s", len(cfgs), m.configPath())
}

// startAll starts all loaded stations using the given context.
func (m *stationManager) startAll(ctx context.Context) {
	m.ctx = ctx
	m.mu.Lock()
	m.setRefProviders()
	stations := make([]*DopplerStation, len(m.stations))
	copy(stations, m.stations)
	m.mu.Unlock()

	for _, ds := range stations {
		if !ds.cfg.Enabled {
			continue
		}
		m.wg.Add(1)
		go func(d *DopplerStation) {
			defer m.wg.Done()
			d.run(ctx)
		}(ds)
	}
}

// save writes the current station list to stations.json.
func (m *stationManager) save() {
	m.mu.RLock()
	cfgs := make([]stationConfig, 0, len(m.stations))
	for _, ds := range m.stations {
		cfgs = append(cfgs, ds.cfg)
	}
	m.mu.RUnlock()

	if err := saveStations(m.configPath(), cfgs); err != nil {
		log.Printf("[manager] save stations: %v", err)
	}
}

// list returns a snapshot of all stations.
func (m *stationManager) list() []*DopplerStation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*DopplerStation, len(m.stations))
	copy(out, m.stations)
	return out
}

// referenceCorrection returns the current clock error of the reference station in ppm.
// ppm = dopplerHz / refFreqHz * 1e6
// Callers scale this to their own frequency: corrected = raw - ppm*stationFreq/1e6
func (m *stationManager) referenceCorrection() (ppm float64, valid bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ds := range m.stations {
		if ds.cfg.IsReference {
			r := ds.CurrentReading()
			if r.Valid && ds.cfg.FreqHz > 0 {
				return r.DopplerHz / float64(ds.cfg.FreqHz) * 1e6, true
			}
			return 0, false
		}
	}
	return 0, false
}

// setRefProviders wires the referenceCorrection function into every non-reference station.
func (m *stationManager) setRefProviders() {
	for _, ds := range m.stations {
		if !ds.cfg.IsReference {
			ds.refProvider = m.referenceCorrection
		} else {
			ds.refProvider = nil
		}
	}
}

// add creates and starts a new station.
func (m *stationManager) add(cfg stationConfig) (*DopplerStation, error) {
	if cfg.ID == "" {
		cfg.ID = uuid.New().String()
	}
	if cfg.MinSNR == 0 {
		cfg.MinSNR = 10.0
	}
	if cfg.MaxDriftHz == 0 {
		cfg.MaxDriftHz = 50.0
	}
	// Clamp maxDriftHz to the spectrum window half-width so the search range
	// never exceeds what the spectrum can actually see.
	maxWindow := float64(specBinCount) / 2.0 * specBinBandwidth // 50 Hz
	if cfg.MaxDriftHz > maxWindow {
		cfg.MaxDriftHz = maxWindow
	}
	cfg.Enabled = true

	m.mu.Lock()
	for _, ds := range m.stations {
		if ds.cfg.Label == cfg.Label {
			m.mu.Unlock()
			return nil, fmt.Errorf("station %q already exists", cfg.Label)
		}
	}
	ds := newDopplerStation(cfg, m.ubersdrURL, m.dataDir, m.hub, m.csvWriter)
	if !cfg.IsReference {
		ds.refProvider = m.referenceCorrection
	}
	m.stations = append(m.stations, ds)
	m.setRefProviders()
	m.mu.Unlock()

	if m.ctx != nil && cfg.Enabled {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			ds.run(m.ctx)
		}()
	}

	m.save()
	log.Printf("[manager] added station %s @ %d Hz (id %s)", cfg.Label, cfg.FreqHz, cfg.ID[:8])
	return ds, nil
}

// remove stops and removes a station by label.
func (m *stationManager) remove(label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ds := range m.stations {
		if ds.cfg.Label == label {
			m.stations = append(m.stations[:i], m.stations[i+1:]...)
			log.Printf("[manager] removed station %s", label)
			go m.save()
			return nil
		}
	}
	return fmt.Errorf("station %q not found", label)
}

// update replaces the config for an existing station (by ID) and restarts it.
func (m *stationManager) update(id string, cfg stationConfig) error {
	m.mu.Lock()
	var target *DopplerStation
	for _, ds := range m.stations {
		if ds.cfg.ID == id {
			target = ds
			break
		}
	}
	if target == nil {
		m.mu.Unlock()
		return fmt.Errorf("station id %q not found", id)
	}
	cfg.ID = id
	// Clamp maxDriftHz to the spectrum window half-width
	maxWindow := float64(specBinCount) / 2.0 * specBinBandwidth // 50 Hz
	if cfg.MaxDriftHz <= 0 {
		cfg.MaxDriftHz = maxWindow
	} else if cfg.MaxDriftHz > maxWindow {
		cfg.MaxDriftHz = maxWindow
	}
	target.cfg = cfg
	target.minSNR = cfg.MinSNR
	target.maxDriftHz = cfg.MaxDriftHz
	m.setRefProviders()
	m.mu.Unlock()

	if m.ctx != nil && cfg.Enabled {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			target.run(m.ctx)
		}()
	}

	m.save()
	log.Printf("[manager] updated station %s (id %s)", cfg.Label, id[:8])
	return nil
}
