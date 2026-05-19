package vc

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestECMWFCapturedMessage decodes a real ECMWF Open Data wind message
// if one has been captured into testdata/ecmwf/*.grib2. The file is
// not checked in (ECMWF Open Data licensing aside, the GRIB blobs are
// large enough that checking them in would bloat the repo) — drop one
// in locally to validate the decoder against wire data.
//
// Capture one with:
//
//	go run ./cmd/ecmwf-probe -date YYYYMMDD -cycle 0 -step 0 -param 10u \
//	    > /tmp/probe.log    # tracing output
//	# or grab the raw message:
//	curl -r OFFSET-END -o testdata/ecmwf/10u-f000.grib2 \
//	    https://data.ecmwf.int/forecasts/YYYYMMDD/00z/ifs/0p25/oper/...grib2
//
// The test skips cleanly when the directory is empty so CI stays green
// in environments without network access (e.g. the sandbox where this
// branch was authored).
func TestECMWFCapturedMessage(t *testing.T) {
	dir := filepath.Join("testdata", "ecmwf")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("no testdata/ecmwf directory — drop a captured .grib2 message in to enable this test")
		}
		t.Fatal(err)
	}
	var found int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".grib2" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		grib, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", e.Name(), err)
			continue
		}
		var buf bytes.Buffer
		if err := DebugDumpGRIB(grib, &buf); err != nil {
			t.Errorf("%s: %v\n%s", e.Name(), err, buf.String())
			continue
		}
		t.Logf("%s OK:\n%s", e.Name(), buf.String())
		found++
	}
	if found == 0 {
		t.Skip("testdata/ecmwf is empty — drop a captured .grib2 message in to enable this test")
	}
}

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
// — every block uses ID = 2^idBits-1 (the libaec NC option), every
// sample is raw bps bits.
func TestAECDecodeNoCompression(t *testing.T) {
	const bps = 8
	const blockSize = 8
	const rsi = 2
	idBits := idSizeBits(bps)        // 3 under libaec bracketing
	idNC := (1 << uint(idBits)) - 1  // 7

	samples := []uint64{
		0, 1, 2, 3, 4, 5, 6, 7,
		255, 128, 64, 32, 16, 8, 4, 2,
	}
	w := &aecBitWriter{}
	// 2 blocks of 8 samples each. Both use no-compression.
	for b := 0; b < 2; b++ {
		w.writeBits(uint64(idNC), idBits)
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
	idBits := idSizeBits(bps) // 3 under libaec bracketing
	samples := []uint64{
		0, 1, 7, 8, 15, 16, 23, 31,
	}

	w := &aecBitWriter{}
	// libaec encodes k-split with id = k + 1, reserving id=0 for the
	// zero-block code. So a k=3 split goes on the wire as id=4.
	w.writeBits(uint64(k+1), idBits)
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

// TestAECDecodeZeroBlock exercises id=0, which libaec uses as the
// zero-block code (NOT as a generic FS-only block — the encoder
// prefers k=1 split for FS-like coding of non-zero data). Each
// zero-block ID is followed by a single FS-encoded m that maps to
// a run of consecutive all-zero blocks. We encode a 3-block run
// (m=2 → 3 zero blocks) and a 5-block run (m=5 → 5 zero blocks),
// then verify the decoder fills all 8 blocks × 4 samples = 32 slots
// with zeros while consuming only two FS reads' worth of bits.
func TestAECDecodeZeroBlock(t *testing.T) {
	const bps = 8
	const blockSize = 4
	const rsi = 8
	const n = rsi * blockSize // 32
	idBits := idSizeBits(bps)

	w := &aecBitWriter{}
	// First zero-block run: id=0, m=2 → 3 zero blocks (12 samples).
	w.writeBits(0, idBits)
	w.writeFS(2)
	// Second zero-block run: id=0, m=5 → 5 zero blocks (20 samples).
	w.writeBits(0, idBits)
	w.writeFS(5)

	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, n)
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got)=%d, want %d", len(got), n)
	}
	for i, v := range got {
		if v != 0 {
			t.Errorf("got[%d] = %d, want 0 (zero-block run)", i, v)
		}
	}
}

