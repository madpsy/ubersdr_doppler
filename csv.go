// csv.go — HamSCI Grape-compatible CSV writer for Doppler observations
//
// Produces files in the HamSCI Grape format used by the Doppler experiment:
// https://hamsci.org/doppler-instructions
//
// File naming convention (Grape standard):
//
//	<dataDir>/YYYY-MM-DDTHH:MM:SSZ_<node>_<radio>_<grid>_FRQ_<station>.csv
//
// File format:
//
//	# metadata header (# comment lines)
//	UTC,Freq,Vpk
//	HH:MM:SS, received_freq_hz, amplitude
//
// One file per UTC day per station. Files are opened at the first observation
// of the day and closed/rotated at UTC midnight.
package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// csvWriter writes 1-second Doppler observations to daily Grape-format CSV files.
type csvWriter struct {
	dataDir  string
	settings globalSettings
	mu       sync.Mutex
	// open file handles keyed by "<label>/<YYYY-MM-DD>"
	handles map[string]*csvHandle
}

type csvHandle struct {
	f        *os.File
	day      string // "YYYY-MM-DD"
	openedAt time.Time
}

func newCSVWriter(dataDir string, settings globalSettings) *csvWriter {
	return &csvWriter{
		dataDir:  dataDir,
		settings: settings,
		handles:  make(map[string]*csvHandle),
	}
}

// UpdateSettings updates the global settings used for new CSV files.
// Takes effect on the next file open (new day or new station).
func (cw *csvWriter) UpdateSettings(s globalSettings) {
	cw.mu.Lock()
	cw.settings = s
	cw.mu.Unlock()
}

// writeReading appends one 1-second reading to the appropriate daily Grape CSV file.
// This is called from the DopplerStation measurement loop every second.
func (cw *csvWriter) writeReading(cfg stationConfig, r DopplerReading) {
	if !r.Valid {
		return // only write valid readings
	}
	cw.mu.Lock()
	defer cw.mu.Unlock()

	day := r.Timestamp.UTC().Format("2006-01-02")
	key := cfg.Label + "/" + day

	h, err := cw.getHandle(key, cfg, day, r.Timestamp)
	if err != nil {
		log.Printf("[csv] %s: open: %v", cfg.Label, err)
		return
	}

	// Rotate if the day has changed.
	if h.day != day {
		h.f.Close()
		delete(cw.handles, key)
		h, err = cw.getHandle(key, cfg, day, r.Timestamp)
		if err != nil {
			log.Printf("[csv] %s: rotate: %v", cfg.Label, err)
			return
		}
	}

	// Grape format: UTC,Freq,Vpk
	// UTC: HH:MM:SS
	// Freq: absolute received frequency in Hz (nominal + doppler)
	// Vpk: peak voltage amplitude (derived from signal dBFS)
	receivedFreq := float64(cfg.FreqHz) + r.DopplerHz
	vpk := dbfsToVpk(r.SignalDBFS)

	line := fmt.Sprintf("%s, %14.3f, %f\n",
		r.Timestamp.UTC().Format("15:04:05"),
		receivedFreq,
		vpk,
	)
	if _, err := h.f.WriteString(line); err != nil {
		log.Printf("[csv] %s: write: %v", cfg.Label, err)
	}
}

// write is kept for backward compatibility — writes a minute-mean as a single row.
// Deprecated: use writeReading for 1-second Grape-format output.
func (cw *csvWriter) write(cfg stationConfig, m MinuteMean, correctedHz *float64) {
	// Convert minute-mean to a DopplerReading and write it
	r := DopplerReading{
		Timestamp:  m.Timestamp,
		DopplerHz:  m.DopplerHz,
		SNR:        m.SNR,
		SignalDBFS: m.SignalDBFS,
		NoiseDBFS:  m.NoiseDBFS,
		Valid:      true,
	}
	cw.writeReading(cfg, r)
}

// dbfsToVpk converts a dBFS power level to an approximate peak voltage (0–1 scale).
// dBFS 0 = full scale = Vpk 1.0; dBFS -60 = Vpk ~0.001
func dbfsToVpk(dbfs float32) float32 {
	if math.IsNaN(float64(dbfs)) || math.IsInf(float64(dbfs), 0) {
		return 0
	}
	// Convert dBFS (power) to linear amplitude: Vpk = 10^(dBFS/20)
	vpk := math.Pow(10.0, float64(dbfs)/20.0)
	if vpk > 1.0 {
		vpk = 1.0
	}
	return float32(vpk)
}

