package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
)

// ---------------------------------------------------------------------------
// Spectrum wire protocol version 2
// ---------------------------------------------------------------------------
//
// Requested as mode=binary8&version=2. Both are required: the server defines
// version 2 only for the 8-bit path (user_spectrum_websocket.go computes
// `useV2 := spectrumVersion >= 2 && useBinary8`), so asking for the version
// alone silently leaves the session on version 1 float32 frames.
//
//	[magic "SPEC"]        4
//	[version = 2]         1
//	[flags]               1   0x05 full, 0x06 delta
//	[sequence uint16]     2   increments per frame; a gap means frames were lost
//	[timestamp uint64]    8   nanoseconds
//	[centreFreq uint64]   8   Hz, authoritative for this frame
//	                     24
//
//	full  0x05: [refCentiDB int16][stepCentiDB uint8][code uint8 × bins]
//	delta 0x06: [mask ⌈bins/8⌉ bytes][value uint8 per set bit]
//
//	dB = refCentiDB/100 + code × stepCentiDB/100
//
// The scale travels with each full frame, because the values move with the
// receiver's gain settings and a fixed window would clip on some configuration.
// A delta carries no scale and refers to the last full frame's, so the scale
// may only change on a full frame.
//
// The sequence number is what version 1 lacked. The server's send is a
// non-blocking write that drops the frame when a client is slow, and version 1
// recorded what the client would hold BEFORE attempting the send, so a dropped
// delta desynchronised those bins permanently with nothing to detect it. Here a
// gap is visible, and the response is to hold the bins until the next full
// frame rather than apply a delta describing a change from a state we never
// reached. A keyframe is at most 50 frames -- five seconds at 10 Hz -- away.

const (
	specProtocolVersion = 2
	specHeaderSize      = 24
	specFlagFull        = 0x05
	specFlagDelta       = 0x06
)

// isSpectrumFrame reports whether a WebSocket message is a spectrum frame
// rather than a control message.
//
// The spectrum endpoint sends BOTH on binary frames: the frames below, and
// gzip-compressed JSON from the server's writeJSONCompressed, which is how
// "config", "pong" and errors arrive. The magic is the only thing that
// separates them -- the message type does not.
func isSpectrumFrame(msg []byte) bool {
	return len(msg) >= 4 && msg[0] == 'S' && msg[1] == 'P' && msg[2] == 'E' && msg[3] == 'C'
}

// parseBinBandwidth pulls binBandwidth out of a "config" control message,
// gunzipping it first when it arrives compressed (which is how the server
// sends every control message on this endpoint).
//
// This is the bin width the server actually chose, which is not necessarily
// the one that was asked for: the server quantises it. Every bin index the
// Doppler measurement turns into a frequency offset is scaled by this, so a
// stale value is a systematically wrong Doppler reading rather than a missing
// one.
func parseBinBandwidth(msg []byte) (float64, bool) {
	if len(msg) >= 2 && msg[0] == 0x1F && msg[1] == 0x8B {
		zr, err := gzip.NewReader(bytes.NewReader(msg))
		if err != nil {
			return 0, false
		}
		defer zr.Close()
		plain, err := io.ReadAll(io.LimitReader(zr, 1<<20))
		if err != nil {
			return 0, false
		}
		msg = plain
	}
	var cfg struct {
		Type         string  `json:"type"`
		BinBandwidth float64 `json:"binBandwidth"`
	}
	if err := json.Unmarshal(msg, &cfg); err != nil {
		return 0, false
	}
	if cfg.Type != "config" || cfg.BinBandwidth <= 0 {
		return 0, false
	}
	return cfg.BinBandwidth, true
}

// spectrumScale converts an 8-bit code back to dBFS. Centidecibels throughout,
// so the arithmetic matches the server's exactly.
type spectrumScale struct {
	refCentiDB  int16
	stepCentiDB uint8
}

func (s spectrumScale) dB(code uint8) float32 {
	return float32(float64(s.refCentiDB)/100 + float64(code)*float64(s.stepCentiDB)/100)
}

// ---------------------------------------------------------------------------
// spectrumDecoder — per-connection state for the version 2 protocol
// ---------------------------------------------------------------------------

type spectrumDecoder struct {
	scale spectrumScale
	codes []uint8   // the codes the last accepted frame left us holding
	bins  []float32 // dequantised dB, reused across frames

	lastSeq uint16
	haveSeq bool

	// desynced is set when a sequence gap proves a frame was lost. While it is
	// set every delta is discarded rather than applied to bins that no longer
	// match what the server thinks we hold; the next full frame clears it.
	desynced bool

	// Gaps counts sequence discontinuities, for logging.
	Gaps uint64
}

