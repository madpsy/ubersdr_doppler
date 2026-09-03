package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
)

// Conformance tests for the version 2 spectrum decoder.
//
// No fixture of real v2 frames exists anywhere in the tree, so the frames here
// are built two ways and the two are checked against each other:
//
//   - by hand, byte for byte, from the layout documented in the server's
//     user_spectrum_v2.go (and matching the vectors in the reference client's
//     clients/tui/client_test.go, which uses the same -12000/50 scale and the
//     same LSB-first mask); and
//
//   - by refEncodeV2 below, which is the server's own encoder rules --
//     spectrumV2ChooseScale, scale.encode and the packet assembly from
//     spectrumV2Encode -- transcribed so a round trip can be driven over
//     generated data rather than over four bins chosen by hand.
//
// Testing a decoder only against frames produced by the same reading of the
// spec would prove nothing, which is why refEncodeV2's own output is pinned to
// a hardcoded packet (TestRefEncoderMatchesDocumentedLayout) and its scale
// choice to a hand-computed one (TestRefChooseScaleMatchesServerFormula).

// ── frame construction, mirroring clients/tui/client_test.go ────────────────

func buildSpecFrameSeq(flags byte, seq uint16, body []byte) []byte {
	msg := make([]byte, specHeaderSize, specHeaderSize+len(body))
	copy(msg, "SPEC")
	msg[4] = specProtocolVersion
	msg[5] = flags
	binary.LittleEndian.PutUint16(msg[6:8], seq)
	binary.LittleEndian.PutUint64(msg[8:16], 1234)
	binary.LittleEndian.PutUint64(msg[16:24], 7_100_000)
	return append(msg, body...)
}

// buildFullBody prefixes codes with the scale a full frame carries.
func buildFullBody(refCentiDB int16, stepCentiDB uint8, codes ...byte) []byte {
	body := make([]byte, 3, 3+len(codes))
	binary.LittleEndian.PutUint16(body[0:2], uint16(refCentiDB))
	body[2] = stepCentiDB
	return append(body, codes...)
}

// buildDeltaBody builds a mask-and-values body for n bins, LSB-first per byte
// exactly as the server writes it.
func buildDeltaBody(n int, changes map[int]byte) []byte {
	maskLen := (n + 7) / 8
	body := make([]byte, maskLen)
	for i := 0; i < n; i++ {
		if _, ok := changes[i]; ok {
			body[i>>3] |= 1 << (uint(i) & 7)
		}
	}
	for i := 0; i < n; i++ {
		if v, ok := changes[i]; ok {
			body = append(body, v)
		}
	}
	return body
}

func decodeOK(t *testing.T, d *spectrumDecoder, frame []byte) []float32 {
	t.Helper()
	bins, ok, err := d.decode(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok {
		t.Fatal("decode returned no bins for a frame that should have produced some")
	}
	out := make([]float32, len(bins))
	copy(out, bins)
	return out
}

func wantBins(t *testing.T, got, want []float32, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d bins, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: bins = %v, want %v", what, got, want)
		}
	}
}

// ── the four behaviours the protocol turns on ───────────────────────────────

// A full frame establishes both the codes and the scale they read against, a
// delta patches individual bins against that scale, and a later full frame may
// replace the scale -- the same codes then mean different decibels.
func TestSpectrumV2FullDeltaAndScaleChange(t *testing.T) {
	d := newSpectrumDecoder(4)

	// -120 dB reference, 0.5 dB steps: dB = -120 + code/2.
	full := buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-12000, 50, 10, 20, 30, 40))
	wantBins(t, decodeOK(t, d, full),
		[]float32{-115, -110, -105, -100}, "full frame")

	// A delta patches bin 0 only, leaving the others and the scale intact.
	delta := buildSpecFrameSeq(specFlagDelta, 2, buildDeltaBody(4, map[int]byte{0: 200}))
	wantBins(t, decodeOK(t, d, delta),
		[]float32{-20, -110, -105, -100}, "delta frame")

	// A second delta, two bins at once, still against the first frame's scale.
	delta2 := buildSpecFrameSeq(specFlagDelta, 3, buildDeltaBody(4, map[int]byte{1: 0, 3: 255}))
	wantBins(t, decodeOK(t, d, delta2),
		[]float32{-20, -120, -105, -120 + 127.5}, "second delta")

	// A new full frame carries a new scale. Identical codes, different decibels:
	// -60 dB reference, 2.55 dB steps.
	full2 := buildSpecFrameSeq(specFlagFull, 4, buildFullBody(-6000, 255, 10, 20, 30, 40))
	wantBins(t, decodeOK(t, d, full2),
		[]float32{-60 + 25.5, -60 + 51, -60 + 76.5, -60 + 102}, "rescaled full frame")

	// And a delta after the rescale reads against the NEW scale, not the old.
	delta3 := buildSpecFrameSeq(specFlagDelta, 5, buildDeltaBody(4, map[int]byte{2: 0}))
	wantBins(t, decodeOK(t, d, delta3),
		[]float32{-60 + 25.5, -60 + 51, -60, -60 + 102}, "delta after rescale")
}

