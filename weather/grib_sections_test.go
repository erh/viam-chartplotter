package weather

import (
	"math"
	"testing"
)

// TestSignedInt16SignMagnitude verifies GRIB2's sign + magnitude
// decoding for the 16-bit case — the original bug was that
// `int16(uint16)` (Go's two's-complement cast) silently decoded a
// negative binary-scale factor (encoded per spec as 0x8001 = -1) as
// -32767, making 2^E ≈ 0 and collapsing every unpacked value to the
// reference, which surfaced downstream as "HRRR shows 57 kt
// uniformly across the whole chart".
func TestSignedInt16SignMagnitude(t *testing.T) {
	cases := []struct {
		raw  uint16
		want int
	}{
		{0x0000, 0},
		{0x0001, 1},
		{0x7FFF, 32767},
		{0x8000, 0},  // -0, treated as 0
		{0x8001, -1}, // the HRRR-bug case
		{0x8002, -2},
		{0xFFFF, -32767}, // sign + max-magnitude
	}
	for _, c := range cases {
		if got := signedInt16(c.raw); got != c.want {
			t.Errorf("signedInt16(0x%04x) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestSignedInt8SignMagnitude covers the 8-bit case, used for surface
// scale factors and the earth-radius scale in Lambert section 3.
func TestSignedInt8SignMagnitude(t *testing.T) {
	cases := []struct {
		raw  uint8
		want int
	}{
		{0x00, 0},
		{0x01, 1},
		{0x7F, 127},
		{0x80, 0},
		{0x81, -1},
		{0xFF, -127},
	}
	for _, c := range cases {
		if got := signedInt8(c.raw); got != c.want {
			t.Errorf("signedInt8(0x%02x) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestPackingNegativeScaleRoundTrip is the end-to-end check: stage a
// section 5 (template 5.0 simple packing) that uses binaryScale = -1
// encoded per spec, parse it, and verify unpacking produces sensible
// values. The "before-fix" code path would underflow the binary
// scale to 2^-32767, collapsing every output value to refValue.
func TestPackingNegativeScaleRoundTrip(t *testing.T) {
	// Encode a 4-byte section 5 body large enough for template 5.0:
	// section header (5) + N data points (4) + template# (2) + ref
	// (4) + binaryScale (2) + decimalScale (2) + bitsPerValue (1).
	// We don't need a real section length here — parsePackingSection
	// only reads up to the bitsPerValue byte for template 0.
	s5 := make([]byte, 21)
	// s5[0..4]: length + section number — parsePackingSection doesn't
	// inspect these, but pad them defensively.
	s5[4] = 5
	// s5[5..9]: number of data points — also unused by parse.
	// s5[9..10]: data representation template number = 0 (simple).
	s5[9] = 0
	s5[10] = 0
	// s5[11..15]: reference value = 1.0 (float32 big-endian = 0x3F800000).
	s5[11] = 0x3F
	s5[12] = 0x80
	s5[13] = 0x00
	s5[14] = 0x00
	// s5[15..17]: binary scale factor = -1 in sign + magnitude (0x8001).
	s5[15] = 0x80
	s5[16] = 0x01
	// s5[17..19]: decimal scale factor = 0.
	s5[17] = 0x00
	s5[18] = 0x00
	// s5[19]: bits per value = 0 (constant-field; every value equals ref).
	s5[19] = 0
	p, err := parsePackingSection(s5, 4)
	if err != nil {
		t.Fatalf("parsePackingSection: %v", err)
	}
	if p.binaryScale != -1 {
		t.Errorf("binaryScale = %d, want -1 (sign + magnitude decode)", p.binaryScale)
	}
	if p.decimalScale != 0 {
		t.Errorf("decimalScale = %d, want 0", p.decimalScale)
	}
	if math.Abs(float64(p.refValue)-1.0) > 1e-6 {
		t.Errorf("refValue = %v, want 1.0", p.refValue)
	}
	// With bitsPerValue=0 the unpacker fills every cell with refValue
	// — but if the binary-scale fix is broken upstream we'd see
	// values silently clobbered.
	vals, err := unpackData(nil, p)
	if err != nil {
		t.Fatalf("unpackData: %v", err)
	}
	for i, v := range vals {
		if math.Abs(v-1.0) > 1e-6 {
			t.Errorf("vals[%d] = %v, want 1.0", i, v)
		}
	}
}
