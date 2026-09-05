package main

import (
	"fmt"
	"net/url"
	"testing"
)

// The protocol a socket speaks is chosen entirely in its query string, and
// getting it wrong is silent in both directions:
//
//   - The spectrum endpoint serves version 2 only when binary8 is asked for as
//     well (user_spectrum_websocket.go: `useV2 := spectrumVersion >= 2 &&
//     useBinary8`). version=2 alone leaves the session on version 1 float32
//     frames, which the v2 decoder would reject as a bad version byte, frame
//     after frame, with the spectrum simply never updating.
//
//   - The audio endpoint on a server before 0.1.63 clamps an unsupported
//     version instead of refusing it, so version=4 unheeded means zstd frames
//     arriving at a decoder that reads none.
//
// Hence a test on the URLs themselves rather than only on the decoders.

func testStation() *DopplerStation {
	return &DopplerStation{
		cfg:        stationConfig{Label: "WWV-10", FreqHz: 10_000_000},
		ubersdrURL: "https://example.invalid/ws",
	}
}

func queryOf(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func TestSpectrumURLRequestsBinary8AndVersion2(t *testing.T) {
	raw := testStation().spectrumWSURL("session-abc")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.Scheme != "wss" || u.Path != "/ws/user-spectrum" {
		t.Fatalf("spectrum URL = %q, want wss://…/ws/user-spectrum", raw)
	}
	q := u.Query()
	// Both, and both matching what the decoder in spectrum_v2.go reads.
	if got := q.Get("mode"); got != "binary8" {
		t.Errorf("mode = %q, want binary8 (version 2 is defined only for the 8-bit path)", got)
	}
	if got := q.Get("version"); got != "2" {
		t.Errorf("version = %q, want 2", got)
	}
	if specProtocolVersion != 2 {
		t.Errorf("decoder reads version %d but the URL asks for 2", specProtocolVersion)
	}
	if got := q.Get("user_session_id"); got != "session-abc" {
		t.Errorf("user_session_id = %q, want session-abc", got)
	}
}

func TestAudioURLsRequestVersion4(t *testing.T) {
	ds := testStation()

	// The usb preview socket, dialled 1 kHz low so the carrier lands at 1000 Hz.
	preview := queryOf(t, ds.audioWSURL("usb", audioPreviewParams(), "s", ds.cfg.FreqHz-1000))
	if got := preview.Get("version"); got != "4" {
		t.Errorf("audio preview version = %q, want 4", got)
	}
	if got := preview.Get("format"); got != "pcm-zstd" {
		t.Errorf("audio preview format = %q, want pcm-zstd (the name version 4 kept)", got)
	}
	if got := preview.Get("mode"); got != "usb" {
		t.Errorf("audio preview mode = %q, want usb", got)
	}
	if got := preview.Get("frequency"); got != "9999000" {
		t.Errorf("audio preview frequency = %q, want 9999000 (carrier - 1000)", got)
	}
	if preview.Get("bandwidthLow") != "300" || preview.Get("bandwidthHigh") != "1500" {
		t.Errorf("audio preview passband = %q..%q, want 300..1500",
			preview.Get("bandwidthLow"), preview.Get("bandwidthHigh"))
	}

	// The iq socket, tuned to the carrier itself with a 12 kHz window.
	iq := queryOf(t, ds.audioWSURL("iq", iqStreamParams(), "s", 0))
	if got := iq.Get("version"); got != "4" {
		t.Errorf("iq version = %q, want 4", got)
	}
	if got := iq.Get("format"); got != "pcm-zstd" {
		t.Errorf("iq format = %q, want pcm-zstd", got)
	}
	if got := iq.Get("mode"); got != "iq" {
		t.Errorf("iq mode = %q, want iq", got)
	}
	if got := iq.Get("frequency"); got != "10000000" {
		t.Errorf("iq frequency = %q, want the nominal carrier 10000000", got)
	}
	if iq.Get("bandwidthLow") != "-6000" || iq.Get("bandwidthHigh") != "6000" {
		t.Errorf("iq window = %q..%q, want -6000..6000",
			iq.Get("bandwidthLow"), iq.Get("bandwidthHigh"))
	}

	// The reduced-depth request has to be on the URL: the server's own default
	// is lossless, so a socket that does not ask silently costs about twice the
	// bandwidth for I/Q whose extra depth sits below the band noise.
	if got := iq.Get("min_margin"); got != fmt.Sprintf("%d", minMarginDefaultDB) {
		t.Errorf("iq min_margin = %q, want %d", got, minMarginDefaultDB)
	}
	// And not on the demodulated socket, where the server ignores it anyway.
	if got := preview.Get("min_margin"); got != "" {
		t.Errorf("audio preview min_margin = %q, want it absent", got)
	}
}

// Lossless is asked for by omitting the parameter, not by sending a zero: a
// server older than 0.1.64 ignores min_margin entirely, so omission is the only
// form that means the same thing to both.
func TestIQURLOmitsMinMarginWhenLossless(t *testing.T) {
	saved := iqMinMarginDB
	iqMinMarginDB = 0
	defer func() { iqMinMarginDB = saved }()

	iq := queryOf(t, testStation().audioWSURL("iq", iqStreamParams(), "s", 0))
	if _, ok := iq["min_margin"]; ok {
		t.Errorf("min_margin = %q, want it absent when lossless", iq.Get("min_margin"))
	}
}