// A gap in the sequence means frames were dropped for a slow reader, so the
// next delta describes a change from a state this client never reached.
// Applying it would corrupt those bins until something else happened to force a
// keyframe -- which is exactly how version 1 desynchronised permanently. The
// bins must be held untouched until the next full frame.
func TestSpectrumV2SequenceGapHoldsBinsUntilKeyframe(t *testing.T) {
	d := newSpectrumDecoder(4)

	decodeOK(t, d, buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-12000, 50, 10, 20, 30, 40)))
	wantBins(t, decodeOK(t, d, buildSpecFrameSeq(specFlagDelta, 2, buildDeltaBody(4, map[int]byte{0: 100}))),
		[]float32{-70, -110, -105, -100}, "delta before the gap")

	held := append([]uint8(nil), d.codes...)

	// Sequence 3 never arrived. Sequence 4 is a delta and must be discarded.
	gapped := buildSpecFrameSeq(specFlagDelta, 4, buildDeltaBody(4, map[int]byte{1: 255, 2: 255}))
	bins, ok, err := d.decode(gapped)
	if err != nil {
		t.Fatalf("a gapped delta is not an error: %v", err)
	}
	if ok || bins != nil {
		t.Fatal("a delta after a sequence gap was applied; it must be discarded")
	}
	if !bytes.Equal(d.codes, held) {
		t.Fatalf("codes changed across a discarded delta: %v, want %v", d.codes, held)
	}
	if d.Gaps != 1 {
		t.Fatalf("gap counter = %d, want 1", d.Gaps)
	}

	// Still desynchronised: an in-sequence delta is no help either, because the
	// state it describes a change from is still one we never reached.
	if _, ok, _ := d.decode(buildSpecFrameSeq(specFlagDelta, 5, buildDeltaBody(4, map[int]byte{3: 255}))); ok {
		t.Fatal("a delta while desynchronised was applied")
	}
	if !bytes.Equal(d.codes, held) {
		t.Fatalf("codes changed while desynchronised: %v, want %v", d.codes, held)
	}

	// The keyframe restates everything, which is what recovers the stream.
	wantBins(t, decodeOK(t, d, buildSpecFrameSeq(specFlagFull, 6, buildFullBody(-12000, 50, 1, 2, 3, 4))),
		[]float32{-119.5, -119, -118.5, -118}, "keyframe after the gap")
	if d.desynced {
		t.Fatal("a full frame did not clear the desynchronised state")
	}

	// And deltas work again from there.
	wantBins(t, decodeOK(t, d, buildSpecFrameSeq(specFlagDelta, 7, buildDeltaBody(4, map[int]byte{0: 5}))),
		[]float32{-117.5, -119, -118.5, -118}, "delta after recovery")
}

// The mask bits are LSB-first within each byte, and a bin count that is not a
// multiple of eight leaves spare bits in the last one. Walking it wrongly is
// the easiest way to decode a plausible but wrong spectrum.
func TestSpectrumV2DeltaMaskWalk(t *testing.T) {
	const n = 20
	d := newSpectrumDecoder(n)

	codes := make([]byte, n)
	for i := range codes {
		codes[i] = byte(i)
	}
	decodeOK(t, d, buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-10000, 100, codes...)))

	// Non-adjacent bins spanning all three mask bytes, including the last bit
	// of a byte and the first of the next.
	changes := map[int]byte{0: 200, 7: 201, 8: 202, 15: 203, 19: 204}
	got := decodeOK(t, d, buildSpecFrameSeq(specFlagDelta, 2, buildDeltaBody(n, changes)))

	want := make([]float32, n)
	for i := 0; i < n; i++ {
		c := byte(i)
		if v, ok := changes[i]; ok {
			c = v
		}
		want[i] = float32(-100 + float64(c)*1.0) // ref -100 dB, step 1.00 dB
	}
	wantBins(t, got, want, "mask walk")
}