// grapeFilename builds the Grape-standard filename for a station/day.
// Format: YYYY-MM-DDTHH:MM:SSZ_<node>_<radio>_<grid>_FRQ_<station>.csv
// where HH:MM:SSZ is the UTC time the file was first opened.
func grapeFilename(cfg stationConfig, settings globalSettings, openedAt time.Time) string {
	node := settings.NodeNumber
	if node == "" {
		node = "N00000"
	}
	grid := cfg.Grid
	if grid == "" {
		grid = settings.Grid
	}
	if grid == "" {
		grid = "XX00xx"
	}
	// Sanitise station label for filename (remove special chars)
	station := strings.ReplaceAll(cfg.Label, " ", "")
	station = strings.ReplaceAll(station, "/", "-")

	ts := openedAt.UTC().Format("2006-01-02T15:04:05Z")
	return fmt.Sprintf("%s_%s_G1_%s_FRQ_%s.csv", ts, node, grid, station)
}

// grapeHeader builds the # metadata header block for a Grape CSV file.
func grapeHeader(cfg stationConfig, settings globalSettings, openedAt time.Time) string {
	node := settings.NodeNumber
	if node == "" {
		node = "N00000"
	}
	callsign := cfg.Callsign
	if callsign == "" {
		callsign = settings.Callsign
	}
	grid := cfg.Grid
	if grid == "" {
		grid = settings.Grid
	}

	freqRef := string(settings.FrequencyReference)
	if settings.ReferenceDescription != "" {
		freqRef += " (" + settings.ReferenceDescription + ")"
	}

	ts := openedAt.UTC().Format("2006-01-02T15:04:05Z")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("#,%s,%s,%s,,,,UberSDR,G1,%s\n", ts, node, grid, cfg.Label))
	sb.WriteString("#######################################\n")
	sb.WriteString("# MetaData for UberSDR Doppler Station\n")
	sb.WriteString("#\n")
	sb.WriteString(fmt.Sprintf("# Station Node Number      %s\n", node))
	sb.WriteString(fmt.Sprintf("# Callsign                 %s\n", callsign))
	sb.WriteString(fmt.Sprintf("# Grid Square              %s\n", grid))
	sb.WriteString(fmt.Sprintf("# Beacon Now Decoded       %s\n", cfg.Label))
	sb.WriteString(fmt.Sprintf("# Nominal Frequency        %d Hz\n", cfg.FreqHz))
	sb.WriteString(fmt.Sprintf("# Frequency Standard       %s\n", freqRef))
	sb.WriteString("# Radio                    UberSDR\n")
	sb.WriteString("# Software                 ubersdr_doppler (https://github.com/madpsy/ubersdr_doppler)\n")
	sb.WriteString("#\n")
	sb.WriteString("#######################################\n")
	sb.WriteString("UTC,Freq,Vpk\n")
	return sb.String()
}

// getHandle returns (creating if necessary) the csvHandle for the given key.
// Must be called with cw.mu held.
func (cw *csvWriter) getHandle(key string, cfg stationConfig, day string, openedAt time.Time) (*csvHandle, error) {
	if h, ok := cw.handles[key]; ok {
		return h, nil
	}

	// Build directory: <dataDir>/YYYY/MM/DD/
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

	// Grape filename
	fname := grapeFilename(cfg, cw.settings, openedAt)
	path := filepath.Join(dir, fname)

	isNew := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		isNew = true
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	if isNew {
		header := grapeHeader(cfg, cw.settings, openedAt)
		if _, err := f.WriteString(header); err != nil {
			f.Close()
			return nil, err
		}
	}

	h := &csvHandle{f: f, day: day, openedAt: openedAt}
	cw.handles[key] = h
	log.Printf("[csv] opened %s", path)
	return h, nil
}

// close flushes and closes all open file handles.
func (cw *csvWriter) close() {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	for _, h := range cw.handles {
		h.f.Close()
	}
	cw.handles = make(map[string]*csvHandle)
}
