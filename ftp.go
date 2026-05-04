// ftp.go — periodic FTP upload of preview CSVs for ubersdr_doppler
//
// The uploader goroutine wakes at aligned clock boundaries (e.g. :00, :15,
// :30, :45 UTC for a 15-minute interval) and uploads one Grape-format CSV
// per enabled station covering the configured window of recent data.
//
// Filename format:
//
//	<rangeStart>_<rangeEnd>_<node>_<grid>_FRQ_<station>.csv
//	e.g. 2026-05-04T18:00:00Z_2026-05-04T18:15:00Z_N00001_IO91wm_FRQ_WWV10.csv
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// ---------------------------------------------------------------------------
// FTP test result — returned by testFTPConnection
// ---------------------------------------------------------------------------

// FTPTestStep is one step in the FTP connection test sequence.
type FTPTestStep struct {
	Step   string `json:"step"`
	Status string `json:"status"` // "ok" | "error" | "skip"
	Detail string `json:"detail"`
}

// FTPTestResult is the full result of a connection test.
type FTPTestResult struct {
	OK    bool          `json:"ok"`
	Steps []FTPTestStep `json:"steps"`
}

// testFTPConnection dials the FTP server described by cfg and runs a sequence
// of diagnostic steps, returning a structured result for display in the UI.
// The test uses the supplied cfg directly (not the saved settings) so the user
// can test before saving.
func testFTPConnection(cfg ftpSettings) FTPTestResult {
	var steps []FTPTestStep
	add := func(step, status, detail string) {
		steps = append(steps, FTPTestStep{Step: step, Status: status, Detail: detail})
	}
	skip := func(step, reason string) {
		steps = append(steps, FTPTestStep{Step: step, Status: "skip", Detail: reason})
	}
	fail := func(step, detail string) FTPTestResult {
		add(step, "error", detail)
		return FTPTestResult{OK: false, Steps: steps}
	}

	if cfg.Host == "" {
		return fail("Validate config", "host is empty")
	}
	if cfg.Username == "" {
		return fail("Validate config", "username is empty")
	}
	port := cfg.Port
	if port <= 0 {
		port = 21
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)
	add("Validate config", "ok", fmt.Sprintf("host=%s port=%d tls=%v", cfg.Host, port, cfg.TLS))

	// Build dial options
	dialOpts := []ftp.DialOption{
		ftp.DialWithTimeout(10 * time.Second),
	}
	if cfg.TLS {
		dialOpts = append(dialOpts, ftp.DialWithExplicitTLS(nil))
	}

	// Step: TCP connect + banner
	start := time.Now()
	conn, err := ftp.Dial(addr, dialOpts...)
	if err != nil {
		return fail("TCP connect / banner", err.Error())
	}
	elapsed := time.Since(start).Milliseconds()
	if cfg.TLS {
		add("TCP connect / TLS / banner", "ok", fmt.Sprintf("connected to %s in %d ms (FTPS)", addr, elapsed))
	} else {
		add("TCP connect / banner", "ok", fmt.Sprintf("connected to %s in %d ms", addr, elapsed))
	}

	// Step: Login
	if err := conn.Login(cfg.Username, cfg.Password); err != nil {
		conn.Quit() //nolint:errcheck
		// Unwrap textproto error for a cleaner message
		msg := err.Error()
		if te, ok := err.(*textproto.Error); ok {
			msg = fmt.Sprintf("%d %s", te.Code, te.Msg)
		}
		return fail("Login", msg)
	}
	add("Login", "ok", fmt.Sprintf("authenticated as %s", cfg.Username))

	// Step: Change directory (if remote path is set)
	if cfg.RemotePath != "" {
		if err := conn.ChangeDir(cfg.RemotePath); err != nil {
			conn.Quit() //nolint:errcheck
			return fail("Change directory", fmt.Sprintf("CWD %s: %v", cfg.RemotePath, err))
		}
		add("Change directory", "ok", fmt.Sprintf("CWD %s OK", cfg.RemotePath))
	} else {
		skip("Change directory", "no remote path configured — using server root")
	}

	// Step: Write test — upload a tiny sentinel file then delete it
	testFilename := "doppler_ftp_test.txt"
	testContent := fmt.Sprintf("ubersdr_doppler FTP test %s\n", time.Now().UTC().Format(time.RFC3339))
	if err := conn.Stor(testFilename, strings.NewReader(testContent)); err != nil {
		conn.Quit() //nolint:errcheck
		return fail("Write test", fmt.Sprintf("STOR %s: %v", testFilename, err))
	}
	// Delete the test file
	if err := conn.Delete(testFilename); err != nil {
		// Non-fatal — file was written successfully
		add("Write test", "ok", fmt.Sprintf("wrote and could not delete %s (non-fatal): %v", testFilename, err))
	} else {
		add("Write test", "ok", fmt.Sprintf("wrote and deleted %s", testFilename))
	}

	conn.Quit() //nolint:errcheck
	return FTPTestResult{OK: true, Steps: steps}
}

