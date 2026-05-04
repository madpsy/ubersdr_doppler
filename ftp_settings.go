// ftp_settings.go — FTP upload configuration for ubersdr_doppler
//
// Stored separately from settings.json so FTP credentials (host, username,
// password) are never returned by the unauthenticated GET /api/settings
// endpoint.  All /api/ftp/* endpoints require authentication.
package main

import (
	"encoding/json"
	"os"
)

// ftpSettings holds the FTP upload configuration.
// Persisted to <dataDir>/ftp_settings.json.
type ftpSettings struct {
	// Enabled controls whether the periodic FTP upload goroutine is active.
	Enabled bool `json:"enabled"`

	// Host is the FTP server address including port, e.g. "ftp.example.com:21".
	Host string `json:"host"`

	// Username and Password for FTP authentication.
	Username string `json:"username"`
	Password string `json:"password"`

	// RemotePath is the directory on the FTP server to upload files into,
	// e.g. "/uploads/doppler".  Leave empty to upload to the root.
	RemotePath string `json:"remote_path"`

	// IntervalMins is how often (in minutes) to upload preview CSVs.
	// Uploads are aligned to clock boundaries (e.g. :00, :15, :30, :45 UTC).
	// Default: 15.  Accepted values: 1, 5, 10, 15, 30, 60.
	IntervalMins int `json:"interval_mins"`

	// WindowMins is the time span of data included in each preview CSV.
	// Default: 15 (last 15 minutes of data).
	WindowMins int `json:"window_mins"`

	// TLS enables explicit FTPS (AUTH TLS) if true.
	TLS bool `json:"tls"`
}

// defaultFTPSettings returns sensible defaults.
func defaultFTPSettings() ftpSettings {
	return ftpSettings{
		Enabled:      false,
		IntervalMins: 15,
		WindowMins:   15,
		TLS:          false,
	}
}

// loadFTPSettings reads ftp_settings.json; returns defaults if file does not exist.
func loadFTPSettings(path string) (ftpSettings, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return defaultFTPSettings(), nil
	}
	if err != nil {
		return defaultFTPSettings(), err
	}
	var s ftpSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultFTPSettings(), err
	}
	// Apply defaults for zero values
	if s.IntervalMins <= 0 {
		s.IntervalMins = 15
	}
	if s.WindowMins <= 0 {
		s.WindowMins = 15
	}
	return s, nil
}

// saveFTPSettings writes ftp_settings.json atomically.
func saveFTPSettings(path string, s ftpSettings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // 0600 — credentials file
		return err
	}
	return os.Rename(tmp, path)
}
