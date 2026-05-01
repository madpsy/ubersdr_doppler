// doppler.go — DopplerStation and stationManager
//
// Each DopplerStation connects to UberSDR as a USB audio channel tuned to
// (carrier - 500 Hz), so the carrier appears as a 500 Hz audio tone.
// A 1-second FFT window finds the peak bin in the 400–600 Hz range and
// converts the offset from 500 Hz to a Doppler shift in Hz.
//
// Minute-mean readings are accumulated and written to CSV.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
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
// DopplerStation — one monitored time/frequency station
// ---------------------------------------------------------------------------

const (
	// The carrier is placed at this audio frequency (Hz) by tuning the
	// receiver to (carrierFreq - audioCarrierHz).
	audioCarrierHz = 500.0

	// Search window around audioCarrierHz (Hz). Readings outside this range
	// are rejected as false locks.
	audioSearchHalfWidth = 100.0

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
	// nil = no reference station configured.
	refProvider func() (float64, bool)

	// inst is the UberSDR audio connection (from ubersdr.go).
	inst *instance

	// Measurement state — protected by mu.
	mu      sync.RWMutex
	current DopplerReading
	history []MinuteMean // ring buffer of minute-means, newest last

	// 1-second sample accumulator for minute-mean calculation.
	sampleMu sync.Mutex
	samples  []DopplerReading

	stopChan chan struct{}
}

// newDopplerStation creates a DopplerStation from a stationConfig.
func newDopplerStation(cfg stationConfig, ubersdrURL string, hub *sseHub, cw *csvWriter) *DopplerStation {
	// Tune to (carrier - audioCarrierHz) so the carrier appears at audioCarrierHz in the audio.
	// We use AM mode with a symmetric ±600 Hz filter so the carrier sits at the centre
	// of the passband regardless of drift direction, and the lower edge never cuts it off.
	// AM mode: bandwidthLow = -600, bandwidthHigh = +600 → 1.2 kHz total passband.
	// The carrier appears at audioCarrierHz (500 Hz) above the dial frequency.
	dialHz := cfg.FreqHz - int(audioCarrierHz)

	// AM mode, 1200 Hz bandwidth (symmetric ±600 Hz around dial = carrier at 500 Hz audio).
	inst := newInstance(dialHz, 0, "am", ubersdrURL, "", cfg.Label, 1200)

	return &DopplerStation{
		cfg:        cfg,
		ubersdrURL: ubersdrURL,
		minSNR:     cfg.MinSNR,
		maxDriftHz: cfg.MaxDriftHz,
		hub:        hub,
		csvWriter:  cw,
		inst:       inst,
		stopChan:   make(chan struct{}),
	}
}

// run starts the station's audio connection and measurement loops.
// Blocks until ctx is cancelled.
func (ds *DopplerStation) run(ctx context.Context) {
	log.Printf("[%s] starting — carrier %d Hz, dial %d Hz, min SNR %.1f dB",
		ds.cfg.Label, ds.cfg.FreqHz, ds.cfg.FreqHz-int(audioCarrierHz), ds.minSNR)

	// Start the UberSDR audio connection in a goroutine.
	go ds.inst.start(ctx)

	// Start the 1-second measurement ticker.
	measureTicker := time.NewTicker(1 * time.Second)
	defer measureTicker.Stop()

	// Start the 1-minute aggregation ticker.
	minuteTicker := time.NewTicker(1 * time.Minute)
	defer minuteTicker.Stop()

	// FFT accumulator — collects PCM samples between ticks.
	// Sample rate is initialised to 0 and set from the first packet's metadata.
	acc := newDopplerFFT(ds.cfg.FreqHz)

	for {
		select {
		case <-ctx.Done():
			return

		case pcm, ok := <-ds.inst.AudioCh:
			if !ok {
				return
			}
			// Update sample rate from the instance once it's known (set after first packet).
			ds.inst.streamMu.RLock()
			sr := ds.inst.streamSampleRate
			ds.inst.streamMu.RUnlock()
			if sr > 0 && sr != acc.sampleRate {
				log.Printf("[%s] sample rate: %d Hz (bin resolution: %.3f Hz)",
					ds.cfg.Label, sr, float64(sr)/float64(dopplerFFTSize))
				acc.init(sr)
			}
			acc.push(pcm)

		case <-measureTicker.C:
			reading := acc.measure(ds.minSNR, ds.maxDriftHz)
			reading.Timestamp = time.Now().UTC()

			ds.mu.Lock()
			ds.current = reading
			ds.mu.Unlock()

			// Write 1-second reading to Grape-format CSV (only valid readings).
			if reading.Valid {
				ds.csvWriter.writeReading(ds.cfg, reading)
			}

			// Accumulate for minute-mean history (used by the web UI chart).
			if reading.Valid {
				ds.sampleMu.Lock()
				ds.samples = append(ds.samples, reading)
				ds.sampleMu.Unlock()
			}

			// Push live update to SSE clients.
			ds.hub.broadcast(ds.cfg.Label, reading)

		case <-minuteTicker.C:
			ds.aggregateMinute()
		}
	}
}

