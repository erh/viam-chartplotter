package vc

import (
	"math"
	"testing"
)

// aecBitWriter is the encoder-side counterpart to aecBitReader, used
// only by the test suite to manufacture AEC bitstreams that exercise
// each of the decoder's code paths without needing a captured ECMWF
// .grib2 file in testdata/.
type aecBitWriter struct {
	data    []byte
	cur     byte
	bitsCur uint // 0..7, count of bits already written into cur
}

func (w *aecBitWriter) writeBits(v uint64, n int) {
	for n > 0 {
		room := 8 - int(w.bitsCur)
		take := n
		if take > room {
			take = room
		}
		shift := n - take
		chunk := byte((v >> uint(shift)) & ((1 << uint(take)) - 1))
		w.cur |= chunk << uint(room-take)
		w.bitsCur += uint(take)
		if w.bitsCur == 8 {
			w.data = append(w.data, w.cur)
			w.cur = 0
			w.bitsCur = 0
		}
		n -= take
	}
}

// writeFS writes a Fundamental-Sequence value: n zero bits followed by
// a 1 bit. The decoder counts zeros until the first 1.
func (w *aecBitWriter) writeFS(v int) {
	for i := 0; i < v; i++ {
		w.writeBits(0, 1)
	}
	w.writeBits(1, 1)
}

func (w *aecBitWriter) bytes() []byte {
	if w.bitsCur == 0 {
		return w.data
	}
	return append(w.data, w.cur)
}

func TestAECBitReaderRoundTrip(t *testing.T) {
	w := &aecBitWriter{}
	w.writeBits(0xA, 4)    // 1010
	w.writeBits(0x5, 4)    // 0101
	w.writeBits(0xFF, 8)   // 11111111
	w.writeBits(0x3, 3)    // 011
	w.writeBits(0x0, 1)    // 0
	w.writeBits(0xDEAD, 16)
	r := newAECBitReader(w.bytes())
	cases := []struct {
		bits int
		want uint64
	}{
		{4, 0xA},
		{4, 0x5},
		{8, 0xFF},
		{3, 0x3},
		{1, 0x0},
		{16, 0xDEAD},
	}
	for i, c := range cases {
		got, err := r.readBits(c.bits)
		if err != nil {
			t.Fatalf("case %d readBits(%d): %v", i, c.bits, err)
		}
		if got != c.want {
			t.Errorf("case %d readBits(%d) = 0x%X, want 0x%X", i, c.bits, got, c.want)
		}
	}
}

func TestAECFSRoundTrip(t *testing.T) {
	w := &aecBitWriter{}
	for _, v := range []int{0, 1, 2, 5, 0, 10} {
		w.writeFS(v)
	}
	r := newAECBitReader(w.bytes())
	for i, want := range []int{0, 1, 2, 5, 0, 10} {
		got, err := r.readFS()
		if err != nil {
			t.Fatalf("readFS %d: %v", i, err)
		}
		if got != want {
			t.Errorf("readFS %d = %d, want %d", i, got, want)
		}
	}
}

// TestAECDecodeNoCompression exercises the no-compression block path
// — every block ID is the max value, every sample is raw bps bits.
// We pick parameters that ECMWF doesn't actually use (bps=8, block=8,
// rsi=2, preprocessor off) to isolate the code path.
func TestAECDecodeNoCompression(t *testing.T) {
	const bps = 8
	const blockSize = 8
	const rsi = 2
	idBits := idSizeBits(bps) // 3
	idMax := (1 << uint(idBits)) - 1

	samples := []uint64{
		0, 1, 2, 3, 4, 5, 6, 7,
		255, 128, 64, 32, 16, 8, 4, 2,
	}
	w := &aecBitWriter{}
	// 2 blocks of 8 samples each. Both use no-compression.
	for b := 0; b < 2; b++ {
		w.writeBits(uint64(idMax), idBits)
		for i := 0; i < blockSize; i++ {
			w.writeBits(samples[b*blockSize+i], bps)
		}
	}
	got, err := aecDecode(w.bytes(), bps, 0 /* flags: preprocessor off */, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	if len(got) != len(samples) {
		t.Fatalf("len(got)=%d, want %d", len(got), len(samples))
	}
	for i, v := range got {
		if v != samples[i] {
			t.Errorf("got[%d] = %d, want %d", i, v, samples[i])
		}
	}
}

// TestAECDecodeKSplit exercises sample-split encoding for a small
// block. Each sample is broken into a high part (FS-coded) and a low
// part (k raw bits); the decoder must recombine them as (high<<k)|low.
func TestAECDecodeKSplit(t *testing.T) {
	const bps = 8
	const blockSize = 8
	const rsi = 1
	const k = 3
	idBits := idSizeBits(bps) // 3
	samples := []uint64{
		0, 1, 7, 8, 15, 16, 23, 31,
	}

	w := &aecBitWriter{}
	w.writeBits(uint64(k), idBits)
	// High parts first, all FS-coded.
	for _, s := range samples {
		high := s >> uint(k)
		w.writeFS(int(high))
	}
	// Then low parts, raw k bits each.
	for _, s := range samples {
		low := s & ((1 << uint(k)) - 1)
		w.writeBits(low, k)
	}

	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	for i, v := range got {
		if v != samples[i] {
			t.Errorf("got[%d] = %d, want %d", i, v, samples[i])
		}
	}
}

// TestAECDecodeFSOnly exercises id=0 with no preprocessor — every
// sample is FS-coded. Tested with small magnitudes so the bitstream
// stays compact.
func TestAECDecodeFSOnly(t *testing.T) {
	const bps = 4
	const blockSize = 8
	const rsi = 1
	idBits := idSizeBits(bps) // 2
	samples := []uint64{0, 1, 2, 3, 0, 1, 2, 0}

	w := &aecBitWriter{}
	w.writeBits(0, idBits) // id = 0 → FS only
	for _, s := range samples {
		w.writeFS(int(s))
	}
	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	for i, v := range got {
		if v != samples[i] {
			t.Errorf("got[%d] = %d, want %d", i, v, samples[i])
		}
	}
}

// TestAECDecodeSecondExtension exercises the SE pairing math. We
// encode pairs (s1, s2) using m = (s1+s2)*(s1+s2+1)/2 + s2 and confirm
// the decoder inverts back to the originals.
func TestAECDecodeSecondExtension(t *testing.T) {
	const bps = 8
	const blockSize = 4
	const rsi = 1
	idBits := idSizeBits(bps) // 3
	idMax := (1 << uint(idBits)) - 1
	idSE := idMax - 1
	samples := []uint64{0, 1, 2, 0, 3, 4, 1, 0}

	w := &aecBitWriter{}
	for b := 0; b < 2; b++ {
		w.writeBits(uint64(idSE), idBits)
		for j := 0; j < blockSize; j += 2 {
			s1 := samples[b*blockSize+j]
			s2 := samples[b*blockSize+j+1]
			sum := s1 + s2
			m := sum*(sum+1)/2 + s2
			w.writeFS(int(m))
		}
	}
	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi*2, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	for i, v := range got {
		if v != samples[i] {
			t.Errorf("got[%d] = %d, want %d", i, v, samples[i])
		}
	}
}

// TestUnpackCCSDSAppliesScaling wraps a no-compression-encoded payload
// through unpackCCSDS to verify the R + X·2^E · 10^-D scaling is
// applied consistently with the simple-packing path.
func TestUnpackCCSDSAppliesScaling(t *testing.T) {
	const bps = 8
	const blockSize = 8
	const rsi = 1
	idBits := idSizeBits(bps)
	idMax := (1 << uint(idBits)) - 1
	samples := []uint64{0, 1, 2, 4, 8, 16, 32, 64}

	w := &aecBitWriter{}
	w.writeBits(uint64(idMax), idBits)
	for _, s := range samples {
		w.writeBits(s, bps)
	}

	// ref = 10.0, E = 1 → 2^1, D = 0
	got, err := unpackCCSDS(w.bytes(), 10.0, 1, 0, bps, 0, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("unpackCCSDS: %v", err)
	}
	for i, x := range samples {
		want := (10.0 + float64(x)*2.0) * 1.0
		if math.Abs(got[i]-want) > 1e-9 {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want)
		}
	}
}

