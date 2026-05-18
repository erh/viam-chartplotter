package vc

import (
	"encoding/binary"
	"fmt"
	"math"
)

// gribSections is the raw output of walking one GRIB2 message. We split
// section parsing from grid/data interpretation so the Lambert Conformal
// fetcher (HRRR) can reuse the section walker without going through the
// regular_ll-only parseGRIBMessage path.
type gribSections struct {
	discipline int
	center     int
	// Section 3 (Grid Definition) — raw bytes, including the 5-byte section
	// header. The grid template number is at sections3[12:14].
	section3 []byte
	// Section 4 (Product Definition) — for the param/category/surface fields.
	section4 []byte
	// Section 5 (Data Representation) — packing template + parameters.
	section5 []byte
	// Section 6 (Bit Map) — present-or-not indicator.
	section6 []byte
	// Section 7 (Data) — packed bytes (excluding the 5-byte section header).
	section7 []byte
	// Total bytes the message occupied.
	totalLen int
}

// walkGRIBMessage parses one GRIB2 message at b[0]. Returns the section
// blocks plus the number of bytes consumed. It does NOT decode the data
// values — that's per-template.
func walkGRIBMessage(b []byte) (*gribSections, error) {
	if len(b) < 16 || string(b[:4]) != "GRIB" {
		return nil, fmt.Errorf("missing GRIB magic")
	}
	edition := int(b[7])
	if edition != 2 {
		return nil, fmt.Errorf("only GRIB2 supported, got edition %d", edition)
	}
	totalLen := int(binary.BigEndian.Uint64(b[8:16]))
	if totalLen <= 0 || totalLen > len(b) {
		return nil, fmt.Errorf("bogus message length %d (have %d)", totalLen, len(b))
	}
	if string(b[totalLen-4:totalLen]) != "7777" {
		return nil, fmt.Errorf("missing 7777 terminator")
	}
	out := &gribSections{
		discipline: int(b[6]),
		totalLen:   totalLen,
	}
	off := 16
	for off < totalLen-4 {
		secLen := int(binary.BigEndian.Uint32(b[off : off+4]))
		if secLen <= 0 {
			return nil, fmt.Errorf("zero-length section at %d", off)
		}
		secNum := int(b[off+4])
		s := b[off : off+secLen]
		switch secNum {
		case 1:
			out.center = int(binary.BigEndian.Uint16(s[5:7]))
		case 3:
			out.section3 = s
		case 4:
			out.section4 = s
		case 5:
			out.section5 = s
		case 6:
			out.section6 = s
		case 7:
			out.section7 = s[5:]
		}
		off += secLen
	}
	return out, nil
}

// gribProductInfo pulls the param category/number + surface type/value
// out of a Section 4 (Product Definition) using template 4.0.
type gribProductInfo struct {
	paramCat  int
	paramNum  int
	surfType  int
	surfValue float64
}

func parseProductSection(s4 []byte) (gribProductInfo, error) {
	var p gribProductInfo
	if len(s4) < 29 {
		return p, fmt.Errorf("product section too short (%d bytes)", len(s4))
	}
	// Template 4.0 layout (0-indexed into the section body):
	//   9:  parameter category
	//   10: parameter number
	//   22: surface 1 type
	//   23: surface 1 scale factor
	//   24-27: surface 1 scaled value
	p.paramCat = int(s4[9])
	p.paramNum = int(s4[10])
	p.surfType = int(s4[22])
	scale := int(int8(s4[23]))
	scaled := int(binary.BigEndian.Uint32(s4[24:28]))
	p.surfValue = float64(scaled) * math.Pow10(-scale)
	return p, nil
}

// gribPackingInfo carries everything unpack* needs to decode the data
// values from Section 7 regardless of which packing template is in use.
type gribPackingInfo struct {
	template               int // 5.0 simple, 5.2 complex, 5.3 complex+spatial
	refValue               float32
	binaryScale            int
	decimalScale           int
	bitsPerValue           int
	dataValuesCount        int
	numGroups              int
	groupWidthRef          int
	groupWidthBits         int
	groupLengthRef         int
	groupLengthIncrement   int
	groupLengthLast        int
	groupLengthBits        int
	spatialDiffOrder       int
	spatialDiffExtraOctets int
}

func parsePackingSection(s5 []byte, valuesCount int) (gribPackingInfo, error) {
	var p gribPackingInfo
	if len(s5) < 20 {
		return p, fmt.Errorf("packing section too short (%d bytes)", len(s5))
	}
	p.dataValuesCount = valuesCount
	p.template = int(binary.BigEndian.Uint16(s5[9:11]))
	p.refValue = math.Float32frombits(binary.BigEndian.Uint32(s5[11:15]))
	p.binaryScale = int(int16(binary.BigEndian.Uint16(s5[15:17])))
	p.decimalScale = int(int16(binary.BigEndian.Uint16(s5[17:19])))
	p.bitsPerValue = int(s5[19])
	switch p.template {
	case 0:
		// Simple — no extra fields.
	case 2, 3:
		if len(s5) < 47 {
			return p, fmt.Errorf("complex-packing section too short (%d bytes)", len(s5))
		}
		p.numGroups = int(binary.BigEndian.Uint32(s5[31:35]))
		p.groupWidthRef = int(s5[35])
		p.groupWidthBits = int(s5[36])
		p.groupLengthRef = int(binary.BigEndian.Uint32(s5[37:41]))
		p.groupLengthIncrement = int(s5[41])
		p.groupLengthLast = int(binary.BigEndian.Uint32(s5[42:46]))
		p.groupLengthBits = int(s5[46])
		if p.template == 3 {
			if len(s5) < 49 {
				return p, fmt.Errorf("spatial-diff section too short (%d bytes)", len(s5))
			}
			p.spatialDiffOrder = int(s5[47])
			p.spatialDiffExtraOctets = int(s5[48])
		}
	default:
		// Unknown — caller decides whether to fail or skip.
	}
	return p, nil
}

// unpackData dispatches the packed bytes through the right per-template
// decoder. Returns ErrUnsupportedPacking for templates we haven't built
// pure-Go decoders for (e.g. JPEG2000 = 40, PNG = 41, CCSDS = 42).
func unpackData(packed []byte, p gribPackingInfo) ([]float64, error) {
	switch p.template {
	case 0:
		return unpackSimple(packed, p.refValue, p.binaryScale, p.decimalScale, p.bitsPerValue, p.dataValuesCount)
	case 2, 3:
		return unpackComplex(packed, p.refValue, p.binaryScale, p.decimalScale, p.bitsPerValue,
			p.numGroups, p.groupWidthRef, p.groupWidthBits,
			p.groupLengthRef, p.groupLengthIncrement, p.groupLengthLast, p.groupLengthBits,
			p.spatialDiffOrder, p.spatialDiffExtraOctets, p.dataValuesCount)
	default:
		return nil, &ErrUnsupportedPacking{Template: p.template}
	}
}

// ErrUnsupportedPacking is returned when we walk a GRIB2 message whose
// data section uses a packing template we don't decode (e.g. JPEG2000
// for NAM / GFSWAVE, CCSDS for ECMWF). The model registry surfaces this
// as a "decoder missing" status to the UI instead of a hard server
// error so the picker can stay populated.
type ErrUnsupportedPacking struct{ Template int }

func (e *ErrUnsupportedPacking) Error() string {
	return fmt.Sprintf("GRIB2 packing template %d not implemented", e.Template)
}
