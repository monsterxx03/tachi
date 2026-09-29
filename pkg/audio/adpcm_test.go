package audio

import (
	"testing"
)

// The codes the device encoder produces for a known sequence of samples, and
// the samples those codes decode back to. Both were taken from
// ai-passport tools/stt-probe/adpcm_roundtrip.py, which is the implementation
// the device's C encoder was written against.
//
// This is the whole point of the test: three implementations of one format, and
// nothing in the protocol will tell us when they disagree — the audio just
// comes out as noise.
var (
	referenceSamples = []int16{
		0, 1000, -1000, 4000, -4000, 12000, -12000, 32767, -32768,
		0, 1, -1, 100, -100, 5000, 6000, 7000, 8000, 3000, -3000,
		20000, 21000, 22000, -20000, -21000, 16384, -16384, 12345, -23456, 777, -777, 42,
	}
	referenceCodes = []byte{
		0xF8, 0xF7, 0xF7, 0xF7, 0xA7, 0x80, 0x08, 0x9F,
		0x89, 0x45, 0xBF, 0x78, 0xD4, 0xB3, 0xB5, 0x80,
	}
	referenceDecoded = []int16{
		0, 11, -19, 44, -92, 201, -430, 927, -1983, 95, -283, 60, 372, 88, 3961,
		5621, 7130, 7587, 3014, -2465, 8585, 19639, 21074, 1496, -23687, 13555,
		-15114, 10955, -26287, 2382, -1342, 2043,
	}
)

func TestDecodeMatchesTheReference(t *testing.T) {
	got := DecodeADPCM(referenceCodes)

	if len(got) != len(referenceDecoded) {
		t.Fatalf("decoded %d samples, want %d (each byte is two codes)", len(got), len(referenceDecoded))
	}
	for i := range got {
		if got[i] != referenceDecoded[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], referenceDecoded[i])
		}
	}
}

// The low nibble comes first: the device packs "earlier sample in the low 4
// bits". Swapping them decodes the same bytes into a different signal — and
// one that still looks like speech-ish noise, so nothing downstream would
// notice.
func TestLowNibbleComesFirst(t *testing.T) {
	got := DecodeADPCM([]byte{0xF8})

	// Low nibble 8 = "no magnitude bits, positive" → the first step's eighth.
	if len(got) != 2 {
		t.Fatalf("one byte decoded to %d samples, want 2", len(got))
	}
	if got[0] != referenceDecoded[0] || got[1] != referenceDecoded[1] {
		t.Errorf("first byte decoded to %v, want %v", got[:2], referenceDecoded[:2])
	}
}

// Decoding is not a lossless round trip — but it must track the input's
// envelope. A decoder that got the tables or the direction bit wrong still
// returns the right NUMBER of samples; what it loses is the loud parts, and
// that is exactly what this pins down.
func TestDecodedSignalTracksTheInput(t *testing.T) {
	got := DecodeADPCM(referenceCodes)
	if len(got) != len(referenceSamples) {
		t.Fatalf("decoded %d samples, want %d", len(got), len(referenceSamples))
	}

	peak := func(samples []int16) int {
		max := 0
		for _, s := range samples {
			v := int(s)
			if v < 0 {
				v = -v
			}
			if v > max {
				max = v
			}
		}
		return max
	}

	// The fixture peaks at 32767 (it contains the extremes on purpose). ADPCM
	// shaves a little off a peak but must not collapse it: a decoder that lost
	// the magnitude bits would come back with a few hundred at most.
	if want, decoded := peak(referenceSamples), peak(got); decoded < want/2 {
		t.Errorf("decoded peak %d against an input peak of %d: the signal collapsed", decoded, want)
	}
}

// An empty upload decodes to nothing rather than to a panic — a truncated or
// empty stream is a real case (the device may send begin+end with no chunks).
func TestEmptyInput(t *testing.T) {
	if got := DecodeADPCM(nil); len(got) != 0 {
		t.Errorf("decoding nothing produced %d samples", len(got))
	}
}

// A decoder is reusable, and Reset really does return it to the start state:
// without that, a second upload would decode against the first one's predictor.
func TestReuseNeedsReset(t *testing.T) {
	var d ADPCMDecoder

	first := d.Decode(referenceCodes, nil)
	d.Reset()
	second := d.Decode(referenceCodes, nil)

	if len(first) != len(second) {
		t.Fatalf("second pass produced %d samples, want %d", len(second), len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("after Reset sample %d = %d, want %d", i, second[i], first[i])
		}
	}
}

// 8 KB/s of input is 16 kHz of samples: the caller sizes its buffers from that,
// and a byte that decodes to anything other than two samples would silently
// shift the whole timeline.
func TestSampleRateArithmetic(t *testing.T) {
	const seconds = 1
	bytes := 8000 * seconds // the link's rate at 16 kHz
	if got := len(DecodeADPCM(make([]byte, bytes))); got != 16000*seconds {
		t.Errorf("%d bytes decoded to %d samples, want %d", bytes, got, 16000*seconds)
	}
	// Silence encodes to zeroes and decodes back to silence.
	silence := DecodeADPCM(make([]byte, 64))
	for i, s := range silence {
		if s != 0 {
			t.Fatalf("sample %d of silence = %d", i, s)
		}
	}
}
