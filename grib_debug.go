package vc

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// DebugDumpGRIB walks every GRIB2 message in `grib`, prints a per-
// section summary, and tries to unpack the data section using the
// in-tree decoder, printing min/max/mean and the first 10 values.
// Used by cmd/ecmwf-probe to validate the CCSDS decoder against
// captured ECMWF wire data. The function does not return on
// per-message decode errors — it prints the error and moves on to
// the next message — so the trace shows partial progress even when
// a later block trips the unpacker.
func DebugDumpGRIB(grib []byte, w io.Writer) error {
	for off, msg := 0, 0; off < len(grib); msg++ {
		secs, err := walkGRIBMessage(grib[off:])
		if err != nil {
			return fmt.Errorf("message %d @ off=%d: %w", msg, off, err)
		}
		fmt.Fprintf(w, "\n=== message %d  off=%d  totalLen=%d  discipline=%d  center=%d ===\n",
			msg, off, secs.totalLen, secs.discipline, secs.center)

		if len(secs.section3) >= 38 {
			gridTemplate := binary.BigEndian.Uint16(secs.section3[12:14])
			nPoints := binary.BigEndian.Uint32(secs.section3[6:10])
			nx := binary.BigEndian.Uint32(secs.section3[30:34])
			ny := binary.BigEndian.Uint32(secs.section3[34:38])
			fmt.Fprintf(w, "section3: gridTemplate=%d nPoints=%d nx=%d ny=%d\n",
				gridTemplate, nPoints, nx, ny)
		}

		if len(secs.section4) >= 28 {
			prod, err := parseProductSection(secs.section4)
			if err != nil {
				fmt.Fprintf(w, "section4: parse error: %v\n", err)
			} else {
				fmt.Fprintf(w, "section4: paramCat=%d paramNum=%d surfType=%d surfValue=%v\n",
					prod.paramCat, prod.paramNum, prod.surfType, prod.surfValue)
			}
		}

		nPoints := 0
		if len(secs.section3) >= 10 {
			nPoints = int(binary.BigEndian.Uint32(secs.section3[6:10]))
		}
		pack, err := parsePackingSection(secs.section5, nPoints)
		if err != nil {
			fmt.Fprintf(w, "section5: parse error: %v\n", err)
			off += secs.totalLen
			continue
		}
		fmt.Fprintf(w, "section5: template=%d ref=%v binScale=%d decScale=%d bps=%d",
			pack.template, pack.refValue, pack.binaryScale, pack.decimalScale, pack.bitsPerValue)
		if pack.template == 42 {
			fmt.Fprintf(w, " ccsdsFlags=0x%02x blockSize=%d rsi=%d",
				pack.ccsdsFlags, pack.ccsdsBlockSize, pack.ccsdsRSI)
		}
		fmt.Fprintln(w)

		if secs.section6 != nil && len(secs.section6) >= 6 {
			bitmapIndicator := secs.section6[5]
			fmt.Fprintf(w, "section6: bitmapIndicator=0x%02x (255 = no bitmap)\n", bitmapIndicator)
			if bitmapIndicator != 0xFF {
				fmt.Fprintf(w, "  WARN: bitmap not supported by the in-tree decoder\n")
			}
		}

		fmt.Fprintf(w, "section7: %d packed bytes\n", len(secs.section7))

		values, err := unpackData(secs.section7, pack)
		if err != nil {
			fmt.Fprintf(w, "unpack ERROR: %v\n", err)
			off += secs.totalLen
			continue
		}
		dumpStats(w, values)

		off += secs.totalLen
	}
	return nil
}

// DebugDumpUnpack runs the CCSDS unpacker directly against a raw
// packed bitstream with caller-supplied template parameters. Useful
// when a captured section-7 has been carved out into a standalone
// file and you want to bisect the bug by hand.
func DebugDumpUnpack(packed []byte, ref float32, binScale, decScale,
	bps int, ccsdsFlags byte, blockSize, rsi, n int, w io.Writer) error {
	fmt.Fprintf(w, "unpackCCSDS: ref=%v binScale=%d decScale=%d bps=%d flags=0x%02x bs=%d rsi=%d n=%d\n",
		ref, binScale, decScale, bps, ccsdsFlags, blockSize, rsi, n)
	values, err := unpackCCSDS(packed, ref, binScale, decScale, bps, ccsdsFlags, blockSize, rsi, n)
	if err != nil {
		return err
	}
	dumpStats(w, values)
	return nil
}

func dumpStats(w io.Writer, values []float64) {
	if len(values) == 0 {
		fmt.Fprintln(w, "values: empty")
		return
	}
	mn, mx := values[0], values[0]
	sum := 0.0
	nan := 0
	for _, v := range values {
		if math.IsNaN(v) {
			nan++
			continue
		}
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		sum += v
	}
	mean := sum / float64(len(values)-nan)
	fmt.Fprintf(w, "values: n=%d min=%v max=%v mean=%v nans=%d\n", len(values), mn, mx, mean, nan)
	head := values
	if len(head) > 10 {
		head = head[:10]
	}
	fmt.Fprintf(w, "  head: %v\n", head)
	if len(values) > 20 {
		tail := values[len(values)-10:]
		fmt.Fprintf(w, "  tail: %v\n", tail)
	}
}