// TestAECDecodeZeroBlockROS exercises the m=4 "rest of segment"
// sentinel: when the FS-decoded m equals 4, the run extends to the
// end of the current RSI (clamped to 64 blocks by libaec's CCSDS
// 121.0-B-2 alignment). Encoded as id=0 + a single FS(4); the decoder
// must emit exactly `rsi` blocks' worth of zeros without trying to
// read past the bitstream for further block IDs.
func TestAECDecodeZeroBlockROS(t *testing.T) {
	const bps = 8
	const blockSize = 4
	const rsi = 6
	const n = rsi * blockSize // 24
	idBits := idSizeBits(bps)

	w := &aecBitWriter{}
	w.writeBits(0, idBits)
	w.writeFS(4) // ROS — rest of segment, all remaining `rsi` blocks zero

	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, n)
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got)=%d, want %d", len(got), n)
	}
	for i, v := range got {
		if v != 0 {
			t.Errorf("got[%d] = %d, want 0 (ROS zero-block)", i, v)
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
	idBits := idSizeBits(bps)        // 3 under libaec bracketing
	idSE := (1 << uint(idBits)) - 2  // 6
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
	idNC := (1 << uint(idBits)) - 1
	samples := []uint64{0, 1, 2, 4, 8, 16, 32, 64}

	w := &aecBitWriter{}
	w.writeBits(uint64(idNC), idBits)
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
	idNC := (1 << uint(idBits)) - 1
	samples := []uint64{1, 2, 3, 4, 5, 6, 7, 8}

	w := &aecBitWriter{}
	w.writeBits(uint64(idNC), idBits)
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

// TestIDSizeBits locks in the libaec-on-the-wire bracket boundaries
// (3 / 4 / 5 bits for bps in ≤8 / ≤16 / ≤32) that ECMWF Open Data
// is encoded against. The strict CCSDS ⌈log₂(bps+1)⌉ formula would
// disagree at boundary values like bps=8 or bps=16 — sticking with
// libaec matches what we actually see on the wire.
func TestIDSizeBits(t *testing.T) {
	cases := []struct {
		bps  int
		want int
	}{
		{1, 3}, {2, 3}, {4, 3}, {7, 3}, {8, 3},
		{9, 4}, {12, 4}, {15, 4}, {16, 4},
		{17, 5}, {31, 5}, {32, 5},
	}
	for _, c := range cases {
		if got := idSizeBits(c.bps); got != c.want {
			t.Errorf("idSizeBits(%d) = %d, want %d", c.bps, got, c.want)
		}
	}
}

// TestAECDecodeBps12NC reproduces an ECMWF Open Data wire case: with
// bps=12 (idBits=4 under the libaec bracketing), the No-Compression
// option ID is the all-ones 4-bit value 15. An earlier revision of
// this decoder followed the strict CCSDS spec instead and put NC at
// ID=bps=12, which made it reject ECMWF's real id=15 NC blocks as
// "invalid block id=15" once it stopped first failing at id=12.
func TestAECDecodeBps12NC(t *testing.T) {
	const bps = 12
	const blockSize = 4
	const rsi = 1
	idBits := idSizeBits(bps)
	idNC := (1 << uint(idBits)) - 1
	if idBits != 4 || idNC != 15 {
		t.Fatalf("idBits=%d idNC=%d, want 4 / 15", idBits, idNC)
	}
	samples := []uint64{0, 1, 4095, 2048}
	w := &aecBitWriter{}
	w.writeBits(uint64(idNC), idBits)
	for _, s := range samples {
		w.writeBits(s, bps)
	}
	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	for i, want := range samples {
		if got[i] != want {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want)
		}
	}
}

// TestAECDecodePartialLastRSI locks in the fix for a production
// failure where the decoder ran off the end of the section-7 buffer
// thousands of samples into the field. libaec encoders write only
// ceil(n / block_size) blocks total, grouped into reference-sample
// intervals of `rsi` blocks each. The last RSI typically has fewer
// blocks than `rsi`; the previous decoder revision unconditionally
// read `rsi` blocks per RSI, which made it try to consume bits the
// encoder never wrote once `n` wasn't a multiple of `rsi*block_size`.
func TestAECDecodePartialLastRSI(t *testing.T) {
	const bps = 8
	const blockSize = 4
	const rsi = 3 // RSI window = 12 samples
	// n = 14: first RSI has 3 blocks (12 samples), last "RSI" has
	// only 1 block (covering samples 12-13, with sample 13 of the
	// block being padded by the encoder).
	const n = 14
	idBits := idSizeBits(bps)
	idNC := (1 << uint(idBits)) - 1

	w := &aecBitWriter{}
	// 3 NC blocks for the first RSI.
	for blk := 0; blk < 3; blk++ {
		w.writeBits(uint64(idNC), idBits)
		for j := 0; j < blockSize; j++ {
			w.writeBits(uint64(blk*blockSize+j), bps)
		}
	}
	// 1 NC block for the short last RSI (samples 12, 13, plus two
	// padding values the encoder fabricates; the decoder must read
	// all 4 then discard the trailing 2).
	w.writeBits(uint64(idNC), idBits)
	w.writeBits(12, bps)
	w.writeBits(13, bps)
	w.writeBits(0xAA, bps) // padding
	w.writeBits(0xBB, bps) // padding

	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, n)
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got)=%d, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		if got[i] != uint64(i) {
			t.Errorf("got[%d] = %d, want %d", i, got[i], i)
		}
	}
}