// A delta arriving before any keyframe has nothing to patch. That is what a
// client joining mid-stream sees, for at most one keyframe interval, and it is
// not an error.
func TestSpectrumV2DeltaBeforeKeyframe(t *testing.T) {
	d := newSpectrumDecoder(4)
	bins, ok, err := d.decode(buildSpecFrameSeq(specFlagDelta, 1, buildDeltaBody(4, map[int]byte{0: 5})))
	if err != nil {
		t.Fatalf("a delta before the first keyframe is not an error: %v", err)
	}
	if ok || bins != nil {
		t.Fatal("a delta before the first keyframe produced bins")
	}
}

// Anything that is not a well-formed version 2 frame must be reported, not
// half-applied. A body whose length disagrees with its mask is the case that
// would otherwise leave the frame updated part way with nothing to say so.
func TestSpectrumV2RejectsMalformedFrames(t *testing.T) {
	full := buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-12000, 50, 10, 20, 30, 40))

	badMagic := append([]byte(nil), full...)
	copy(badMagic, "SPEQ")

	badVersion := append([]byte(nil), full...)
	badVersion[4] = 1 // a v1 frame, which we must refuse rather than misread

	zeroStep := buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-12000, 0, 10, 20))

	unknownFlags := buildSpecFrameSeq(0x03, 1, buildFullBody(-12000, 50, 10, 20))

	cases := []struct {
		name  string
		frame []byte
	}{
		{"short header", full[:20]},
		{"bad magic", badMagic},
		{"version 1", badVersion},
		{"zero step", zeroStep},
		{"unknown flags", unknownFlags},
		{"truncated full body", buildSpecFrameSeq(specFlagFull, 1, []byte{0x00, 0x00})},
		{"full frame with no bins", buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-12000, 50))},
	}
	for _, tc := range cases {
		d := newSpectrumDecoder(4)
		if _, ok, err := d.decode(tc.frame); err == nil || ok {
			t.Errorf("%s: decoded without error", tc.name)
		}
	}

	// Mask/value disagreements need a keyframe first so the bin count is known.
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"delta shorter than its mask", []byte{}},
		{"delta missing a value", []byte{0x03, 0x11}},          // two mask bits, one value
		{"delta with a spare value", []byte{0x01, 0x11, 0x22}}, // one mask bit, two values
	} {
		d := newSpectrumDecoder(4)
		decodeOK(t, d, full)
		before := append([]uint8(nil), d.codes...)
		if _, ok, err := d.decode(buildSpecFrameSeq(specFlagDelta, 2, tc.body)); err == nil || ok {
			t.Errorf("%s: decoded without error", tc.name)
		}
		if !bytes.Equal(d.codes, before) {
			t.Errorf("%s: bins were modified by a malformed frame", tc.name)
		}
	}
}

// A reconnect starts a new session whose first frame is a keyframe. Carrying
// the previous socket's codes and scale across would mix two streams.
func TestSpectrumV2ResetForgetsTheStream(t *testing.T) {
	d := newSpectrumDecoder(4)
	decodeOK(t, d, buildSpecFrameSeq(specFlagFull, 9, buildFullBody(-12000, 50, 10, 20, 30, 40)))
	d.reset()

	if d.codes != nil || d.haveSeq || d.desynced {
		t.Fatal("reset left stream state behind")
	}
	// A delta on the fresh connection has nothing to patch, and the sequence
	// restarting at 1 is not a gap.
	if _, ok, err := d.decode(buildSpecFrameSeq(specFlagDelta, 1, buildDeltaBody(4, map[int]byte{0: 5}))); err != nil || ok {
		t.Fatalf("post-reset delta: ok=%v err=%v, want false/nil", ok, err)
	}
	if d.Gaps != 0 {
		t.Fatalf("a reconnect counted as a sequence gap (%d)", d.Gaps)
	}
}

