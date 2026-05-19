package vc

import (
	"fmt"
	"log"
	"math"
)

// AECDebug, when set to a non-nil logger, causes aecDecode to print
// a one-line summary per block decoded (ID, option, samples emitted,
// bit position). Off by default — production servers should leave it
// nil. The cmd/ecmwf-probe diagnostic tool toggles it on to make the
// decoder traceable against a captured ECMWF file when iterating on
// the spec interpretation.
var AECDebug *log.Logger

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
	rsiIdx := 0
	for len(out) < n {
		// Determine the number of blocks in this RSI. libaec encoders
		// write ceil(n / block_size) blocks total, grouped in `rsi`-
		// block reference-sample intervals — a full RSI for all but
		// the last, then a short RSI of ceil(remaining / block_size)
		// blocks. We must NOT read `rsi` blocks for the short last
		// RSI, otherwise we'd consume bits the encoder never wrote
		// and overrun the section-7 buffer (this is what surfaced as
		// "no-compression sample N: read past end of stream"
		// thousands of samples into the field).
		nBlocks := rsi
		remaining := n - len(out)
		fullRSISamples := rsi * blockSize
		if remaining < fullRSISamples {
			nBlocks = (remaining + blockSize - 1) / blockSize
			if nBlocks < 1 {
				nBlocks = 1
			}
		}

		// rsiOut indexes into out for the start of the current RSI so we
		// can apply the preprocessor inverse at the end (theta-mapping).
		rsiOut := len(out)

		var refSample uint64
		if preprocess {
			v, err := r.readBits(bps)
			if err != nil {
				return nil, fmt.Errorf("aec: rsi=%d reference sample: %w", rsiIdx, err)
			}
			refSample = v
			out = append(out, refSample)
		}

		if AECDebug != nil {
			AECDebug.Printf("aec: rsi=%d blocks=%d remaining=%d ref=%d bytePos=%d bitPos=%d",
				rsiIdx, nBlocks, remaining, refSample, r.bytePos, r.bitPos)
		}

		for blockIndex := 0; blockIndex < nBlocks; blockIndex++ {
			// First block consumes one fewer coded sample when the ref
			// already occupies its first slot. All other blocks always
			// encode exactly `block_size` samples (the encoder pads
			// the very last block's samples to fill out to block_size
			// — those padded samples get truncated from `out` at the
			// end of this function).
			samplesNeeded := blockSize
			if blockIndex == 0 && preprocess {
				samplesNeeded = blockSize - 1
			}
			bytePosBefore := r.bytePos
			bitPosBefore := r.bitPos

			id, err := r.readBits(idBits)
			if err != nil {
				return nil, fmt.Errorf("aec: rsi=%d block=%d ID: %w", rsiIdx, blockIndex, err)
			}

			var optionName string
			switch {
			case id == 0:
				// libaec uses ID=0 unconditionally as the zero-block
				// code (NOT as a generic FS-only block). A single
				// FS-encoded value m maps to a run of all-θ-mapped-
				// zero blocks per the ROS rules below; for runs of 5
				// or more the encoded m is decremented by 1 vs the
				// block count. A previous revision of this decoder
				// read 32 FS values here (treating ID=0 as FS-only),
				// which over-consumed roughly 31 sample-bits per
				// zero-block region of the field — that's what
				// surfaced in production as "read past end of stream"
				// hundreds of RSIs in.
				const ROS = 5
				m, err := r.readFS()
				if err != nil {
					return nil, fmt.Errorf("aec: rsi=%d block=%d zero-block FS: %w", rsiIdx, blockIndex, err)
				}
				var zeroBlocks int
				switch {
				case m+1 < ROS:
					// m=0..3 → 1..4 zero blocks.
					zeroBlocks = m + 1
				case m+1 == ROS:
					// "Rest of segment" sentinel. libaec clamps to the
					// remaining blocks of the RSI and to a 64-block
					// modular alignment that comes from CCSDS 121.0-B-2.
					rem := nBlocks - blockIndex
					modAlign := 64 - (blockIndex % 64)
					if rem < modAlign {
						zeroBlocks = rem
					} else {
						zeroBlocks = modAlign
					}
				default:
					// m >= 5: encoded count is decremented by 1
					// versus literal m+1 to make room for the ROS code.
					zeroBlocks = m
				}
				if zeroBlocks > nBlocks-blockIndex {
					zeroBlocks = nBlocks - blockIndex
				}
				if zeroBlocks < 1 {
					zeroBlocks = 1
				}
				optionName = fmt.Sprintf("ZB×%d (m=%d)", zeroBlocks, m)
				// Emit zero deltas across all covered blocks. The
				// first covered block respects the ref-on accounting
				// (one fewer slot), every subsequent block emits a
				// full block_size of zeros.
				for b := 0; b < zeroBlocks; b++ {
					emit := blockSize
					if (blockIndex+b) == 0 && preprocess {
						emit = blockSize - 1
					}
					for j := 0; j < emit; j++ {
						out = append(out, 0)
					}
				}
				// Advance blockIndex past the additional covered
				// blocks; the outer for-loop's ++ adds the last 1.
				blockIndex += zeroBlocks - 1
			case int(id) == idNC:
				optionName = "NC"
				// No-Compression block: raw bps bits per sample.
				for j := 0; j < samplesNeeded; j++ {
					v, err := r.readBits(bps)
					if err != nil {
						return nil, fmt.Errorf("aec: rsi=%d block=%d NC sample %d: %w", rsiIdx, blockIndex, j, err)
					}
					out = append(out, v)
				}
			case int(id) == idSE && bps >= 3:
				optionName = "SE"
				// Second Extension: each FS-coded value m decodes into
				// a pair (s1, s2) via the triangular pairing
				// m = (s1+s2)*(s1+s2+1)/2 + s2 → s1+s2 = floor((√(1+8m)-1)/2),
				// s2 = m - tri, s1 = (s1+s2) - s2.
				//
				// libaec's loop tracks the per-block sample index i
				// (starting at 1 when the preprocessor places a
				// reference at position 0 of block 0, else 0). On
				// each iteration it reads one m: if i is even the
				// pair (s1, s2) is emitted (advancing by 2); if i is
				// odd only s2 is emitted (the s1 half is the "pair
				// partner" for the absent slot before sample 1, and
				// is discarded). That's how SE coexists with an odd
				// remaining-sample count in ref-on block 0; earlier
				// revisions of this decoder errored out on that case,
				// which is what surfaced in production as
				// "SE block needs even sample count, got 31".
				blockI := 0
				if blockIndex == 0 && preprocess {
					blockI = 1
				}
				emitted := 0
				for emitted < samplesNeeded {
					m, err := r.readFS()
					if err != nil {
						return nil, fmt.Errorf("aec: rsi=%d block=%d SE FS: %w", rsiIdx, blockIndex, err)
					}
					mu := uint64(m)
					k := uint64((math.Sqrt(float64(1+8*mu)) - 1) / 2)
					for (k+1)*(k+2)/2 <= mu {
						k++
					}
					tri := k * (k + 1) / 2
					s2 := mu - tri
					s1 := k - s2
					if blockI%2 == 0 {
						out = append(out, s1)
						blockI++
						emitted++
						if emitted >= samplesNeeded {
							break
						}
					}
					out = append(out, s2)
					blockI++
					emitted++
				}
			default:
				optionName = fmt.Sprintf("k=%d", id)
				// k-split: low k bits raw, high (bps-k) bits FS-coded.
				// Per libaec's on-the-wire layout, k can range from 1
				// up to idNC-2; when k >= bps the FS portion encodes
				// only zeros (each sample emits one "1" stop bit) and
				// the low k bits carry the raw value. That's wasteful
				// versus straight NC, but it's what libaec emits in
				// some blocks and the decoder must accept it.
				k := int(id)
				if k <= 0 || k > idNC-2 {
					return nil, fmt.Errorf("aec: rsi=%d block=%d invalid id=%d (bps=%d, idNC=%d)",
						rsiIdx, blockIndex, k, bps, idNC)
				}
				// Per CCSDS spec, the encoder emits the high parts
				// first (all FS values back-to-back) then the low
				// parts (k bits each). We materialise the highs into
				// a small fixed-size scratch, then fold in lows.
				highs := make([]uint64, samplesNeeded)
				for j := 0; j < samplesNeeded; j++ {
					h, err := r.readFS()
					if err != nil {
						return nil, fmt.Errorf("aec: rsi=%d block=%d split high %d (k=%d): %w", rsiIdx, blockIndex, j, k, err)
					}
					highs[j] = uint64(h)
				}
				for j := 0; j < samplesNeeded; j++ {
					low, err := r.readBits(k)
					if err != nil {
						return nil, fmt.Errorf("aec: rsi=%d block=%d split low %d (k=%d): %w", rsiIdx, blockIndex, j, k, err)
					}
					out = append(out, (highs[j]<<uint(k))|low)
				}
			}
			if AECDebug != nil {
				bitsConsumed := (r.bytePos-bytePosBefore)*8 + int(r.bitPos) - int(bitPosBefore)
				AECDebug.Printf("aec:   block=%d id=%d (%s) samples=%d bits=%d bytePos=%d",
					blockIndex, id, optionName, samplesNeeded, bitsConsumed, r.bytePos)
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
		rsiIdx++
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
