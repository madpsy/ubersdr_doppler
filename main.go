// main.go — ubersdr_doppler: HamSCI Doppler shift monitor
//
// Stations are persisted to stations.json inside the data directory.
// Global settings (frequency reference type etc.) are persisted to
// settings.json and editable via the web UI.
//
// Receiver identity (callsign, grid, lat/lon, elevation, location) is
// fetched automatically from the UberSDR /api/description endpoint and
// does not need to be entered manually.
//
// Usage:
//
//	ubersdr_doppler -url ws://sdr.example.com/ws \
//	                -data /data \
//	                -listen :6096
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat64Or(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func main() {
	var (
		ubersdrURL = flag.String("url", envOr("UBERSDR_URL", "ws://ubersdr:8080/ws"), "UberSDR WebSocket URL (env: UBERSDR_URL)")
		dataDir    = flag.String("data", envOr("DOPPLER_DATA_DIR", "/data"), "Data directory for stations.json, settings.json and CSV logs (env: DOPPLER_DATA_DIR)")
		listenAddr = flag.String("listen", ":"+envOr("WEB_PORT", "6096"), "HTTP listen address (env: WEB_PORT)")
		uiPassword = flag.String("ui-password", envOr("UI_PASSWORD", ""), "Password required for write actions in the web UI (env: UI_PASSWORD; empty = write actions disabled)")
	)
	flag.Parse()

	if *ubersdrURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url (or UBERSDR_URL env) is required")
		flag.Usage()
		os.Exit(1)
	}

	// Ensure data directory exists
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("[main] cannot create data dir %s: %v", *dataDir, err)
	}

	log.Printf("[main] ubersdr_doppler starting")
	log.Printf("[main] UberSDR URL : %s", *ubersdrURL)
	log.Printf("[main] Data dir    : %s", *dataDir)
	log.Printf("[main] Listen addr : %s", *listenAddr)

	// Load global settings (frequency reference type, calibration offsets, etc.)
	settingsPath := *dataDir + "/settings.json"
	settings, err := loadGlobalSettings(settingsPath)
	if err != nil {
		log.Printf("[main] warning: could not load settings.json: %v — using defaults", err)
	}
	log.Printf("[main] Frequency ref: %s", settings.FrequencyReference)

	// settingsMu protects settings when updated via the web UI at runtime.
	var settingsMu sync.RWMutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SSE hub for pushing live updates to browsers
	hub := newSSEHub()

	// Receiver description cache — populated by fetching /api/description from UberSDR.
	// This provides callsign, grid, lat/lon, elevation and location without manual entry.
	var descCache receiverDescCache

	// Derive the UberSDR HTTP base URL from the WebSocket URL.
	// ws://host:port/ws  →  http://host:port
	// wss://host:port/ws →  https://host:port
	httpBase := wsURLToHTTPBase(*ubersdrURL)
	log.Printf("[main] UberSDR HTTP : %s", httpBase)

	// Fetch description immediately at startup (best-effort; non-fatal if unavailable).
	if desc, err := fetchReceiverDescription(httpBase); err != nil {
		log.Printf("[main] description fetch: %v (will retry in background)", err)
	} else {
		descCache.Store(desc)
		log.Printf("[main] Receiver     : %s  Grid: %s  Lat: %.4f  Lon: %.4f",
			desc.Callsign, desc.Maidenhead, desc.Lat, desc.Lon)
	}

	// CSV writer (uses global settings for frequency_reference column)
	settingsMu.RLock()
	cw := newCSVWriter(*dataDir, settings, &descCache)
	settingsMu.RUnlock()

	// Station manager — loads stations.json, manages running DopplerStations
	mgr := newStationManager(*ubersdrURL, *dataDir, hub, cw)
	mgr.load()

	// Load FTP settings (separate file — credentials must not appear in /api/settings)
	ftpCfgPath := *dataDir + "/ftp_settings.json"
	ftpCfg, err := loadFTPSettings(ftpCfgPath)
	if err != nil {
		log.Printf("[main] warning: could not load ftp_settings.json: %v — using defaults", err)
	}
	var ftpMu sync.RWMutex
	if ftpCfg.Enabled {
		log.Printf("[main] FTP upload: enabled — host=%s interval=%dm window=%dm",
			ftpCfg.Host, ftpCfg.IntervalMins, ftpCfg.WindowMins)
	} else {
		log.Printf("[main] FTP upload: disabled (configure via Settings → FTP Upload)")
	}

	// HTTP server
	go func() {
		if err := startHTTPServer(*listenAddr, mgr, hub, settingsPath, &settings, &settingsMu, cw, *uiPassword, &ftpCfg, &ftpMu, ftpCfgPath, &descCache, httpBase); err != nil {
			log.Fatalf("[main] HTTP server: %v", err)
		}
	}()

	// Start all loaded stations
	mgr.startAll(ctx)

	// FTP uploader goroutine — runs independently, re-reads config on each tick
	go startFTPUploader(ctx, mgr, &ftpCfg, &ftpMu, &settings, &settingsMu, ftpCfgPath, &descCache)

	// Background description refresh — re-fetches every 5 minutes so the
	// cached receiver identity stays current if the SDR is reconfigured.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if desc, err := fetchReceiverDescription(httpBase); err != nil {
					log.Printf("[main] description refresh: %v", err)
				} else {
					descCache.Store(desc)
				}
			}
		}
	}()

	// Wait for SIGINT / SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("[main] shutting down…")
	cancel()
	mgr.wg.Wait()
	log.Printf("[main] done")
}

