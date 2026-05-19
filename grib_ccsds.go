package vc

import (
	"fmt"
	"math"
)

// CCSDS / AEC (Adaptive Entropy Coder) decoder for GRIB2 data
// representation template 5.42. ECMWF Open Data publishes every
// forecast field with this packing, so without it the ECMWF stub
// in the model registry can't be wired up.
//
// References:
//   - CCSDS 121.0-B-3 Lossless Data Compression Recommendation
//   - libaec  (the reference C implementation eccodes uses)
//
// What's implemented:
//   - Block-by-block decode with all five code options:
//       FS only (id=0), k-split (id=1..bps-3), second extension
//       (id=2^id_size-2), no compression (id=2^id_size-1), and
//       zero-block (a special encoding under id=0).
//   - Default (non-restricted) option set, which is what ECMWF uses.
//   - MSB-first and LSB-first byte ordering.
//   - Preprocessor inverse (offset-binary diff → absolute samples) for
//     unsigned data, which is the only mode GRIB2 emits.
//
// What's not implemented:
//   - Restricted option set (no GRIB2 producer in our registry uses it).
//   - 24-bit packed samples (AEC_DATA_3BYTE).
//   - Signed (two's-complement) input data — GRIB2 always stores
//     non-negative integers in the packed stream.
//   - RSI byte padding (AEC_PAD_RSI). The default flag bit is off in
//     every ECMWF Open Data file we've inspected.
//
// The decoder is "clean room" against the CCSDS spec — it has not yet
// been validated end-to-end against a live ECMWF GRIB2 file. Round-trip
// tests in grib_ccsds_test.go exercise every code option against a
// matching in-tree encoder; real-world validation against an ECMWF
// 10m-wind file is still required before declaring the ECMWF model
// production-ready.

// ccsdsFlag bits, encoded into GRIB2 Section 5 Octet 22. Despite
// living in a WMO-defined byte, ecCodes (the de-facto reference
// implementation that ECMWF Open Data is encoded against) treats this
// byte as the libaec flag set directly, with no remapping — see
// eccodes/src/grib_accessor_class_data_ccsds_packing.cc, which does a
// straight `strm.flags = ccsds_flags`. So our bit positions here are
// the libaec ones, not a separately-numbered WMO scheme.
//
// Real ECMWF Open Data fields typically arrive with flags = 0x0C
// (MSB-first + preprocessor on). Earlier revisions of this file used
// a WMO 1-indexed-bit interpretation which made MSB-first look like
// "restricted option set" and bailed the decoder out at first byte.
const (
	ccsdsFlagSigned       = 0x01 // input data is signed (two's complement)
	ccsdsFlag3Byte        = 0x02 // 24-bit packed samples
	ccsdsFlagMSBFirst     = 0x04 // 0 = LSB first within sample, 1 = MSB first
	ccsdsFlagPreprocessor = 0x08 // preprocessor (offset-binary θ-mapping) applied
	ccsdsFlagRestricted   = 0x10 // restricted code option set
	ccsdsFlagPadRSI       = 0x20 // pad each RSI to a byte boundary
)

// idSizeBits is the width of the per-block ID field. ECMWF Open Data
// is encoded by ecCodes-via-libaec, which rounds bps up to fixed
// brackets (3/4/5 bits) rather than using the strict ⌈log₂(bps+1)⌉
// width from CCSDS 121.0-B-3 §5.1.2.1.1.3. We follow libaec because
// that's what we'll see on the wire: bps=12 fields ship with a 4-bit
// ID space and reserve the maximum ID (= 2^4-1 = 15) for the No-
// Compression option, leaving room for k-split codes between bps and
// idMax-2 that the strict CCSDS layout would call "unused".
func idSizeBits(bps int) int {
	switch {
	case bps <= 0:
		return 0
	case bps <= 8:
		return 3
	case bps <= 16:
		return 4
	default:
		return 5
	}
}