func newSpectrumDecoder(binCount int) *spectrumDecoder {
	return &spectrumDecoder{bins: make([]float32, 0, binCount)}
}

// reset discards everything the stream established, for reuse across a
// reconnect: the codes, the scale that interprets them and the sequence run.
// A new session starts with a full frame, so nothing is lost by forgetting.
func (d *spectrumDecoder) reset() {
	d.scale = spectrumScale{}
	d.codes = nil
	d.bins = d.bins[:0]
	d.lastSeq, d.haveSeq = 0, false
	d.desynced = false
}

// decode parses one binary "SPEC" frame and updates the internal bin state.
//
// Returns the current bins (a slice into the decoder's own buffer, valid until
// the next call) and true when this frame produced a usable spectrum. A frame
// that is merely unusable -- a delta before the first keyframe, or a delta
// while desynced -- returns false with no error; only a malformed frame
// returns one.
func (d *spectrumDecoder) decode(data []byte) ([]float32, bool, error) {
	if len(data) < specHeaderSize {
		return nil, false, fmt.Errorf("spectrum frame too short: %d bytes", len(data))
	}
	if data[0] != 'S' || data[1] != 'P' || data[2] != 'E' || data[3] != 'C' {
		return nil, false, fmt.Errorf("spectrum frame has bad magic %q", data[0:4])
	}
	// A version byte that is not 2 is an error rather than a fallback: the
	// server refuses a version it cannot serve, so anything else means the
	// frame is not what it claims to be.
	if v := data[4]; v != specProtocolVersion {
		return nil, false, fmt.Errorf("unsupported spectrum version %d (this client reads %d only)",
			v, specProtocolVersion)
	}

	flags := data[5]
	sequence := binary.LittleEndian.Uint16(data[6:8])
	// data[8:16] timestamp nanoseconds, data[16:24] centre frequency Hz — the
	// Doppler measurement takes its frequency reference from the config
	// message and its timing from arrival, so neither is read here.
	body := data[specHeaderSize:]

	if d.haveSeq && sequence != d.lastSeq+1 {
		d.Gaps++
		d.desynced = true
	}
	d.lastSeq, d.haveSeq = sequence, true

	switch flags {
	case specFlagFull:
		if len(body) < 3 {
			return nil, false, fmt.Errorf("truncated full frame: %d bytes", len(body))
		}
		step := body[2]
		if step == 0 {
			return nil, false, fmt.Errorf("full frame declares a zero quantisation step")
		}
		codes := body[3:]
		if len(codes) == 0 {
			return nil, false, fmt.Errorf("full frame carries no bins")
		}
		d.scale = spectrumScale{
			refCentiDB:  int16(binary.LittleEndian.Uint16(body[0:2])),
			stepCentiDB: step,
		}
		d.codes = append(d.codes[:0], codes...)
		// A keyframe restates everything, which is exactly what recovers a
		// stream that lost a frame.
		d.desynced = false

	case specFlagDelta:
		if d.codes == nil {
			// A client that joins mid-stream sees this until the next
			// keyframe, at most five seconds away. Not an error.
			return nil, false, nil
		}
		n := len(d.codes)
		maskLen := (n + 7) / 8
		if len(body) < maskLen {
			return nil, false, fmt.Errorf("delta frame shorter than its mask: %d < %d", len(body), maskLen)
		}
		// The mask is validated whole before a single bin is touched: a body
		// whose length disagrees with it is malformed, and applying it part way
		// would leave the frame half updated with no way to tell.
		expected := 0
		for _, b := range body[:maskLen] {
			expected += bits.OnesCount8(b)
		}
		if len(body) != maskLen+expected {
			return nil, false, fmt.Errorf("delta frame carries %d values for %d mask bits",
				len(body)-maskLen, expected)
		}
		if d.desynced {
			// Frames were lost, so this delta describes a change from a state
			// we never reached. Hold what we have and wait for the keyframe.
			return nil, false, nil
		}
		vi := maskLen
		for i := 0; i < n; i++ {
			if body[i>>3]&(1<<(uint(i)&7)) != 0 {
				d.codes[i] = body[vi]
				vi++
			}
		}

	default:
		return nil, false, fmt.Errorf("unknown spectrum flags 0x%02x", flags)
	}

	if cap(d.bins) < len(d.codes) {
		d.bins = make([]float32, len(d.codes))
	}
	d.bins = d.bins[:len(d.codes)]
	for i, c := range d.codes {
		d.bins[i] = d.scale.dB(c)
	}
	return d.bins, true, nil
}