// ---------------------------------------------------------------------------
// FrequencyReferenceType — describes the SDR's frequency reference
// ---------------------------------------------------------------------------

// FrequencyReferenceType describes the frequency reference used by the SDR.
// Included in CSV output so HamSCI can assess data quality.
type FrequencyReferenceType string

const (
	// FreqRefNone — free-running oscillator (TCXO/OCXO, no GPS lock).
	// Data is qualitative; absolute Doppler values include hardware clock offset.
	FreqRefNone FrequencyReferenceType = "none"

	// FreqRefGPSDO — GPS-disciplined oscillator (hardware).
	// Data is quantitative; absolute Doppler values are accurate to ~0.01 Hz.
	FreqRefGPSDO FrequencyReferenceType = "gpsdo"

	// FreqRefReferenceStation — software correction via a local reference signal.
	// A nearby signal (e.g. leaky GPSDO, local oscillator) is monitored as a
	// reference station and its Doppler is subtracted from all other stations
	// to cancel hardware clock drift in real time.
	FreqRefReferenceStation FrequencyReferenceType = "reference_station"
)

// ---------------------------------------------------------------------------
// receiverDescription — live data from UberSDR /api/description
// ---------------------------------------------------------------------------

// receiverDescription holds the subset of UberSDR's /api/description response
// that ubersdr_doppler needs for CSV metadata and the UI header badge.
type receiverDescription struct {
	Callsign   string  `json:"callsign"`
	Maidenhead string  `json:"maidenhead"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	ASL        float64 `json:"asl"`        // metres above sea level
	Location   string  `json:"location"`   // free-text location string
	Name       string  `json:"name"`       // SDR name
	PublicURL  string  `json:"public_url"` // public URL of the SDR
}

// receiverDescCache is a thread-safe cache for the latest receiverDescription.
type receiverDescCache struct {
	mu   sync.RWMutex
	desc *receiverDescription
}

func (c *receiverDescCache) Store(d receiverDescription) {
	c.mu.Lock()
	c.desc = &d
	c.mu.Unlock()
}

func (c *receiverDescCache) Load() (receiverDescription, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.desc == nil {
		return receiverDescription{}, false
	}
	return *c.desc, true
}

// fetchReceiverDescription fetches /api/description from the UberSDR HTTP base URL
// and extracts the receiver identity fields.
func fetchReceiverDescription(httpBase string) (receiverDescription, error) {
	resp, err := http.Get(httpBase + "/api/description")
	if err != nil {
		return receiverDescription{}, fmt.Errorf("GET /api/description: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return receiverDescription{}, fmt.Errorf("GET /api/description: status %d", resp.StatusCode)
	}

	// Parse only the fields we need from the (potentially large) response.
	var raw struct {
		Receiver struct {
			Callsign string `json:"callsign"`
			GPS      struct {
				Lat        float64 `json:"lat"`
				Lon        float64 `json:"lon"`
				Maidenhead string  `json:"maidenhead"`
			} `json:"gps"`
			ASL      float64 `json:"asl"`
			Location string  `json:"location"`
			Name     string  `json:"name"`
		} `json:"receiver"`
		PublicURL string `json:"public_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return receiverDescription{}, fmt.Errorf("decode /api/description: %w", err)
	}

	return receiverDescription{
		Callsign:   raw.Receiver.Callsign,
		Maidenhead: raw.Receiver.GPS.Maidenhead,
		Lat:        raw.Receiver.GPS.Lat,
		Lon:        raw.Receiver.GPS.Lon,
		ASL:        raw.Receiver.ASL,
		Location:   raw.Receiver.Location,
		Name:       raw.Receiver.Name,
		PublicURL:  raw.PublicURL,
	}, nil
}

