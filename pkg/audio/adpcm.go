// Package audio holds audio codec pieces that are not specific to any product.
//
// Today it has one thing: IMA ADPCM (4 bit per sample, 4:1) decoding, which is
// the host end of the badge voice link. The device encodes 16 kHz mono PCM down
// to 8 KB/s (ai-passport main/badge_adpcm.c) and sends it as base64 chunks.
//
// Three implementations of this format exist and all of them must agree
// byte-for-byte — the device encoder, this decoder, and the Python probe
// (ai-passport tools/stt-probe/adpcm_roundtrip.py) that was used to measure
// whether the compression costs recognition accuracy. A mismatch does not fail
// loudly: it produces noise that sounds like a bad microphone.
package audio

// The step and index-adjust tables: all of what IMA ADPCM "knows". Copied from
// the device encoder; do not touch one without the other.
var adpcmStep = [89]int32{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31,
	34, 37, 41, 45, 50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143,
	157, 173, 190, 209, 230, 253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658,
	724, 796, 876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024,
	3327, 3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487,
	12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

var adpcmIndexAdjust = [16]int32{
	-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8,
}

// ADPCMDecoder decodes the 4-bit codes of one voice upload back into samples.
//
// Stateful on purpose: ADPCM is differential — every sample is relative to the
// predictor the previous one produced. Both ends must therefore start from the
// same state and see the codes in the same order; the zero value IS that start
// state (index 0, predictor 0), which is why Reset only matters for reuse.
type ADPCMDecoder struct {
	index     int32
	predictor int32
}

// Reset returns the decoder to the state both ends start from.
func (d *ADPCMDecoder) Reset() {
	d.index = 0
	d.predictor = 0
}

// Decode appends the samples encoded by src to dst and returns the extended
// slice. Each byte carries two codes, low nibble first — the order the device
// packs them in (ai-passport main/badge_adpcm.c).
func (d *ADPCMDecoder) Decode(src []byte, dst []int16) []int16 {
	for _, b := range src {
		dst = d.decodeCode(int32(b&0x0F), dst)
		dst = d.decodeCode(int32(b>>4), dst)
	}
	return dst
}

// decodeCode decodes one 4-bit code (0..15) and appends it.
func (d *ADPCMDecoder) decodeCode(code int32, dst []int16) []int16 {
	step := adpcmStep[d.index]
	// The magnitude bits are 0..7; bit 3 is the direction.
	diff := step >> 3
	if code&4 != 0 {
		diff += step
	}
	if code&2 != 0 {
		diff += step >> 1
	}
	if code&1 != 0 {
		diff += step >> 2
	}
	if code&8 != 0 {
		d.predictor += diff
	} else {
		d.predictor -= diff
	}
	if d.predictor > 32767 {
		d.predictor = 32767
	}
	if d.predictor < -32768 {
		d.predictor = -32768
	}
	// The step index moves by the same amount the encoder moved it, which is why
	// it only looks at the magnitude bits.
	d.index += adpcmIndexAdjust[code&7]
	if d.index < 0 {
		d.index = 0
	}
	if d.index > 88 {
		d.index = 88
	}
	return append(dst, int16(d.predictor))
}

// DecodeADPCM decodes a complete upload from the initial state. It is the
// convenience the call sites want: one upload is one self-contained stream, so
// nothing needs to survive between calls.
func DecodeADPCM(src []byte) []int16 {
	var d ADPCMDecoder

	return d.Decode(src, make([]int16, 0, len(src)*2))
}
