// csv.go — daily CSV writer for Doppler observations
//
// One CSV file per UTC day per station:
//
//	<dataDir>/YYYY/MM/DD/<label>.csv
//
// Header row:
//
//	timestamp_utc,station,freq_hz,doppler_hz,corrected_doppler_hz,snr_db,
//	signal_dbfs,noise_dbfs,callsign,grid,frequency_reference
package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// csvWriter writes minute-mean Doppler observations to daily CSV files.
type csvWriter struct {
	dataDir  string
	settings globalSettings
	mu       sync.Mutex
	// open file handles keyed by "<label>/<YYYY-MM-DD>"
	handles map[string]*csvHandle
}

type csvHandle struct {
	f   *os.File
	w   *csv.Writer
	day string // "YYYY-MM-DD"
}

func newCSVWriter(dataDir string, settings globalSettings) *csvWriter {
	return &csvWriter{
		dataDir:  dataDir,
		settings: settings,
		handles:  make(map[string]*csvHandle),
	}
}

// UpdateSettings updates the global settings used for new CSV rows.
// Takes effect on the next write; does not rewrite existing files.
func (cw *csvWriter) UpdateSettings(s globalSettings) {
	cw.mu.Lock()
	cw.settings = s
	cw.mu.Unlock()
}

// write appends one minute-mean row to the appropriate daily CSV file.
// correctedHz is the reference-corrected Doppler (nil if no reference station).
func (cw *csvWriter) write(cfg stationConfig, m MinuteMean, correctedHz *float64) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	day := m.Timestamp.UTC().Format("2006-01-02")
	key := cfg.Label + "/" + day

	h, err := cw.getHandle(key, cfg.Label, day)
	if err != nil {
		log.Printf("[csv] %s: open: %v", cfg.Label, err)
		return
	}

	// Rotate if the day has changed.
	if h.day != day {
		h.w.Flush()
		h.f.Close()
		delete(cw.handles, key)
		h, err = cw.getHandle(key, cfg.Label, day)
		if err != nil {
			log.Printf("[csv] %s: rotate: %v", cfg.Label, err)
			return
		}
	}

	// Effective callsign/grid: per-station overrides global setting
	callsign := cfg.Callsign
	if callsign == "" {
		callsign = cw.settings.Callsign
	}
	grid := cfg.Grid
	if grid == "" {
		grid = cw.settings.Grid
	}

	// Corrected Doppler column
	corrStr := ""
	if correctedHz != nil {
		corrStr = fmt.Sprintf("%.4f", *correctedHz)
	}

	row := []string{
		m.Timestamp.UTC().Format(time.RFC3339),
		cfg.Label,
		fmt.Sprintf("%d", cfg.FreqHz),
		fmt.Sprintf("%.4f", m.DopplerHz),
		corrStr,
		fmt.Sprintf("%.2f", m.SNR),
		fmt.Sprintf("%.2f", m.SignalDBFS),
		fmt.Sprintf("%.2f", m.NoiseDBFS),
		callsign,
		grid,
		string(cw.settings.FrequencyReference),
	}
	if err := h.w.Write(row); err != nil {
		log.Printf("[csv] %s: write: %v", cfg.Label, err)
		return
	}
	h.w.Flush()
}

// getHandle returns (creating if necessary) the csvHandle for the given key.
// Must be called with cw.mu held.
func (cw *csvWriter) getHandle(key, label, day string) (*csvHandle, error) {
	if h, ok := cw.handles[key]; ok {
		return h, nil
	}

	// Build path: <dataDir>/YYYY/MM/DD/<label>.csv
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cw.dataDir,
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", t.Month()),
		fmt.Sprintf("%02d", t.Day()),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, label+".csv")
	isNew := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		isNew = true
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	w := csv.NewWriter(f)
	if isNew {
		header := []string{
			"timestamp_utc", "station", "freq_hz",
			"doppler_hz", "corrected_doppler_hz",
			"snr_db", "signal_dbfs", "noise_dbfs",
			"callsign", "grid", "frequency_reference",
		}
		if err := w.Write(header); err != nil {
			f.Close()
			return nil, err
		}
		w.Flush()
	}

	h := &csvHandle{f: f, w: w, day: day}
	cw.handles[key] = h
	log.Printf("[csv] opened %s", path)
	return h, nil
}

// close flushes and closes all open file handles.
func (cw *csvWriter) close() {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	for _, h := range cw.handles {
		h.w.Flush()
		h.f.Close()
	}
	cw.handles = make(map[string]*csvHandle)
}