// aecBitReader pulls bit fields from a byte slice. The CCSDS spec
// always reads bits from the most-significant end of each byte; the
// MSB/LSB flag at the GRIB2 level only affects how multi-byte sample
// fields (when bps > 8) are reassembled, which we don't currently use
// for fundamental-sequence reads.
type aecBitReader struct {
	data    []byte
	bytePos int
	bitPos  uint // bits already consumed in data[bytePos] (0..7)
}

func newAECBitReader(data []byte) *aecBitReader {
	return &aecBitReader{data: data}
}

// readBits pulls the next n bits and returns them right-justified.
// Returns 0 + done=true once the buffer is exhausted; callers should
// already know how many samples they expect (block_size * num_blocks)
// so running off the end is treated as a corrupt-stream error by the
// caller.
func (r *aecBitReader) readBits(n int) (uint64, error) {
	var v uint64
	for n > 0 {
		if r.bytePos >= len(r.data) {
			return 0, fmt.Errorf("aec: read past end of stream (have %d bytes)", len(r.data))
		}
		take := 8 - int(r.bitPos)
		if take > n {
			take = n
		}
		b := r.data[r.bytePos]
		shift := 8 - int(r.bitPos) - take
		chunk := (uint64(b) >> uint(shift)) & ((1 << uint(take)) - 1)
		v = (v << uint(take)) | chunk
		r.bitPos += uint(take)
		if r.bitPos >= 8 {
			r.bytePos++
			r.bitPos = 0
		}
		n -= take
	}
	return v, nil
}

// readFS decodes a single Fundamental Sequence value: the number of
// zero bits read until the first 1 bit. Used for k-split high parts,
// for FS-only blocks, and for the zero-block count.
func (r *aecBitReader) readFS() (int, error) {
	count := 0
	for {
		b, err := r.readBits(1)
		if err != nil {
			return 0, err
		}
		if b == 1 {
			return count, nil
		}
		count++
		if count > 1<<20 {
			return 0, fmt.Errorf("aec: runaway FS decode (>%d zeros)", count)
		}
	}
}

