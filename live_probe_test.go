package main

import (
	"net/http"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A probe against a real receiver, skipped unless asked for:
//
//	LIVE_PROBE=1 PROBE_URL=https://host PROBE_FREQ=10000000 \
//	  go test -run TestLiveSpectrumProbe -v .
//
// The synthetic tests prove the decoder matches the format as documented. Only
// a real server proves the format is what the documentation says, and that the
// rest of the session -- the zoom, the config reply, the frame cadence -- works
// end to end. It is what found that this endpoint sends its control messages as
// gzip-compressed JSON on BINARY frames, which no reading of the wire format
// would have suggested.
//
// It reports the quantisation scale the server derives per keyframe, because
// that is what sets the resolution of every bin the Doppler measurement reads.
func TestLiveSpectrumProbe(t *testing.T) {
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
	wsAddr := ds.spectrumWSURL(sessionID)
	t.Logf("dialling %s", wsAddr)

	conn, _, err := wsDialer.Dial(wsAddr, http.Header{"User-Agent": {"ubersdr_doppler/probe"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]interface{}{
		"type":         "zoom",
		"frequency":    ds.cfg.FreqHz,
		"binBandwidth": binBWReq,
	}); err != nil {
		t.Fatalf("zoom: %v", err)
	}

	dec := newSpectrumDecoder(specBinCount)
	deadline := time.Now().Add(30 * time.Second)
	conn.SetReadDeadline(deadline)

	binBW := binBWReq
	fullFrames, deltaFrames, configs := 0, 0, 0
	steps := map[uint8]int{}
	var spanLo, spanHi float64
	for time.Now().Before(deadline) {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if !isSpectrumFrame(msg) {
			if bw, ok := parseBinBandwidth(msg); ok {
				configs++
				binBW = bw
				t.Logf("config: binBandwidth=%g Hz", bw)
			}
			continue
		}
		isFull := len(msg) > 5 && msg[5] == specFlagFull
		bins, ok, derr := dec.decode(msg)
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		if !ok {
			continue
		}
		if !isFull {
			deltaFrames++
			continue
		}
		fullFrames++

		lo, hi := bins[0], bins[0]
		for _, v := range bins {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		steps[dec.scale.stepCentiDB]++
		if spanHi == 0 || float64(hi-lo) > spanHi {
			spanHi = float64(hi - lo)
		}
		if spanLo == 0 || float64(hi-lo) < spanLo {
			spanLo = float64(hi - lo)
		}
		if fullFrames > 6 {
			continue
		}
		n := len(bins)
		unwrapped := make([]float32, n)
		copy(unwrapped[:n/2], bins[n/2:])
		copy(unwrapped[n/2:], bins[:n/2])
		sorted := append([]float32(nil), unwrapped...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		r := detectDoppler(unwrapped, binBW, 10.0, 50.0, 0, 0)
		t.Logf("FULL #%d bins=%d ref=%d step=%d centidB (%.2f dB) | span %.2f..%.2f dB (%.2f dB) | P5=%.2f | SNR=%.2f sig=%.2f noise=%.2f doppler=%+.3f Hz valid=%v",
			fullFrames, n, dec.scale.refCentiDB, dec.scale.stepCentiDB,
			float64(dec.scale.stepCentiDB)/100, lo, hi, hi-lo,
			sorted[len(sorted)*5/100], r.SNR, r.SignalDBFS, r.NoiseDBFS, r.DopplerHz, r.Valid)
	}

	t.Logf("frames: %d full, %d delta, %d config; sequence gaps: %d",
		fullFrames, deltaFrames, configs, dec.Gaps)
	t.Logf("bin span over all full frames: %.2f..%.2f dB", spanLo, spanHi)
	keys := make([]int, 0, len(steps))
	for k := range steps {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		t.Logf("  step %3d centidB (%.2f dB): %d full frames", k, float64(k)/100, steps[uint8(k)])
	}
	if fullFrames == 0 {
		t.Fatal("no full frames observed")
	}
	if configs == 0 {
		t.Error("no config message decoded: binBandwidth would stay at the compiled-in default")
	}
}
