package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"
)

// Conformance tests for the protocol version 4 receive path, through the
// pcmDecoder wrapper this program actually calls.
//
// testdata/pcmv4_stream.bin is a packet stream the SERVER's encoder produced,
// and pcmv4ExpectedSHA is the SHA-256 of the samples that went into it, little
// endian, exactly as pcmDecoder renders them. The same fixture and hash are
// used by the Go, C++, Python and JavaScript ports of this decoder.
//
// It earns its 90 kB. The version 4 predictor is backward adaptive: the two
// ends derive their filter taps independently from the samples already coded
// and never exchange a coefficient, so any arithmetic difference between this
// decoder and the server's produces plausible NOISE rather than an error.
// Nothing short of comparing the samples would catch it -- the Doppler
// measurement takes its readings from the spectrum socket, so a broken audio
// path would show up only as a preview stream that sounds wrong, and a broken
// IQ path only as an analysis canvas full of hash.
//
// The stream covers what the format can do: ordinary mono audio, silent packets
// carrying no body, an escape to verbatim samples on incompressible noise, a
// sample-rate change, and interleaved I/Q -- so it exercises both of the
// sockets this program opens, the usb preview and the iq stream.
const pcmv4ExpectedSHA = "4875d2185f1ff5a2031386c569cac0c2259e6a827b9e61f813399a19c3b9c903"

// testdata/pcmv4_scaled.bin is the same for the reduced-depth IQ profile, and
// the hash is the server's own, from ka9q_ubersdr/clients/rtl_sdr/pcmv4_test.go.
const pcmv4ScaledSHA = "7315366ceed3e70552c28d31cde690a14dc66f5244b5a8dc34a5e696f5698ccc"

// testdata/pcmv4_rice_edge.bin covers what a recording of ordinary traffic will
// not: a Rice codeword whose unary run is exactly 63 bits long, counted out of
// a full 64-bit accumulator. It appeared about once in a quarter of a million
// packets on live IQ -- often enough to break a receiver in minutes, rare
// enough that a recorded fixture holds one only by luck.
//
// The hash is the server's own, from
// ka9q_ubersdr/clients/soapy_driver/test/run.sh, not a value derived from any
// decoder.
const pcmv4RiceEdgeSHA = "3413109ff6d06d44fb8fa44c84595b776f5570f05663b762830853ddc0183527"

// readV4Fixture returns the packets in a fixture file.
//
// Layout: "UV4F", a format byte, a uint32 packet count, then each packet as a
// uint32 length and that many bytes.
func readV4Fixture(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(raw) < 9 || string(raw[:4]) != "UV4F" || raw[4] != 0 {
		t.Fatal("fixture: bad header")
	}
	count := int(binary.LittleEndian.Uint32(raw[5:]))
	off := 9

	packets := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if off+4 > len(raw) {
			t.Fatalf("fixture: truncated length at packet %d", i)
		}
		n := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		if off+n > len(raw) {
			t.Fatalf("fixture: truncated packet %d", i)
		}
		packets = append(packets, raw[off:off+n])
		off += n
	}
	if off != len(raw) {
		t.Fatalf("fixture: %d trailing bytes", len(raw)-off)
	}
	return packets
}

func TestPCMv4DecodesServerStream(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_stream.bin")
	dec := newPCMDecoder()
	h := sha256.New()

	// Every distinct (rate, channels) the fixture passes through, in order. A
	// decoder that lost the carried-forward metadata could still hash correctly
	// while mislabelling the stream, and the rate is what the WAV headers this
	// program serves to the browser are built from.
	wantParams := [][2]int{{12000, 1}, {24000, 1}, {384000, 2}}
	var gotParams [][2]int

	mono, iq := 0, 0
	for i, pkt := range packets {
		p, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if len(p.pcm) == 0 || len(p.pcm)%(2*p.channels) != 0 {
			t.Fatalf("packet %d: %d bytes is not whole frames of %d channels", i, len(p.pcm), p.channels)
		}
		switch p.channels {
		case 1:
			mono++
		case 2:
			// The iq socket takes these as interleaved S16LE I/Q pairs and
			// hands them straight to the browser, so a pair is four bytes.
			if len(p.pcm)%4 != 0 {
				t.Fatalf("packet %d: %d bytes of I/Q is not whole pairs", i, len(p.pcm))
			}
			iq++
		default:
			t.Fatalf("packet %d: %d channels", i, p.channels)
		}
		q := [2]int{p.sampleRate, p.channels}
		if len(gotParams) == 0 || gotParams[len(gotParams)-1] != q {
			gotParams = append(gotParams, q)
		}
		h.Write(p.pcm)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4ExpectedSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s", got, pcmv4ExpectedSHA)
	}
	if len(gotParams) != len(wantParams) {
		t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
	}
	for i := range wantParams {
		if gotParams[i] != wantParams[i] {
			t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
		}
	}
	// Both sockets this program opens are covered by the one fixture.
	if mono == 0 || iq == 0 {
		t.Fatalf("fixture exercised %d mono and %d I/Q packets; both paths must be covered", mono, iq)
	}
}