// aggregateMinute computes the mean of the last minute's valid samples,
// appends it to history, and writes it to CSV.
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

	// Compute reference correction for CSV (nil if no reference station).
	var correctedHz *float64
	if ds.refProvider != nil && !ds.cfg.IsReference {
		if refHz, ok := ds.refProvider(); ok {
			c := mean.DopplerHz - refHz
			correctedHz = &c
		}
	}

	// Write to CSV.
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

// BaselineMean returns the mean Doppler shift over the last n minute-means
// and the number of samples used. Returns (0, 0) if no history.
// Used to show the hardware clock offset (for non-GPSDO receivers) or to
// verify a reference station is tracking near zero.
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
// dopplerFFT — accumulates PCM and measures carrier Doppler shift
// ---------------------------------------------------------------------------

// dopplerFFT accumulates S16LE mono PCM samples and, on demand, runs an FFT
// to find the peak bin near audioCarrierHz and compute the Doppler offset.
type dopplerFFT struct {
	carrierHz  int       // nominal carrier frequency (for labelling only)
	sampleRate int       // set from first packet; 0 until known
	buf        []float64 // ring buffer of normalised samples
	bufPos     int
	window     []float64    // Hann window coefficients
	cx         []complex128 // FFT work buffer
	ready      bool         // true once sampleRate is known and buf is sized
}

// dopplerFFTSize controls frequency resolution.
// At 12 kHz sample rate: 131072 samples = ~10.9 s integration window
// Bin resolution = 12000 / 131072 = 0.0916 Hz/bin
// With parabolic interpolation: ~0.01 Hz precision — suitable for ionospheric Doppler.
// The ring buffer always holds the most recent dopplerFFTSize samples; the FFT
// is computed on demand every second using whatever samples have accumulated.
const dopplerFFTSize = 131072 // power of 2; ~10.9 s at 12 kHz → 0.092 Hz/bin

func newDopplerFFT(carrierHz int) *dopplerFFT {
	return &dopplerFFT{carrierHz: carrierHz}
}

// init sets up the FFT buffers once the sample rate is known.
func (d *dopplerFFT) init(sampleRate int) {
	d.sampleRate = sampleRate
	d.buf = make([]float64, dopplerFFTSize)
	d.window = make([]float64, dopplerFFTSize)
	d.cx = make([]complex128, dopplerFFTSize)
	for i := 0; i < dopplerFFTSize; i++ {
		d.window[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(dopplerFFTSize-1)))
	}
	d.ready = true
}

// push adds S16LE PCM bytes to the ring buffer.
// Does nothing if init() has not yet been called (sample rate not yet known).
func (d *dopplerFFT) push(pcm []byte) {
	if !d.ready {
		return // wait until sample rate is known from the first packet header
	}
	n := len(pcm) / 2
	if n == 0 {
		return
	}
	for i := 0; i < n; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		d.buf[d.bufPos] = float64(s) / 32768.0
		d.bufPos = (d.bufPos + 1) % dopplerFFTSize
	}
}