// Every control message on the spectrum endpoint is gzip-compressed JSON
// carried on a BINARY WebSocket frame -- the server's sendStatus ("config"),
// sendMessage ("pong") and sendError all go through writeJSONCompressed. So a
// read loop that demultiplexes on the WebSocket message type finds no control
// messages at all, and feeds every one of them to the spectrum decoder.
//
// "config" is the message carrying the binBandwidth the server actually chose,
// which is what converts a bin index into a frequency offset. Against a live
// receiver the session opens at 29296.875 Hz per bin and only reaches 0.5 after
// the zoom is applied, so this is not a theoretical difference.
func TestControlMessagesAreDemultiplexedByMagic(t *testing.T) {
	gz := func(s string) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	spec := buildSpecFrameSeq(specFlagFull, 1, buildFullBody(-12000, 50, 10, 20, 30, 40))
	if !isSpectrumFrame(spec) {
		t.Error("a SPEC frame was not recognised as one")
	}

	config := gz(`{"type":"config","centerFreq":10000000,"binCount":2048,"binBandwidth":0.5}`)
	if isSpectrumFrame(config) {
		t.Fatal("a gzip control message was taken for a spectrum frame")
	}
	if bw, ok := parseBinBandwidth(config); !ok || bw != 0.5 {
		t.Errorf("compressed config: got %v, %v; want 0.5, true", bw, ok)
	}

	// The wide default the session opens at, before the zoom lands.
	wide := gz(`{"type":"config","binBandwidth":29296.875}`)
	if bw, ok := parseBinBandwidth(wide); !ok || bw != 29296.875 {
		t.Errorf("wide config: got %v, %v; want 29296.875, true", bw, ok)
	}

	// Plain JSON on a text frame still works, in case a server sends one.
	if bw, ok := parseBinBandwidth([]byte(`{"type":"config","binBandwidth":2}`)); !ok || bw != 2 {
		t.Errorf("uncompressed config: got %v, %v; want 2, true", bw, ok)
	}

	// Anything that is not a config, and anything malformed, is simply not one.
	for _, msg := range [][]byte{
		gz(`{"type":"pong"}`),
		gz(`{"type":"error","error":"nope"}`),
		gz(`{"type":"config"}`),                  // no binBandwidth
		gz(`{"type":"config","binBandwidth":0}`), // zero is not a width
		[]byte("not json"),
		{0x1F, 0x8B, 0x08, 0x00, 0x00}, // truncated gzip
		nil,
	} {
		if bw, ok := parseBinBandwidth(msg); ok {
			t.Errorf("non-config message yielded binBandwidth %v", bw)
		}
		if isSpectrumFrame(msg) {
			t.Errorf("control message %q taken for a spectrum frame", msg)
		}
	}
}

// ── the server's own encoder rules, for a round trip over generated data ────

type refScale struct {
	refCentiDB  int16
	stepCentiDB uint8
}

// refChooseScale is spectrumV2ChooseScale from user_spectrum_v2.go.
func refChooseScale(data []float32) refScale {
	const (
		minStep    = 25
		maxStep    = 255
		defStep    = 50
		marginDB   = 6.0
		maxCodeVal = 255
	)
	if len(data) == 0 {
		return refScale{refCentiDB: -12800, stepCentiDB: defStep}
	}
	lo, hi := float64(data[0]), float64(data[0])
	for _, v := range data {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		if f < lo {
			lo = f
		}
		if f > hi {
			hi = f
		}
	}
	if math.IsNaN(lo) || math.IsInf(lo, 0) {
		lo = -128
	}
	if math.IsNaN(hi) || math.IsInf(hi, 0) || hi < lo {
		hi = lo
	}
	lo -= marginDB
	hi += marginDB

	refCenti := math.Floor(lo * 100)
	if refCenti < math.MinInt16 {
		refCenti = math.MinInt16
	}
	if refCenti > math.MaxInt16 {
		refCenti = math.MaxInt16
	}
	step := math.Ceil((hi - lo) * 100 / maxCodeVal)
	if step < minStep {
		step = minStep
	}
	if step > maxStep {
		step = maxStep
	}
	return refScale{refCentiDB: int16(refCenti), stepCentiDB: uint8(step)}
}

// refEncode is spectrumV2Scale.encode: round to nearest, clamp at both ends.
func (s refScale) refEncode(db float32) uint8 {
	f := float64(db)
	if math.IsNaN(f) {
		return 0
	}
	code := math.Round((f*100 - float64(s.refCentiDB)) / float64(s.stepCentiDB))
	if code < 0 {
		return 0
	}
	if code > 255 {
		return 255
	}
	return uint8(code)
}