// wsURLToHTTPBase converts a WebSocket URL to an HTTP base URL.
// ws://host:port/path  →  http://host:port
// wss://host:port/path →  https://host:port
func wsURLToHTTPBase(wsURL string) string {
	s := wsURL
	if len(s) >= 6 && s[:6] == "wss://" {
		s = "https://" + s[6:]
	} else if len(s) >= 5 && s[:5] == "ws://" {
		s = "http://" + s[5:]
	}
	// Strip path — keep only scheme://host:port
	if idx := strings.Index(s[8:], "/"); idx >= 0 {
		s = s[:8+idx]
	}
	return s
}

// ---------------------------------------------------------------------------
// globalSettings — stored in settings.json
// ---------------------------------------------------------------------------

// globalSettings holds operator and hardware configuration that applies to all stations.
// Receiver identity (callsign, grid, lat/lon, elevation, location) is sourced
// automatically from UberSDR's /api/description and is NOT stored here.
type globalSettings struct {
	// HamSCI Grape node number — assigned by HamSCI for registered stations
	// Format: "N00001" (6 chars). Leave empty if not registered.
	// Register at: https://hamsci.org/grape-node-registration
	NodeNumber string `json:"node_number"`

	// Frequency reference type — affects CSV quality flag and UI display
	// Values: "none", "gpsdo", "reference_station"
	FrequencyReference FrequencyReferenceType `json:"frequency_reference"`

	// Optional description of the reference (for documentation and CSV metadata)
	// Examples: "Leo Bodnar GPSDO on 10 MHz", "Trimble Thunderbolt", "Leaky 10 MHz OCXO"
	ReferenceDescription string `json:"reference_description"`

	// CalibrationOffsetDB is added to the raw dBFS signal level before converting to Vpk.
	// Use this to correct for known system gain/loss (antenna, cable, preamp, SDR gain).
	// Positive values boost Vpk (system reads lower than true); negative values cut it.
	// Set to 0 to disable. Typical values: -40 to +40 dB.
	CalibrationOffsetDB float64 `json:"calibration_offset_db"`

	// ManualOffsetHz is a static frequency correction applied to all station readings.
	// Use this when you know your hardware clock is off by a fixed amount (e.g. measured
	// against a known reference). This is subtracted from all Doppler readings.
	// Set to 0 to disable. Typical values: -50 to +50 Hz.
	// This correction is applied in addition to any reference station correction.
	ManualOffsetHz float64 `json:"manual_offset_hz"`
}

// defaultGlobalSettings returns sensible defaults.
func defaultGlobalSettings() globalSettings {
	return globalSettings{
		FrequencyReference: FreqRefNone,
	}
}

// saveGlobalSettings writes settings to settings.json atomically.
func saveGlobalSettings(path string, s globalSettings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadGlobalSettings reads settings.json; returns defaults if file does not exist.
func loadGlobalSettings(path string) (globalSettings, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return defaultGlobalSettings(), nil
	}
	if err != nil {
		return defaultGlobalSettings(), err
	}
	var s globalSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultGlobalSettings(), err
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// stationConfig — one entry in stations.json
// ---------------------------------------------------------------------------

// stationConfig is the persisted configuration for a single monitored station.
type stationConfig struct {
	ID          string  `json:"id"`           // stable UUID
	Label       string  `json:"label"`        // display name, e.g. "WWV-10"
	FreqHz      int     `json:"freq_hz"`      // nominal carrier frequency in Hz
	Callsign    string  `json:"callsign"`     // operator callsign (overrides global setting if set)
	Grid        string  `json:"grid"`         // Maidenhead grid locator (overrides global setting if set)
	MinSNR      float64 `json:"min_snr"`      // minimum SNR in dB to accept a reading
	MaxDriftHz  float64 `json:"max_drift_hz"` // ±Hz search range around carrier
	Enabled     bool    `json:"enabled"`      // false = configured but not running
	IsReference bool    `json:"is_reference"` // true = local reference signal (e.g. leaky GPSDO)
	// When is_reference is true, this station's Doppler is subtracted from all
	// other stations' readings to cancel hardware clock drift in real time.
}

// defaultStationConfig returns sensible defaults for a new station.
func defaultStationConfig() stationConfig {
	return stationConfig{
		MinSNR:     10.0,
		MaxDriftHz: 50.0, // spectrum window is ±50 Hz (200 bins × 0.5 Hz/bin)
		Enabled:    true,
	}
}

// saveStations writes the station list to stations.json atomically.
func saveStations(path string, cfgs []stationConfig) error {
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadStations reads stations.json; returns empty slice if file does not exist.
func loadStations(path string) ([]stationConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfgs []stationConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return nil, err
	}
	return cfgs, nil
}