// ---------------------------------------------------------------------------
// Aligned ticker — fires at clock-boundary multiples of intervalMins
// ---------------------------------------------------------------------------

// nextAlignedTick returns the next UTC time that is a multiple of intervalMins
// from the start of the current UTC hour.  For example, with intervalMins=15
// and current time 18:07 UTC, it returns 18:15 UTC.
func nextAlignedTick(now time.Time, intervalMins int) time.Time {
	if intervalMins <= 0 {
		intervalMins = 15
	}
	// Truncate to the start of the current UTC hour
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC)
	// Find the next boundary after now
	for t := hourStart; ; t = t.Add(time.Duration(intervalMins) * time.Minute) {
		next := t.Add(time.Duration(intervalMins) * time.Minute)
		if next.After(now) {
			return next
		}
	}
}

// ---------------------------------------------------------------------------
// CSV builder — generates a Grape-format CSV for a time window
// ---------------------------------------------------------------------------

// buildPreviewCSV generates a Grape-format CSV covering [rangeStart, rangeEnd)
// for the given station label.  It reuses the same on-disk file reading and
// range-filtering logic as the /api/csv HTTP handler.
// Returns the CSV bytes and the filename to use for the upload.
func buildPreviewCSV(
	dataDir string,
	settings globalSettings,
	cfg stationConfig,
	rangeStart, rangeEnd time.Time,
) (data []byte, filename string, err error) {
	// Collect UTC days the window spans
	var days []time.Time
	d := time.Date(rangeStart.Year(), rangeStart.Month(), rangeStart.Day(), 0, 0, 0, 0, time.UTC)
	endDay := time.Date(rangeEnd.Year(), rangeEnd.Month(), rangeEnd.Day(), 0, 0, 0, 0, time.UTC)
	for !d.After(endDay) {
		days = append(days, d)
		d = d.Add(24 * time.Hour)
	}

	// Sanitise label for filename suffix matching
	csvLabel := strings.ReplaceAll(cfg.Label, " ", "")
	csvLabel = strings.ReplaceAll(csvLabel, "/", "-")
	suffix := "_FRQ_" + csvLabel + ".csv"

	// Gather matching CSV file paths across all required days
	var matchPaths []string
	for _, day := range days {
		dir := filepath.Join(dataDir,
			fmt.Sprintf("%04d", day.Year()),
			fmt.Sprintf("%02d", day.Month()),
			fmt.Sprintf("%02d", day.Day()),
		)
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), suffix) {
				matchPaths = append(matchPaths, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(matchPaths) == 0 {
		return nil, "", fmt.Errorf("no data files found for station %q in window %s–%s",
			cfg.Label, rangeStart.Format(time.RFC3339), rangeEnd.Format(time.RFC3339))
	}

	var buf bytes.Buffer
	firstFile := true

	for _, p := range matchPaths {
		fileDay := csvFileDay(p)

		f, openErr := os.Open(p)
		if openErr != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		pastHeader := false
		var headerLines []string

		for scanner.Scan() {
			line := scanner.Text()
			if !pastHeader {
				if line == "UTC,Freq,Vpk" {
					pastHeader = true
					if firstFile {
						for _, hl := range headerLines {
							buf.WriteString(hl)
							buf.WriteByte('\n')
						}
						buf.WriteString(line)
						buf.WriteByte('\n')
						firstFile = false
					}
				} else {
					if firstFile {
						headerLines = append(headerLines, line)
					}
				}
				continue
			}

			// Range-filter data rows
			ts, ok := csvRowTime(line, fileDay)
			if !ok {
				continue
			}
			if (ts.Equal(rangeStart) || ts.After(rangeStart)) && ts.Before(rangeEnd) {
				buf.WriteString(line)
				buf.WriteByte('\n')
			}
		}
		f.Close()
	}

	if buf.Len() == 0 {
		return nil, "", fmt.Errorf("no data rows in window %s–%s for station %q",
			rangeStart.Format(time.RFC3339), rangeEnd.Format(time.RFC3339), cfg.Label)
	}

	// Build filename: <start>_<end>_<node>_<grid>_FRQ_<station>.csv
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
	startStr := rangeStart.UTC().Format("2006-01-02T15:04:05Z")
	endStr := rangeEnd.UTC().Format("2006-01-02T15:04:05Z")
	fname := fmt.Sprintf("%s_%s_%s_%s_FRQ_%s.csv", startStr, endStr, node, grid, csvLabel)

	return buf.Bytes(), fname, nil
}

// ---------------------------------------------------------------------------
// FTP upload — connect, optionally CWD, STOR file, quit
// ---------------------------------------------------------------------------

func ftpUpload(cfg ftpSettings, filename string, data []byte) error {
	dialOpts := []ftp.DialOption{
		ftp.DialWithTimeout(30 * time.Second),
	}
	if cfg.TLS {
		dialOpts = append(dialOpts, ftp.DialWithExplicitTLS(nil))
	}

	port := cfg.Port
	if port <= 0 {
		port = 21
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)
	conn, err := ftp.Dial(addr, dialOpts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Quit() //nolint:errcheck

	if err := conn.Login(cfg.Username, cfg.Password); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	if cfg.RemotePath != "" {
		if err := conn.ChangeDir(cfg.RemotePath); err != nil {
			return fmt.Errorf("CWD %s: %w", cfg.RemotePath, err)
		}
	}

	if err := conn.Stor(filename, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("STOR %s: %w", filename, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Upload all stations — called once per tick
// ---------------------------------------------------------------------------

func uploadAllStations(
	mgr *stationManager,
	ftpCfg ftpSettings,
	settingsMu *sync.RWMutex,
	settings *globalSettings,
	rangeStart, rangeEnd time.Time,
) {
	settingsMu.RLock()
	s := *settings
	settingsMu.RUnlock()

	stations := mgr.list()
	uploaded := 0
	for _, ds := range stations {
		if !ds.cfg.Enabled {
			continue
		}
		data, fname, err := buildPreviewCSV(mgr.dataDir, s, ds.cfg, rangeStart, rangeEnd)
		if err != nil {
			log.Printf("[ftp] %s: build CSV: %v", ds.cfg.Label, err)
			continue
		}
		if err := ftpUpload(ftpCfg, fname, data); err != nil {
			log.Printf("[ftp] %s: upload %s: %v", ds.cfg.Label, fname, err)
			continue
		}
		log.Printf("[ftp] %s: uploaded %s (%d bytes)", ds.cfg.Label, fname, len(data))
		uploaded++
	}
	log.Printf("[ftp] upload cycle complete: %d/%d stations uploaded (window %s–%s)",
		uploaded, len(stations),
		rangeStart.UTC().Format("15:04:05"),
		rangeEnd.UTC().Format("15:04:05"))
}

// ---------------------------------------------------------------------------
// FTP uploader goroutine
// ---------------------------------------------------------------------------

// startFTPUploader runs the periodic FTP upload loop.
// It reads ftpCfg at each tick so changes saved via the API take effect
// without a restart.
func startFTPUploader(
	ctx context.Context,
	mgr *stationManager,
	ftpCfg *ftpSettings,
	ftpMu *sync.RWMutex,
	settings *globalSettings,
	settingsMu *sync.RWMutex,
	ftpCfgPath string,
) {
	log.Printf("[ftp] uploader goroutine started")
	for {
		// Read current FTP config
		ftpMu.RLock()
		cfg := *ftpCfg
		ftpMu.RUnlock()

		if !cfg.Enabled || cfg.Host == "" {
			// Not enabled — poll every 30 s to pick up config changes
			select {
			case <-ctx.Done():
				log.Printf("[ftp] uploader goroutine stopped")
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}

		// Wait until the next aligned tick
		now := time.Now().UTC()
		next := nextAlignedTick(now, cfg.IntervalMins)
		log.Printf("[ftp] next upload at %s UTC (interval %d min)",
			next.Format("15:04:05"), cfg.IntervalMins)

		select {
		case <-ctx.Done():
			log.Printf("[ftp] uploader goroutine stopped")
			return
		case <-time.After(time.Until(next)):
		}

		// Re-read config in case it changed while we were waiting
		ftpMu.RLock()
		cfg = *ftpCfg
		ftpMu.RUnlock()

		if !cfg.Enabled || cfg.Host == "" {
			continue
		}

		// Compute the window: [tick - windowMins, tick)
		tick := time.Now().UTC()
		// Snap tick back to the nearest aligned boundary so the window is
		// consistent even if we woke up a few milliseconds late.
		intervalD := time.Duration(cfg.IntervalMins) * time.Minute
		windowD := time.Duration(cfg.WindowMins) * time.Minute
		// Align tick to the interval boundary
		tickAligned := tick.Truncate(intervalD)
		rangeEnd := tickAligned
		rangeStart := rangeEnd.Add(-windowD)

		log.Printf("[ftp] starting upload cycle: window %s–%s",
			rangeStart.UTC().Format("15:04:05"),
			rangeEnd.UTC().Format("15:04:05"))

		uploadAllStations(mgr, cfg, settingsMu, settings, rangeStart, rangeEnd)
	}
}
