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
// The spectrum connection uses a 500-bin × 2 Hz = 1 kHz window centred on the
// carrier frequency, giving 2 Hz/bin resolution. With parabolic sub-bin
// interpolation the effective precision is ~0.01 Hz.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
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
	Timestamp  time.Time `json:"timestamp"`
	DopplerHz  float64   `json:"doppler_hz"`  // measured carrier offset from nominal (Hz)
	SNR        float32   `json:"snr_db"`      // signal-to-noise ratio (dB)
	SignalDBFS float32   `json:"signal_dbfs"` // peak signal power (dBFS)
	NoiseDBFS  float32   `json:"noise_dbfs"`  // noise floor (dBFS)
	Valid      bool      `json:"valid"`       // false if SNR < minSNR or no signal
}

// MinuteMean is the 1-minute mean of valid DopplerReadings.
type MinuteMean struct {
	Timestamp  time.Time `json:"timestamp"`
	DopplerHz  float64   `json:"doppler_hz"`
	SNR        float32   `json:"snr_db"`
	SignalDBFS float32   `json:"signal_dbfs"`
	NoiseDBFS  float32   `json:"noise_dbfs"`
	Count      int       `json:"count"` // number of valid samples averaged
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
}

func newAudioBroadcastHub() *audioBroadcastHub {
	return &audioBroadcastHub{clients: make(map[chan []byte]struct{})}
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
	h.mu.Unlock()
	close(ch)
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
	// Spectrum channel parameters — 500 bins × 2 Hz = 1 kHz window.
	// This matches UberSDR's own frequency reference monitor.
	specBinCount     = 500
	specBinBandwidth = 2.0 // Hz/bin

	// History depth for minute-means (24 hours × 60 minutes).
	historyDepth = 24 * 60
)

// DopplerStation monitors one standard time/frequency station.
type DopplerStation struct {
	cfg stationConfig

	ubersdrURL string
	minSNR     float64
	maxDriftHz float64

	hub       *sseHub
	csvWriter *csvWriter

	// refProvider returns the current reference station Doppler correction (Hz)
	// and whether it is valid. Set by stationManager after creation.
	refProvider func() (float64, bool)

	// audioHub fans out PCM audio to preview listeners.
	audioHub *audioBroadcastHub

	// streamSampleRate is set from the first audio packet header.
	streamMu         sync.RWMutex
	streamSampleRate int

	// Measurement state — protected by mu.
	mu          sync.RWMutex
	current     DopplerReading
	history     []MinuteMean
	latestBins  []float32 // latest unwrapped spectrum bins for display
	latestPeak  int       // peak bin index in latestBins (-1 if no valid signal)

	// 1-second sample accumulator for minute-mean calculation.
	sampleMu sync.Mutex
	samples  []DopplerReading
}

// newDopplerStation creates a DopplerStation from a stationConfig.
func newDopplerStation(cfg stationConfig, ubersdrURL string, hub *sseHub, cw *csvWriter) *DopplerStation {
	return &DopplerStation{
		cfg:        cfg,
		ubersdrURL: ubersdrURL,
		minSNR:     cfg.MinSNR,
		maxDriftHz: cfg.MaxDriftHz,
		hub:        hub,
		csvWriter:  cw,
		audioHub:   newAudioBroadcastHub(),
	}
}

// wsBaseURL converts the UberSDR WebSocket URL to an HTTP base URL.
func wsBaseURL(wsURL string) string {
	u, _ := url.Parse(wsURL)
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "/ws" || path == "" {
		path = ""
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, path)
}

