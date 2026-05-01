// main.go — ubersdr_doppler: HamSCI Doppler shift monitor
//
// Stations are persisted to stations.json inside the data directory.
// Global settings (callsign, grid, frequency reference type) are persisted
// to settings.json. Both are editable via the web UI.
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
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
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

	// Load global settings (callsign, grid, frequency reference type)
	settingsPath := *dataDir + "/settings.json"
	settings, err := loadGlobalSettings(settingsPath)
	if err != nil {
		log.Printf("[main] warning: could not load settings.json: %v — using defaults", err)
	}
	log.Printf("[main] Frequency ref: %s", settings.FrequencyReference)
	if settings.Callsign != "" {
		log.Printf("[main] Callsign    : %s  Grid: %s", settings.Callsign, settings.Grid)
	}

	// settingsMu protects settings when updated via the web UI at runtime.
	var settingsMu sync.RWMutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SSE hub for pushing live updates to browsers
	hub := newSSEHub()

	// CSV writer (uses global settings for frequency_reference column)
	settingsMu.RLock()
	cw := newCSVWriter(*dataDir, settings)
	settingsMu.RUnlock()

	// Station manager — loads stations.json, manages running DopplerStations
	mgr := newStationManager(*ubersdrURL, *dataDir, hub, cw)
	mgr.load()

	// HTTP server
	go func() {
		if err := startHTTPServer(*listenAddr, mgr, hub, settingsPath, &settings, &settingsMu, cw, *uiPassword); err != nil {
			log.Fatalf("[main] HTTP server: %v", err)
		}
	}()

	// Start all loaded stations
	mgr.startAll(ctx)

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
// globalSettings — stored in settings.json
// ---------------------------------------------------------------------------

// globalSettings holds operator and hardware configuration that applies to all stations.
type globalSettings struct {
	// Operator identity — used as defaults for all stations (can be overridden per-station)
	Callsign string `json:"callsign"` // Amateur radio callsign, e.g. "G0XYZ"
	Grid     string `json:"grid"`     // Maidenhead grid locator, e.g. "IO91wm"

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
		MaxDriftHz: 100.0,
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