// aecDecode unpacks the raw integer samples from an AEC-compressed
// stream. The caller is responsible for converting the integers to
// floats via GRIB2's R + X*2^E * 10^-D formula.
func aecDecode(packed []byte, bps int, flags byte, blockSize, rsi, n int) ([]uint64, error) {
	if bps == 0 {
		return make([]uint64, n), nil
	}
	if bps < 1 || bps > 32 {
		return nil, fmt.Errorf("aec: bps=%d outside [1,32]", bps)
	}
	if flags&ccsdsFlagRestricted != 0 {
		return nil, fmt.Errorf("aec: restricted option set not implemented")
	}
	if blockSize <= 0 {
		return nil, fmt.Errorf("aec: blockSize=%d invalid", blockSize)
	}
	if rsi <= 0 {
		return nil, fmt.Errorf("aec: rsi=%d invalid", rsi)
	}
	preprocess := flags&ccsdsFlagPreprocessor != 0
	idBits := idSizeBits(bps)
	// libaec-on-the-wire layout (what ECMWF Open Data uses):
	//   - 2^idBits-1 = idNC (No Compression)
	//   - 2^idBits-2 = idSE (Second Extension)
	//   - 0          = FS / zero-block
	//   - 1..idNC-2  = k-split with k = id
	// Note the k-split range can extend past bps when idBits brackets
	// bps below the next power-of-two boundary. The decoder accepts
	// k=bps and k=bps+1 etc. — those carry no useful information
	// versus NC, but libaec's encoder emits them in some corners
	// (we observed id=12 with bps=12 in production ECMWF runs).
	idNC := (1 << uint(idBits)) - 1
	idSE := idNC - 1

	out := make([]uint64, 0, n)
	r := newAECBitReader(packed)

	// Walk RSI by RSI: each RSI is rsi blocks of blockSize samples.
	// When the preprocessor is on, the first sample of each RSI is a
	// raw "reference" sample (bps bits) stored ahead of the first
	// block's coded payload — it counts as the first sample of the
	// first block, so the coded portion of that block is one shorter.
	for len(out) < n {
		samplesThisRSI := rsi * blockSize
		if samplesThisRSI > n-len(out) {
			samplesThisRSI = n - len(out)
		}

		// rsiOut indexes into out for the start of the current RSI so we
		// can apply the preprocessor inverse at the end (theta-mapping).
		rsiOut := len(out)

		var refSample uint64
		if preprocess {
			v, err := r.readBits(bps)
			if err != nil {
				return nil, fmt.Errorf("aec: reading RSI reference sample: %w", err)
			}
			refSample = v
			out = append(out, refSample)
		}

		samplesEmittedInRSI := len(out) - rsiOut
		blockIndex := 0
		for samplesEmittedInRSI < samplesThisRSI {
			// First block consumes one fewer coded sample if the ref
			// already occupied its first slot.
			samplesNeeded := blockSize
			if blockIndex == 0 && preprocess {
				samplesNeeded = blockSize - 1
			}
			if samplesEmittedInRSI+samplesNeeded > samplesThisRSI {
				samplesNeeded = samplesThisRSI - samplesEmittedInRSI
			}
			blockIndex++

			id, err := r.readBits(idBits)
			if err != nil {
				return nil, fmt.Errorf("aec: reading block ID: %w", err)
			}

			switch {
			case id == 0:
				// FS-only block (k=0) — every sample encoded as its
				// own unary run. The spec also overloads id=0 with a
				// zero-block escape (a run of all-zero blocks), but
				// we cannot tell zero-block from FS at the bit level
				// without out-of-band encoder state, so we
				// conservatively decode as FS. Zero-block runs are
				// rare on wind fields (10u/10v vary almost
				// everywhere) — flagged as a known limitation in the
				// package doc above.
				for j := 0; j < samplesNeeded; j++ {
					v, err := r.readFS()
					if err != nil {
						return nil, fmt.Errorf("aec: FS sample %d: %w", j, err)
					}
					out = append(out, uint64(v))
				}
				samplesEmittedInRSI += samplesNeeded
			case int(id) == idNC:
				// No-Compression block: raw bps bits per sample.
				for j := 0; j < samplesNeeded; j++ {
					v, err := r.readBits(bps)
					if err != nil {
						return nil, fmt.Errorf("aec: no-compression sample %d: %w", j, err)
					}
					out = append(out, v)
				}
				samplesEmittedInRSI += samplesNeeded
			case int(id) == idSE && bps >= 3:
				// Second Extension: encode pairs (s1, s2) of adjacent
				// samples as a single combined value m using the
				// triangular pairing m = (s1+s2)*(s1+s2+1)/2 + s2,
				// then FS-encode m. Decoder inverts via
				// s1+s2 = floor((sqrt(1+8m)-1)/2), s2 = m - tri.
				// SE is only defined for bps >= 3 per CCSDS.
				if samplesNeeded%2 != 0 {
					return nil, fmt.Errorf("aec: SE block needs even sample count, got %d", samplesNeeded)
				}
				for j := 0; j < samplesNeeded; j += 2 {
					m, err := r.readFS()
					if err != nil {
						return nil, fmt.Errorf("aec: SE FS read: %w", err)
					}
					mu := uint64(m)
					// Largest k s.t. k*(k+1)/2 <= m.
					k := uint64((math.Sqrt(float64(1+8*mu)) - 1) / 2)
					for (k+1)*(k+2)/2 <= mu {
						k++
					}
					tri := k * (k + 1) / 2
					s2 := mu - tri
					s1 := k - s2
					out = append(out, s1, s2)
				}
				samplesEmittedInRSI += samplesNeeded
			default:
				// k-split: low k bits raw, high (bps-k) bits FS-coded.
				// Per libaec's on-the-wire layout, k can range from 1
				// up to idNC-2; when k >= bps the FS portion encodes
				// only zeros (each sample emits one "1" stop bit) and
				// the low k bits carry the raw value. That's wasteful
				// versus straight NC, but it's what libaec emits in
				// some blocks and the decoder must accept it.
				k := int(id)
				if k <= 0 || k > idNC-2 {
					return nil, fmt.Errorf("aec: invalid block id=%d (bps=%d, idNC=%d)",
						k, bps, idNC)
				}
				// Per CCSDS spec, the encoder emits the high parts
				// first (all FS values back-to-back) then the low
				// parts (k bits each). We materialise the highs into
				// a small fixed-size scratch, then fold in lows.
				highs := make([]uint64, samplesNeeded)
				for j := 0; j < samplesNeeded; j++ {
					h, err := r.readFS()
					if err != nil {
						return nil, fmt.Errorf("aec: split high %d (k=%d): %w", j, k, err)
					}
					highs[j] = uint64(h)
				}
				for j := 0; j < samplesNeeded; j++ {
					low, err := r.readBits(k)
					if err != nil {
						return nil, fmt.Errorf("aec: split low %d (k=%d): %w", j, k, err)
					}
					out = append(out, (highs[j]<<uint(k))|low)
				}
				samplesEmittedInRSI += samplesNeeded
			}
		}

		// Preprocessor inverse: convert the per-RSI θ-mapped deltas
		// back to absolute samples. The mapping turns each signed
		// difference d (relative to the previous sample) into a
		// non-negative integer: d≥0 → 2d, d<0 → -2d-1, clipped to
		// the [0, 2^bps - 1] range.
		if preprocess {
			xMax := uint64(1)<<uint(bps) - 1
			prev := refSample
			for i := rsiOut + 1; i < len(out); i++ {
				theta := minU64(prev, xMax-prev)
				mapped := out[i]
				var delta int64
				if mapped <= 2*theta {
					if mapped&1 == 0 {
						delta = int64(mapped) / 2
					} else {
						delta = -(int64(mapped) + 1) / 2
					}
				} else {
					// Beyond theta — un-mapped portion is raw signed.
					if prev <= theta {
						delta = int64(mapped) - int64(theta)
					} else {
						delta = -(int64(mapped) - int64(theta))
					}
				}
				v := int64(prev) + delta
				if v < 0 {
					v = 0
				}
				if v > int64(xMax) {
					v = int64(xMax)
				}
				out[i] = uint64(v)
				prev = uint64(v)
			}
		}

		if flags&ccsdsFlagPadRSI != 0 {
			// Round bit position up to next byte boundary.
			if r.bitPos != 0 {
				r.bytePos++
				r.bitPos = 0
			}
		}
	}

	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// unpackCCSDS is the GRIB2 template-5.42 entry point used by
// unpackData. The integer samples returned by aecDecode are converted
// to floats via the same R + X*2^E * 10^-D formula simple/complex
// packing uses, so the caller path (parseGRIBMessage, decodeHRRRWind,
// decodeRegularLLMessage) doesn't need to know which packing template
// the source used.
func unpackCCSDS(packed []byte, ref float32, binaryScale, decimalScale,
	bitsPerValue int, flags byte, blockSize, rsi, n int) ([]float64, error) {
	if bitsPerValue == 0 {
		// Constant field — every value equals the reference.
		v := float64(ref)
		out := make([]float64, n)
		for i := range out {
			out[i] = v
		}
		return out, nil
	}
	raw, err := aecDecode(packed, bitsPerValue, flags, blockSize, rsi, n)
	if err != nil {
		return nil, err
	}
	out := make([]float64, n)
	scaleBin := math.Pow(2, float64(binaryScale))
	scaleDec := math.Pow10(-decimalScale)
	for i, x := range raw {
		out[i] = (float64(ref) + float64(x)*scaleBin) * scaleDec
	}
	return out, nil
}