// wsURL builds a WebSocket URL for the given parameters.
func (ds *DopplerStation) wsURL(mode string, extraParams url.Values) string {
	u, _ := url.Parse(ds.ubersdrURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}
	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", ds.cfg.FreqHz))
	q.Set("mode", mode)
	q.Set("user_session_id", uuid.New().String())
	for k, vs := range extraParams {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	return fmt.Sprintf("%s://%s%s?%s", wsScheme, u.Host, path, q.Encode())
}

// checkConnection registers a session with UberSDR.
func (ds *DopplerStation) checkConnection(sessionID string) error {
	base := wsBaseURL(ds.ubersdrURL)
	endpoint := base + "/connection"
	body := fmt.Sprintf(`{"user_session_id":"%s"}`, sessionID)
	resp, err := http.Post(endpoint, "application/json", bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("connection rejected (password required?)")
	}
	return nil
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

	// Latest spectrum bins (updated by the WebSocket goroutine)
	var specMu sync.Mutex
	var latestBins []float32

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

			specParams := url.Values{}
			specParams.Set("bin_count", fmt.Sprintf("%d", specBinCount))
			specParams.Set("bin_bandwidth", fmt.Sprintf("%.1f", specBinBandwidth))
			specParams.Set("user_session_id", sessionID)
			wsAddr := ds.wsURL("spectrum", specParams)

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

			// Read loop
			for {
				if ctx.Err() != nil {
					localCancel()
					conn.Close()
					return
				}
				_, msg, err := conn.ReadMessage()
				if err != nil {
					localCancel()
					conn.Close()
					log.Printf("[%s] spectrum: read error: %v — reconnecting", ds.cfg.Label, err)
					break
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
				specMu.Unlock()
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
			specMu.Unlock()

			if len(bins) == 0 {
				continue
			}

			reading, peakBin := detectDopplerWithPeak(bins, ds.cfg.FreqHz, specBinBandwidth, ds.minSNR, ds.maxDriftHz)
			reading.Timestamp = time.Now().UTC()

			// Store spectrum snapshot for the mini-spectrum display
			binsCopy := make([]float32, len(bins))
			copy(binsCopy, bins)

			ds.mu.Lock()
			ds.current = reading
			ds.latestBins = binsCopy
			ds.latestPeak = peakBin
			ds.mu.Unlock()

			// Write 1-second reading to Grape CSV
			if reading.Valid {
				ds.csvWriter.writeReading(ds.cfg, reading)
			}

			// Accumulate for minute-mean
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

		// Only connect when there are preview listeners
		if !ds.audioHub.hasListeners() {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		sessionID := uuid.New().String()
		if err := ds.checkConnection(sessionID); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		audioParams := url.Values{}
		audioParams.Set("format", "pcm-zstd")
		audioParams.Set("version", "2")
		audioParams.Set("user_session_id", sessionID)
		// Narrow AM filter centred on carrier
		audioParams.Set("bandwidthLow", "-600")
		audioParams.Set("bandwidthHigh", "600")
		wsAddr := ds.wsURL("am", audioParams)

		hdr := http.Header{}
		hdr.Set("User-Agent", "ubersdr_doppler/1.0")
		conn, _, err := wsDialer.Dial(wsAddr, hdr)
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		log.Printf("[%s] audio preview connected", ds.cfg.Label)

		localCtx, localCancel := context.WithCancel(ctx)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-localCtx.Done():
					return
				case <-ticker.C:
					conn.WriteJSON(map[string]string{"type": "ping"})
				}
			}
		}()

		for {
			if ctx.Err() != nil || !ds.audioHub.hasListeners() {
				localCancel()
				conn.Close()
				break
			}
			msgType, msg, err := conn.ReadMessage()
			if err != nil {
				localCancel()
				conn.Close()
				break
			}
			if msgType != websocket.BinaryMessage {
				continue
			}
			pkt, err := dec.decode(msg, true)
			if err != nil || len(pkt.pcm) == 0 {
				continue
			}
			// Update sample rate
			if pkt.sampleRate > 0 {
				ds.streamMu.Lock()
				ds.streamSampleRate = pkt.sampleRate
				ds.streamMu.Unlock()
			}
			ds.audioHub.broadcast(pkt.pcm)
		}

		localCancel()
		time.Sleep(2 * time.Second)
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
// first, then positive freqs).
func detectDoppler(bins []float32, carrierHz int, binBandwidth, minSNR, maxDriftHz float64) DopplerReading {
	n := len(bins)
	if n == 0 {
		return DopplerReading{}
	}

	// Noise floor: median of all bins (robust to a single strong carrier)
	noiseFloor := medianFloat32(bins)

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

	// Find peak bin in search range
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
		return DopplerReading{
			SNR:        snr,
			SignalDBFS: peakPower,
			NoiseDBFS:  noiseFloor,
			Valid:       false,
		}
	}

	// Sub-bin interpolation using parabolic peak
	peakFreqHz := subBinFreq(bins, peakBin, binBandwidth, n)

	// Doppler shift = measured frequency - nominal carrier frequency
	// After unwrapping: center bin corresponds to carrierHz
	// bin offset from center × binBandwidth = frequency offset from carrier
	dopplerHz := peakFreqHz - float64(carrierHz)

	return DopplerReading{
		DopplerHz:  dopplerHz,
		SNR:        snr,
		SignalDBFS: peakPower,
		NoiseDBFS:  noiseFloor,
		Valid:       true,
	}
}

// detectDopplerWithPeak is like detectDoppler but also returns the integer peak bin index.
// Returns -1 for peakBin when no valid signal is found.
func detectDopplerWithPeak(bins []float32, carrierHz int, binBandwidth, minSNR, maxDriftHz float64) (DopplerReading, int) {
	n := len(bins)
	if n == 0 {
		return DopplerReading{}, -1
	}
	noiseFloor := medianFloat32(bins)
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
	peakFreqHz := subBinFreq(bins, peakBin, binBandwidth, n)
	dopplerHz := peakFreqHz - float64(carrierHz)
	return DopplerReading{
		DopplerHz:  dopplerHz,
		SNR:        snr,
		SignalDBFS: peakPower,
		NoiseDBFS:  noiseFloor,
		Valid:       true,
	}, peakBin
}

// LatestSpectrum returns the latest unwrapped spectrum bins and peak bin index
// for the mini-spectrum display. Returns nil if no data yet.
func (ds *DopplerStation) LatestSpectrum() (bins []float32, peakBin int) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	if len(ds.latestBins) == 0 {
		return nil, -1
	}
	out := make([]float32, len(ds.latestBins))
	copy(out, ds.latestBins)
	return out, ds.latestPeak
}