// measure runs the FFT on the current buffer and returns a DopplerReading.
// minSNR is the minimum SNR (dB) required for a valid reading.
// maxDriftHz is the ±Hz search range around audioCarrierHz.
func (d *dopplerFFT) measure(minSNR, maxDriftHz float64) DopplerReading {
	if !d.ready {
		return DopplerReading{}
	}

	sr := float64(d.sampleRate)
	binHz := sr / float64(dopplerFFTSize)

	// Apply Hann window and fill complex input (unwrap ring buffer).
	for i := 0; i < dopplerFFTSize; i++ {
		idx := (d.bufPos + i) % dopplerFFTSize
		v := d.buf[idx] * d.window[i]
		d.cx[i] = complex(v, 0)
	}

	fftRadix2(d.cx) // from fft.go

	// Convert to magnitude in dBFS.
	mags := make([]float32, dopplerFFTSize/2)
	for i := range mags {
		re := real(d.cx[i])
		im := imag(d.cx[i])
		power := (re*re + im*im) / float64(dopplerFFTSize*dopplerFFTSize)
		if power > 1e-20 {
			mags[i] = float32(10 * math.Log10(power))
		} else {
			mags[i] = -100
		}
	}

	// Noise floor: median of all bins (robust to a single strong carrier).
	noiseFloor := medianFloat32(mags)

	// Search range: audioCarrierHz ± maxDriftHz.
	searchLow := audioCarrierHz - maxDriftHz
	searchHigh := audioCarrierHz + maxDriftHz
	if searchLow < 0 {
		searchLow = 0
	}
	binLow := int(searchLow / binHz)
	binHigh := int(searchHigh/binHz) + 1
	if binHigh >= len(mags) {
		binHigh = len(mags) - 1
	}

	// Find peak bin in search range.
	peakBin := binLow
	peakPower := mags[binLow]
	for i := binLow + 1; i <= binHigh; i++ {
		if mags[i] > peakPower {
			peakPower = mags[i]
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

	// Sub-bin interpolation using parabolic peak.
	peakFreqHz := subBinFreq(mags, peakBin, binHz)

	// Doppler shift = measured audio frequency - expected audio carrier frequency.
	dopplerHz := peakFreqHz - audioCarrierHz

	return DopplerReading{
		DopplerHz:  dopplerHz,
		SNR:        snr,
		SignalDBFS: peakPower,
		NoiseDBFS:  noiseFloor,
		Valid:       true,
	}
}

// subBinFreq returns the interpolated frequency (Hz) of the peak using
// parabolic interpolation on the three bins around peakBin.
func subBinFreq(mags []float32, peakBin int, binHz float64) float64 {
	if peakBin <= 0 || peakBin >= len(mags)-1 {
		return float64(peakBin) * binHz
	}
	alpha := float64(mags[peakBin-1])
	beta := float64(mags[peakBin])
	gamma := float64(mags[peakBin+1])
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
	return (float64(peakBin) + offset) * binHz
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
// stationManager — thread-safe registry of running DopplerStations
// ---------------------------------------------------------------------------

// stationManager manages the set of active DopplerStations and persists
// their configuration to stations.json.
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
// Must be called after load() and before any UI interaction.
func (m *stationManager) startAll(ctx context.Context) {
	m.ctx = ctx
	m.mu.Lock()
	// Wire reference providers before starting any goroutines.
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

// list returns a snapshot of all stations (safe to iterate without lock).
func (m *stationManager) list() []*DopplerStation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*DopplerStation, len(m.stations))
	copy(out, m.stations)
	return out
}

// referenceCorrection returns the current Doppler reading of the reference station
// (is_reference: true), or (0, false) if no reference station is configured or valid.
// This value represents the hardware clock drift and can be subtracted from all
// other stations' readings to get the true ionospheric Doppler shift.
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

// add creates and starts a new station. Returns error if label already exists.
// setRefProviders wires the referenceCorrection function into every non-reference station.
// Must be called with m.mu held or when no goroutines are running yet.
func (m *stationManager) setRefProviders() {
	for _, ds := range m.stations {
		if !ds.cfg.IsReference {
			ds.refProvider = m.referenceCorrection
		} else {
			ds.refProvider = nil
		}
	}
}

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
	// Re-wire all stations in case a new reference station was added.
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
			ds.inst.stop()
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
	// Stop the old instance.
	target.inst.stop()
	// Replace config and create a new instance.
	cfg.ID = id
	target.cfg = cfg
	target.minSNR = cfg.MinSNR
	target.maxDriftHz = cfg.MaxDriftHz
	dialHz := cfg.FreqHz - int(audioCarrierHz)
	target.inst = newInstance(dialHz, 0, "am", m.ubersdrURL, "", cfg.Label, 1200)
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

// fmt is needed for the error returns above — import it via main.go's import block.
// (Go allows cross-file use within the same package.)
var _ = fmt.Sprintf // ensure fmt is used