// testdata/pcmv4_scaled.bin is the reduced-depth IQ the iq socket asks for with
// min_margin: profile 2, where a shift byte leads the body and the decoded
// samples are shifted back left by it on the way out. The fixture crosses
// between profile 2 and plain profile 0 part way through, as a margin change on
// a live socket does.
//
// A shift read wrongly does not fail; it delivers I/Q several bits too quiet,
// which the phase measurement would report as a station that had faded.
func TestPCMv4DecodesScaledStream(t *testing.T) {
	dec := newPCMDecoder()
	h := sha256.New()
	for i, pkt := range readV4Fixture(t, "pcmv4_scaled.bin") {
		p, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if p.channels != 2 || len(p.pcm)%4 != 0 {
			t.Fatalf("packet %d: %d bytes over %d channels is not whole I/Q pairs", i, len(p.pcm), p.channels)
		}
		h.Write(p.pcm)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4ScaledSHA {
		t.Fatalf("scaled stream decoded wrongly\n got %s\nwant %s", got, pcmv4ScaledSHA)
	}
}

func TestPCMv4DecodesRiceEdgeStream(t *testing.T) {
	dec := newPCMDecoder()
	h := sha256.New()
	for i, pkt := range readV4Fixture(t, "pcmv4_rice_edge.bin") {
		p, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		h.Write(p.pcm)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4RiceEdgeSHA {
		t.Fatalf("rice-edge stream decoded wrongly\n got %s\nwant %s", got, pcmv4RiceEdgeSHA)
	}
}

// decodePrefix hashes the samples of the first n packets, or reports the first
// error. A prefix rather than the whole stream on purpose: the fixture changes
// codec profile part way through, and PCMv4StreamDecoder rebuilds its codec
// when the profile changes, so a replay of the WHOLE stream through a
// carried-over decoder resets itself by accident and reproduces the right hash
// for the wrong reason. Fifty packets stay inside one profile.
func decodePrefix(t *testing.T, dec *pcmDecoder, packets [][]byte, n int) (string, error) {
	t.Helper()
	h := sha256.New()
	for i := 0; i < n; i++ {
		p, err := dec.decode(packets[i])
		if err != nil {
			return "", err
		}
		h.Write(p.pcm)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// A decoder carries the adaptation state of its predictor, so it belongs to one
// socket. reset is what a reconnect calls, and it has to actually restore the
// initial state -- both halves of that are tested here, because a reset that
// did nothing would pass the first half alone.
func TestPCMv4DecoderResetMatchesAFreshDecoder(t *testing.T) {
	packets := readV4Fixture(t, "pcmv4_stream.bin")
	const prefix = 50

	fresh, err := decodePrefix(t, newPCMDecoder(), packets, prefix)
	if err != nil {
		t.Fatalf("fresh decoder: %v", err)
	}

	// Reset after a run must be indistinguishable from never having run.
	reused := newPCMDecoder()
	if _, err := decodePrefix(t, reused, packets, prefix); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	reused.reset()
	afterReset, err := decodePrefix(t, reused, packets, prefix)
	if err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if afterReset != fresh {
		t.Fatalf("reset decoder produced %s, a fresh one %s", afterReset, fresh)
	}

	// And without the reset it must NOT match, or the reset would be proving
	// nothing: the predictor has adapted to the first pass, and replaying the
	// stream against those taps is exactly the corruption a reconnect would
	// suffer.
	carried := newPCMDecoder()
	if _, err := decodePrefix(t, carried, packets, prefix); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	again, err := decodePrefix(t, carried, packets, prefix)
	if err == nil && again == fresh {
		t.Fatal("a carried-over decoder reproduced the fresh decoder's samples; " +
			"the reset test above proves nothing")
	}
}

// A server too old for version 4 answers with the zstd-wrapped version 1 shape
// -- versions before 0.1.63 clamp the requested version instead of refusing it.
// Naming that is what turns a dead stream into a message the operator can act
// on.
func TestPCMv4RejectsLegacyAndJunkFrames(t *testing.T) {
	dec := newPCMDecoder()

	zstd := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00}
	if _, err := dec.decode(zstd); err == nil {
		t.Error("a zstd frame decoded without error")
	}

	for _, junk := range [][]byte{nil, {}, {0x50}, {0x50, 0x43, 0x4D}, []byte("PCM3xxxx")} {
		if _, err := dec.decode(junk); err == nil {
			t.Errorf("junk frame %v decoded without error", junk)
		}
	}

	// None of that may have poisoned the decoder: a real stream still decodes.
	if _, err := decodePrefix(t, dec, readV4Fixture(t, "pcmv4_stream.bin"), 10); err != nil {
		t.Fatalf("after rejected frames: %v", err)
	}
}
