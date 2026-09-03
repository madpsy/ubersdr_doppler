package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// What the version 2 quantisation actually costs, measured on real bins.
//
//	LIVE_PROBE=1 PROBE_URL=https://host PROBE_FREQ=10000000 \
//	  go test -run TestLiveQuantisationCost -v .
//
// The server still serves the version 1 float32 frames when binary8 is not
// asked for, and those are the UNQUANTISED bins -- the same spectrum, before
// the 8-bit codes. So this takes real float32 frames off the wire, quantises a
// copy with the server's own scale rules (refChooseScale/refEncode, the same
// arithmetic spectrumV2Encode uses), and runs detectDoppler over both.
//
// That answers the question the synthetic simulation could only model: on the
// receiver's actual spectra, how far does the reported Doppler move, and how
// far do the SNR/SignalDBFS/NoiseDBFS figures written to CSV move.
//
// It matters because the Doppler figure is NOT the argmax bin. detectDopplerWithPeak
// picks the peak bin by argmax, but then reports
// (centroidBin - n/2) * binBandwidth, where centroidBin comes from a parabolic
// interpolation over the LINEAR POWER of bins[peak-1..peak+1], blended with a
// power-weighted centroid when the 3 dB range spans three bins or more. Both are
// amplitude-driven, so a change in bin amplitudes does move the reported
// frequency. The question is by how much, not whether.
func TestLiveQuantisationCost(t *testing.T) {
	if os.Getenv("LIVE_PROBE") == "" {
		t.Skip("set LIVE_PROBE=1 (with PROBE_URL, PROBE_FREQ) to run against a real receiver")
	}
	freq := 10_000_000
	if f, err := strconv.Atoi(os.Getenv("PROBE_FREQ")); err == nil && f > 0 {
		freq = f
	}
	binBWReq := specBinBandwidth
	if b, err := strconv.ParseFloat(os.Getenv("PROBE_BINBW"), 64); err == nil && b > 0 {
		binBWReq = b
	}

	ds := &DopplerStation{
		cfg:        stationConfig{Label: "probe", FreqHz: freq},
		ubersdrURL: os.Getenv("PROBE_URL"),
	}
	sessionID := uuid.New().String()
	if err := ds.checkConnection(sessionID); err != nil {
		t.Fatalf("connection: %v", err)
	}

	// The version 1 float32 path: user_session_id only, no mode=binary8.
	u, _ := url.Parse(ds.ubersdrURL)
	scheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		scheme = "wss"
	}
	q := url.Values{}
	q.Set("user_session_id", sessionID)
	wsAddr := fmt.Sprintf("%s://%s/ws/user-spectrum?%s", scheme, u.Host, q.Encode())
	t.Logf("dialling %s (version 1 float32, for unquantised bins)", wsAddr)

	conn, _, err := wsDialer.Dial(wsAddr, http.Header{"User-Agent": {"ubersdr_doppler/probe"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]interface{}{
		"type": "zoom", "frequency": ds.cfg.FreqHz, "binBandwidth": binBWReq,
	}); err != nil {
		t.Fatalf("zoom: %v", err)
	}

	binBW := binBWReq
	var bins []float32
	var dHz, dSNR, dSig, dNoise []float64
	var steps []int
	frames, compared := 0, 0

	deadline := time.Now().Add(30 * time.Second)
	conn.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if !isSpectrumFrame(msg) {
			if bw, ok := parseBinBandwidth(msg); ok {
				binBW = bw
				t.Logf("config: binBandwidth=%g Hz", bw)
			}
			continue
		}
		// Version 1 float32 frames: 22-byte header, flags 0x01 full / 0x02 delta.
		if len(msg) < 22 || msg[4] != 1 {
			continue
		}
		body := msg[22:]
		switch msg[5] {
		case 0x01:
			n := len(body) / 4
			bins = make([]float32, n)
			for i := 0; i < n; i++ {
				bins[i] = math.Float32frombits(binary.LittleEndian.Uint32(body[i*4:]))
			}
		case 0x02:
			if bins == nil || len(body) < 2 {
				continue
			}
			cnt := int(binary.LittleEndian.Uint16(body[0:2]))
			p := body[2:]
			if len(p) < cnt*6 {
				continue
			}
			for i := 0; i < cnt; i++ {
				idx := int(binary.LittleEndian.Uint16(p[i*6:]))
				if idx < len(bins) {
					bins[idx] = math.Float32frombits(binary.LittleEndian.Uint32(p[i*6+2:]))
				}
			}
		default:
			continue
		}
		if bins == nil || binBW > 100 { // still on the wide pre-zoom view
			continue
		}
		frames++

		// Quantise a copy exactly as the server's v2 encoder would.
		s := refChooseScale(bins)
		qbins := make([]float32, len(bins))
		for i, v := range bins {
			qbins[i] = float32((float64(s.refCentiDB) + float64(s.refEncode(v))*float64(s.stepCentiDB)) / 100)
		}
		steps = append(steps, int(s.stepCentiDB))

		n := len(bins)
		unwrap := func(b []float32) []float32 {
			out := make([]float32, n)
			copy(out[:n/2], b[n/2:])
			copy(out[n/2:], b[:n/2])
			return out
		}
		ref := detectDoppler(unwrap(bins), binBW, 10.0, 50.0, 0, 0)
		got := detectDoppler(unwrap(qbins), binBW, 10.0, 50.0, 0, 0)
		if !ref.Valid || !got.Valid {
			continue
		}
		compared++
		dHz = append(dHz, math.Abs(float64(got.DopplerHz-ref.DopplerHz)))
		dSNR = append(dSNR, math.Abs(float64(got.SNR-ref.SNR)))
		dSig = append(dSig, math.Abs(float64(got.SignalDBFS-ref.SignalDBFS)))
		dNoise = append(dNoise, math.Abs(float64(got.NoiseDBFS-ref.NoiseDBFS)))
	}

	if compared == 0 {
		t.Skipf("no valid signal to compare (%d frames seen); try a frequency with a carrier", frames)
	}
	report := func(name, unit string, v []float64) {
		sort.Float64s(v)
		var sum float64
		for _, x := range v {
			sum += x
		}
		t.Logf("  |d%s|: mean %.5f %s, median %.5f, p95 %.5f, max %.5f",
			name, sum/float64(len(v)), unit, v[len(v)/2], v[len(v)*95/100], v[len(v)-1])
	}
	sort.Ints(steps)
	t.Logf("%d frames, %d with a valid signal both ways; step %d..%d centidB (%.2f..%.2f dB)",
		frames, compared, steps[0], steps[len(steps)-1],
		float64(steps[0])/100, float64(steps[len(steps)-1])/100)
	report("Doppler", "Hz", dHz)
	report("SNR", "dB", dSNR)
	report("SignalDBFS", "dB", dSig)
	report("NoiseDBFS", "dB", dNoise)
}