// refEncodeV2 is spectrumV2Encode: it decides full versus delta the way the
// server does, and lays the packet out byte for byte the way the server does.
func refEncodeV2(st *refEncoderState, data []float32, seq uint16, deltaThresholdDB float64) []byte {
	n := len(data)
	full := st.previous == nil || len(st.previous) != n || st.forceFull || st.sinceFull >= 50

	scale := st.scale
	if full {
		scale = refChooseScale(data)
	}
	codes := make([]uint8, n)
	for i, v := range data {
		codes[i] = scale.refEncode(v)
	}
	if !full {
		// A finite reading outside the scale forces a re-key: only a full
		// frame may carry a new scale.
		for _, v := range data {
			f := float64(v)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				continue
			}
			c := (f*100 - float64(scale.refCentiDB)) / float64(scale.stepCentiDB)
			if c < 0 || c > 255 {
				full = true
				break
			}
		}
		if full {
			scale = refChooseScale(data)
			for i, v := range data {
				codes[i] = scale.refEncode(v)
			}
		}
	}

	var mask []byte
	var values []uint8
	if !full {
		maskLen := (n + 7) / 8
		mask = make([]byte, maskLen)
		threshold := deltaThresholdDB * 100 / float64(scale.stepCentiDB)
		for i := 0; i < n; i++ {
			if math.Abs(float64(codes[i])-float64(st.previous[i])) > threshold {
				mask[i>>3] |= 1 << (uint(i) & 7)
				values = append(values, codes[i])
			} else {
				// Unsent bins keep the value the client already holds.
				codes[i] = st.previous[i]
			}
		}
		if maskLen+len(values) >= n {
			full = true
			scale = refChooseScale(data)
			for i, v := range data {
				codes[i] = scale.refEncode(v)
			}
		}
	}

	var packet []byte
	if full {
		packet = make([]byte, specHeaderSize+3+n)
	} else {
		packet = make([]byte, specHeaderSize+len(mask)+len(values))
	}
	copy(packet[0:4], "SPEC")
	packet[4] = specProtocolVersion
	if full {
		packet[5] = specFlagFull
	} else {
		packet[5] = specFlagDelta
	}
	binary.LittleEndian.PutUint16(packet[6:8], seq)
	binary.LittleEndian.PutUint64(packet[8:16], 1234)
	binary.LittleEndian.PutUint64(packet[16:24], 7_100_000)

	off := specHeaderSize
	if full {
		binary.LittleEndian.PutUint16(packet[off:], uint16(scale.refCentiDB))
		packet[off+2] = scale.stepCentiDB
		copy(packet[off+3:], codes)
	} else {
		copy(packet[off:], mask)
		copy(packet[off+len(mask):], values)
	}

	st.scale = scale
	st.previous = codes
	st.forceFull = false
	if full {
		st.sinceFull = 0
	} else {
		st.sinceFull++
	}
	st.lastCodes, st.lastScale, st.lastFull = codes, scale, full
	return packet
}

type refEncoderState struct {
	scale     refScale
	previous  []uint8
	sinceFull int
	forceFull bool

	lastCodes []uint8
	lastScale refScale
	lastFull  bool
}

// The reference encoder is only worth trusting if its own output is pinned.
// This is the documented layout spelled out by hand: 24-byte header, then the
// scale and one code per bin.
func TestRefEncoderMatchesDocumentedLayout(t *testing.T) {
	st := &refEncoderState{}
	// Range -100..-10 dB → lo-6 = -106, hi+6 = -4, so ref = -10600 centidB and
	// step = ceil(10200/255) = 40 centidB.
	pkt := refEncodeV2(st, []float32{-100, -55, -10}, 1, 1.0)

	want := make([]byte, 24)
	copy(want, "SPEC")
	want[4] = 2    // version
	want[5] = 0x05 // full frame
	binary.LittleEndian.PutUint16(want[6:8], 1)
	binary.LittleEndian.PutUint64(want[8:16], 1234)
	binary.LittleEndian.PutUint64(want[16:24], 7_100_000)
	want = append(want, 0x98, 0xD6) // refCentiDB -10600, little-endian int16
	want = append(want, 40)         // stepCentiDB
	// (-100*100 + 10600)/40 = 15; (-55*100 + 10600)/40 = 127.5 → 128;
	// (-10*100 + 10600)/40 = 240.
	want = append(want, 15, 128, 240)

	if !bytes.Equal(pkt, want) {
		t.Fatalf("reference packet\n got %v\nwant %v", pkt, want)
	}

	// And the decoder reads back exactly those decibels.
	d := newSpectrumDecoder(3)
	got := decodeOK(t, d, pkt)
	wantDB := []float32{
		float32(-10600.0/100 + 15*40.0/100),
		float32(-10600.0/100 + 128*40.0/100),
		float32(-10600.0/100 + 240*40.0/100),
	}
	wantBins(t, got, wantDB, "hand-built packet")
}