// TestCCSDSThroughPackingSection drives a synthetic section-5 body for
// template 5.42 through parsePackingSection → unpackData. Catches
// regressions where the section-parser's field offsets drift away from
// what the decoder expects.
func TestCCSDSThroughPackingSection(t *testing.T) {
	const bps = 8
	const blockSize = 8
	const rsi = 1
	idBits := idSizeBits(bps)
	idMax := (1 << uint(idBits)) - 1
	samples := []uint64{1, 2, 3, 4, 5, 6, 7, 8}

	w := &aecBitWriter{}
	w.writeBits(uint64(idMax), idBits)
	for _, s := range samples {
		w.writeBits(s, bps)
	}
	packed := w.bytes()

	// Build a minimal section-5 body. parsePackingSection reads up to
	// octet 25 (s5[24]) for template 5.42, so allocate that much.
	s5 := make([]byte, 25)
	s5[4] = 5
	// Template 42 → bytes 9-10
	s5[9] = 0
	s5[10] = 42
	// Reference value = 0.0 → all bytes 0 (positive zero).
	// Binary scale factor 0, decimal scale factor 0, bps=8.
	s5[19] = bps
	// CCSDS flags = 0 (no preprocessor, LSB-first interpretation
	// irrelevant when bps fits in one byte read).
	s5[21] = 0
	s5[22] = blockSize
	// RSI big-endian 16 bits.
	s5[23] = 0
	s5[24] = rsi

	p, err := parsePackingSection(s5, len(samples))
	if err != nil {
		t.Fatalf("parsePackingSection: %v", err)
	}
	if p.template != 42 {
		t.Fatalf("template = %d, want 42", p.template)
	}
	if p.bitsPerValue != bps {
		t.Fatalf("bitsPerValue = %d, want %d", p.bitsPerValue, bps)
	}
	if p.ccsdsBlockSize != blockSize {
		t.Fatalf("ccsdsBlockSize = %d, want %d", p.ccsdsBlockSize, blockSize)
	}
	if p.ccsdsRSI != rsi {
		t.Fatalf("ccsdsRSI = %d, want %d", p.ccsdsRSI, rsi)
	}
	got, err := unpackData(packed, p)
	if err != nil {
		t.Fatalf("unpackData: %v", err)
	}
	if len(got) != len(samples) {
		t.Fatalf("len(got)=%d, want %d", len(got), len(samples))
	}
	for i, want := range samples {
		if got[i] != float64(want) {
			t.Errorf("got[%d] = %v, want %v", i, got[i], float64(want))
		}
	}
}

// TestUnpackCCSDSConstantField verifies bps=0 produces a constant field
// equal to the reference value, matching simple-packing's behaviour.
func TestUnpackCCSDSConstantField(t *testing.T) {
	got, err := unpackCCSDS(nil, 3.5, 0, 0, 0, 0, 8, 1, 7)
	if err != nil {
		t.Fatalf("unpackCCSDS: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("len(got)=%d, want 7", len(got))
	}
	for i, v := range got {
		if v != 3.5 {
			t.Errorf("got[%d] = %v, want 3.5", i, v)
		}
	}
}