// subBinFreq returns the interpolated frequency (Hz) of the peak using
// parabolic interpolation on the three bins around peakBin.
// After unwrapping, bin 0 = carrierHz - (n/2)*binBandwidth,
// bin n/2 = carrierHz, bin n-1 = carrierHz + (n/2-1)*binBandwidth.
func subBinFreq(bins []float32, peakBin int, binBandwidth float64, n int) float64 {
	var centroidBin float64
	if peakBin > 0 && peakBin < n-1 {
		alpha := float64(bins[peakBin-1])
		beta := float64(bins[peakBin])
		gamma := float64(bins[peakBin+1])
		denom := alpha - 2*beta + gamma
		var offset float64
		if math.Abs(denom) > 0.001 {
			offset = 0.5 * (alpha - gamma) / denom
			if offset > 0.5 {
				offset = 0.5
			} else if offset < -0.5 {
				offset = -0.5
			}
		}
		centroidBin = float64(peakBin) + offset
	} else {
		centroidBin = float64(peakBin)
	}

	// Convert bin index to absolute frequency
	// bin 0 = center - (n/2)*binBandwidth
	// bin n/2 = center (carrier)
	// bin i = center + (i - n/2) * binBandwidth
	// But we don't know center here — return offset from center instead
	// and let the caller add carrierHz.
	// Actually: return absolute frequency = carrierHz + (centroidBin - n/2) * binBandwidth
	// We pass n as parameter so we can compute this.
	return float64(0) + (centroidBin-float64(n)/2.0)*binBandwidth
	// Note: caller adds carrierHz via: dopplerHz = peakFreqHz - carrierHz
	// which simplifies to: dopplerHz = (centroidBin - n/2) * binBandwidth
}

// medianFloat32 returns the median value of a float32 slice without modifying it.
// Uses sort.Slice (O(n log n)) — safe for large FFT magnitude arrays.
func medianFloat32(data []float32) float32 {
	if len(data) == 0 {
		return 0
	}
	tmp := make([]float32, len(data))
	copy(tmp, data)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	return tmp[len(tmp)/2]
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

	var sumDoppler float64
	var sumSNR, sumSig, sumNoise float32
	for _, s := range samples {
		sumDoppler += s.DopplerHz
		sumSNR += s.SNR
		sumSig += s.SignalDBFS
		sumNoise += s.NoiseDBFS
	}
	n := float64(len(samples))
	mean := MinuteMean{
		Timestamp:  time.Now().UTC(),
		DopplerHz:  sumDoppler / n,
		SNR:        sumSNR / float32(n),
		SignalDBFS: sumSig / float32(n),
		NoiseDBFS:  sumNoise / float32(n),
		Count:      len(samples),
	}

	ds.mu.Lock()
	ds.history = append(ds.history, mean)
	if len(ds.history) > historyDepth {
		ds.history = ds.history[len(ds.history)-historyDepth:]
	}
	ds.mu.Unlock()

	// Compute reference correction for CSV
	var correctedHz *float64
	if ds.refProvider != nil && !ds.cfg.IsReference {
		if refHz, ok := ds.refProvider(); ok {
			c := mean.DopplerHz - refHz
			correctedHz = &c
		}
	}

	ds.csvWriter.write(ds.cfg, mean, correctedHz)
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

// BaselineMean returns the mean Doppler shift over the last n minute-means.
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
		sum += m.DopplerHz
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
		ds := newDopplerStation(cfg, m.ubersdrURL, m.hub, m.csvWriter)
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

// referenceCorrection returns the current Doppler reading of the reference station.
func (m *stationManager) referenceCorrection() (dopplerHz float64, valid bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ds := range m.stations {
		if ds.cfg.IsReference {
			r := ds.CurrentReading()
			if r.Valid {
				return r.DopplerHz, true
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
		cfg.MaxDriftHz = 100.0
	}
	cfg.Enabled = true

	m.mu.Lock()
	for _, ds := range m.stations {
		if ds.cfg.Label == cfg.Label {
			m.mu.Unlock()
			return nil, fmt.Errorf("station %q already exists", cfg.Label)
		}
	}
	ds := newDopplerStation(cfg, m.ubersdrURL, m.hub, m.csvWriter)
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
