package main

import (
	"encoding/binary"
	"fmt"

	"github.com/madpsy/ubersdr_doppler/internal/pcmv4"
)

// ---------------------------------------------------------------------------
// PCM binary packet decoder — protocol version 4
// ---------------------------------------------------------------------------
// The UberSDR server's audio WebSocket speaks protocol version 4, which this
// program asks for explicitly (format=pcm-zstd&version=4).
//
// Versions 1-3 sent a fixed 29/37-byte header and wrapped the samples in zstd.
// zstd made the data LARGER -- it matches repeated byte strings, and a
// band-limited RF signal has none, only sample-to-sample correlation a byte
// matcher cannot see. Version 4 replaces the wrapper with a backward-adaptive
// predictive lossless codec and sends a variable-length header carrying only
// what changed since the last packet.
//
// The wire format and the decoder live in internal/pcmv4, vendored verbatim
// from the reference port so that a fix made there can be dropped in here
// without a rewrite. Two consequences for callers:
//
//   - A decoder holds the adaptation state of its predictor, so it belongs to
//     exactly ONE WebSocket and one goroutine. Reconnecting means a new one.
//   - Every packet the socket delivers must reach it, in order, even when the
//     samples are then thrown away: the predictor derives its filter taps from
//     the samples already coded, so a skipped packet desynchronises everything
//     after it into plausible noise rather than into an error.

// pcmPacket is the result of decoding one binary WebSocket message.
type pcmPacket struct {
	pcm          []byte // little-endian int16 PCM samples (interleaved for IQ)
	sampleRate   int
	channels     int
	hasSigInfo   bool    // true when radiod reported signal quality
	basebandDBFS float32 // baseband power dBFS
	noiseDBFS    float32 // noise density dBFS
}

// pcmDecoder decodes the version 4 packets of one connection.
type pcmDecoder struct {
	v4 *pcmv4.PCMv4StreamDecoder
}

// newPCMDecoder returns a decoder for one WebSocket. Create it after the dial
// succeeds and discard it when the socket closes; see reset.
func newPCMDecoder() *pcmDecoder {
	return &pcmDecoder{v4: pcmv4.NewPCMv4StreamDecoder()}
}

// reset discards the predictor and header state, for reuse across a reconnect.
// A stream decoder carried over a reconnect would apply the old connection's
// adaptation to the new one's first packets.
func (d *pcmDecoder) reset() { d.v4 = pcmv4.NewPCMv4StreamDecoder() }

// decode parses one binary PCM packet into little-endian int16 samples.
func (d *pcmDecoder) decode(data []byte) (pcmPacket, error) {
	if pcmv4.IsZstdFrame(data) {
		// A server older than 0.1.63 clamps the requested version to 1-3 and
		// serves version 1 instead of refusing, so this is what "too old" looks
		// like on the wire. Saying so beats logging a bad magic per packet.
		return pcmPacket{}, fmt.Errorf("server sent a zstd-wrapped (pre-v4) packet: it is too old for protocol version 4")
	}
	if !pcmv4.PCMv4IsHeader(data) {
		return pcmPacket{}, fmt.Errorf("not a protocol version 4 packet (%d bytes)", len(data))
	}

	pcmLE, rate, channels, baseband, noise, err := d.v4.DecodePacketLE(data)
	if err != nil {
		return pcmPacket{}, err
	}
	return pcmPacket{
		pcm:          pcmLE,
		sampleRate:   rate,
		channels:     channels,
		hasSigInfo:   baseband > -998,
		basebandDBFS: baseband,
		noiseDBFS:    noise,
	}, nil
}

// downmixStereoToMono converts 2-channel S16LE PCM to mono S16LE.
// Used for wfm mode which delivers stereo 48 kHz audio.
func downmixStereoToMono(stereo []byte) []byte {
	n := len(stereo) / 4 // 2 bytes per sample × 2 channels
	mono := make([]byte, n*2)
	for i := 0; i < n; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(stereo[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(stereo[i*4+2:])))
		m := int16((l + r) / 2)
		binary.LittleEndian.PutUint16(mono[i*2:], uint16(m))
	}
	return mono
}