// The scale formula, checked against a hand computation rather than against
// itself: floor((min-6)*100) for the reference, ceil(span*100/255) for the step.
func TestRefChooseScaleMatchesServerFormula(t *testing.T) {
	s := refChooseScale([]float32{-100, -42, -10})
	if s.refCentiDB != -10600 || s.stepCentiDB != 40 {
		t.Fatalf("scale = %+v, want {refCentiDB:-10600 stepCentiDB:40}", s)
	}
	// A span small enough that the 0.25 dB floor binds: -50..-49 → -56..-43,
	// span 1300 centidB, 1300/255 = 5.1 → below the 25 minimum.
	s = refChooseScale([]float32{-50, -49})
	if s.refCentiDB != -5600 || s.stepCentiDB != 25 {
		t.Fatalf("narrow-span scale = %+v, want {refCentiDB:-5600 stepCentiDB:25}", s)
	}
}

// The whole thing end to end: a stream of frames the server's rules produced,
// decoded, and compared to the decibels that went in. Quantisation is the only
// permitted difference, and it is bounded -- a full frame by half a step, a
// delta by the threshold the encoder was allowed to leave unsent.
func TestSpectrumV2RoundTripAgainstServerRules(t *testing.T) {
	const (
		bins             = 200 // what this program actually asks for
		frames           = 120 // more than the 50-frame keyframe interval
		deltaThresholdDB = 1.0
	)
	rng := rand.New(rand.NewSource(20260903))

	st := &refEncoderState{}
	d := newSpectrumDecoder(bins)

	data := make([]float32, bins)
	for i := range data {
		data[i] = float32(-110 + rng.Float64()*4)
	}

	sawFull, sawDelta := 0, 0
	for f := 0; f < frames; f++ {
		// Drift the noise floor and walk a carrier spike across the window, so
		// the encoder meets both quiet frames (deltas) and a scale it outgrows
		// (forced keyframes).
		for i := range data {
			data[i] += float32(rng.NormFloat64() * 0.3)
		}
		spike := 40 + f%120
		data[spike] = float32(-40 + rng.Float64()*2 + float64(f)/4)

		pkt := refEncodeV2(st, data, uint16(f+1), deltaThresholdDB)
		if st.lastFull {
			sawFull++
		} else {
			sawDelta++
		}

		got, ok, err := d.decode(pkt)
		if err != nil {
			t.Fatalf("frame %d: %v", f, err)
		}
		if !ok {
			t.Fatalf("frame %d produced no bins", f)
		}

		// The decoder must hold exactly the codes the encoder believes it
		// holds -- that equality is what makes the next delta correct.
		if !bytes.Equal(d.codes, st.lastCodes) {
			t.Fatalf("frame %d: decoder codes diverged from the encoder's", f)
		}
		if d.scale.refCentiDB != st.lastScale.refCentiDB || d.scale.stepCentiDB != st.lastScale.stepCentiDB {
			t.Fatalf("frame %d: scale = %+v, want %+v", f, d.scale, st.lastScale)
		}

		// Half a step of quantisation, plus whatever the delta threshold let
		// the encoder leave unsent.
		tol := float32(float64(st.lastScale.stepCentiDB)/200) + float32(deltaThresholdDB)
		for i := range data {
			if diff := got[i] - data[i]; diff > tol || diff < -tol {
				t.Fatalf("frame %d bin %d: got %.3f dB, want %.3f ± %.3f",
					f, i, got[i], data[i], tol)
			}
		}
	}

	// A stream that never took one branch would not have tested it.
	if sawFull < 2 || sawDelta < 10 {
		t.Fatalf("stream did not exercise both frame types: %d full, %d delta", sawFull, sawDelta)
	}
}