// TestAECDecodeSEFirstBlockWithRef reproduces the production case
// "SE block needs even sample count, got 31": block_size = 32 with
// the preprocessor on, so block 0 of an RSI has a raw reference
// sample at position 0 and 31 (odd) remaining samples coded by SE.
// libaec resolves the odd count by reading 16 m values where the
// first iteration emits only the s2 half (the s1 half would have
// paired with the absent slot before sample 1, so it's discarded),
// and the remaining 15 iterations emit full (s1, s2) pairs.
//
// We also exercise the preprocessor inverse here by setting the ref
// to a known mid-range value and choosing m=0 for every iteration —
// that decodes (s1=0, s2=0) which the preprocessor inverse then
// maps to "no change from previous", i.e., every sample equals ref.
func TestAECDecodeSEFirstBlockWithRef(t *testing.T) {
	const bps = 12
	const blockSize = 32
	const rsi = 1
	idBits := idSizeBits(bps)
	idSE := (1 << uint(idBits)) - 2
	const ref uint64 = 0x800
	const flags = ccsdsFlagPreprocessor

	w := &aecBitWriter{}
	// Reference sample first (raw bps bits).
	w.writeBits(ref, bps)
	// Then the SE block ID.
	w.writeBits(uint64(idSE), idBits)
	// 16 m values, all m=0 → each decodes to (s1=0, s2=0).
	for i := 0; i < 16; i++ {
		w.writeFS(0)
	}

	got, err := aecDecode(w.bytes(), bps, flags, blockSize, rsi, blockSize)
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	if len(got) != blockSize {
		t.Fatalf("len(got)=%d, want %d", len(got), blockSize)
	}
	if got[0] != ref {
		t.Errorf("got[0] = %d, want ref %d", got[0], ref)
	}
	// Preprocessor inverse: theta-mapped 0 means "delta = 0" relative
	// to the previous sample, so every sample equals the reference.
	for i := 1; i < blockSize; i++ {
		if got[i] != ref {
			t.Errorf("got[%d] = %d, want %d (preprocessor inverse identity)", i, got[i], ref)
		}
	}
}

// TestAECDecodeBps12KSplitNearBps covers the boundary case in ECMWF
// wire data: id=12 with bps=12, which under libaec's id=k+1 layout
// means k=11 — eleven low bits per sample plus a single FS stop bit
// (since high = sample >> 11 ∈ {0, 1} and most samples have high=0).
// Before locking in `k = id - 1`, the decoder read 12 low bits per
// sample instead of 11, over-consuming block_size bits per k-split
// block and exhausting the bitstream partway through every wind
// field.
func TestAECDecodeBps12KSplitNearBps(t *testing.T) {
	const bps = 12
	const blockSize = 4
	const rsi = 1
	const k = bps - 1 // 11 — the boundary case
	const id = k + 1  // 12 on the wire
	idBits := idSizeBits(bps)
	if id > (1<<uint(idBits))-2 {
		t.Fatalf("id=%d not in libaec k-split range for bps=%d", id, bps)
	}
	samples := []uint64{0, 1, 2047, 2048, 4095, 100, 200, 0}[:blockSize]
	w := &aecBitWriter{}
	w.writeBits(uint64(id), idBits)
	// High parts: sample >> k, FS-encoded.
	for _, s := range samples {
		w.writeFS(int(s >> uint(k)))
	}
	// Low parts: low k bits, raw.
	for _, s := range samples {
		w.writeBits(s&((1<<uint(k))-1), k)
	}
	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	for i, want := range samples {
		if got[i] != want {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want)
		}
	}
}

// TestAECDecodeKSplitK0 exercises the libaec encoding for "FS-only"
// blocks: id=1 (i.e., k = id - 1 = 0), no low-bit phase, each FS
// value IS the sample. This is the path libaec uses for blocks where
// pure FS coding would be optimal — id=0 is reserved for the zero-
// block code, so FS-only lands at id=1.
func TestAECDecodeKSplitK0(t *testing.T) {
	const bps = 8
	const blockSize = 8
	const rsi = 1
	const id = 1 // k = id - 1 = 0 → pure FS
	idBits := idSizeBits(bps)
	samples := []uint64{0, 1, 2, 3, 0, 1, 2, 0}

	w := &aecBitWriter{}
	w.writeBits(uint64(id), idBits)
	for _, s := range samples {
		w.writeFS(int(s))
	}
	got, err := aecDecode(w.bytes(), bps, 0, blockSize, rsi, len(samples))
	if err != nil {
		t.Fatalf("aecDecode: %v", err)
	}
	for i, want := range samples {
		if got[i] != want {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want)
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
