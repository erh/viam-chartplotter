package parser

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/beetlebugorg/iso8211/pkg/iso8211"
)

// Tests for positional update control (SGCC/VRPC/FSPC), FRID-keyed feature
// updates, and RCNM-exact spatial lookup. Each of these, done wrong, produced
// shoreline geometry whose vertices jump kilometres across the chart once an
// ENC's .001+ update files were applied (seen in US5NYCEG and US5PHLBB).

func ctlBytes(instr UpdateInstruction, index, count int) []byte {
	b := []byte{byte(instr), 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(b[1:3], uint16(index))
	binary.LittleEndian.PutUint16(b[3:5], uint16(count))
	return b
}

func vrid(rcnm spatialType, rcid uint32, ruin UpdateInstruction) []byte {
	b := make([]byte, 8)
	b[0] = byte(rcnm)
	binary.LittleEndian.PutUint32(b[1:5], rcid)
	binary.LittleEndian.PutUint16(b[5:7], 2)
	b[7] = byte(ruin)
	return b
}

func frid(rcid uint32, ruin UpdateInstruction) []byte {
	b := make([]byte, 12)
	b[0] = 100
	binary.LittleEndian.PutUint32(b[1:5], rcid)
	b[5] = 2 // line
	binary.LittleEndian.PutUint16(b[7:9], 30)
	binary.LittleEndian.PutUint16(b[9:11], 2)
	b[11] = byte(ruin)
	return b
}

// sg2d encodes [lon, lat] pairs at COMF 10^7.
func sg2d(pts ...[2]float64) []byte {
	b := make([]byte, 0, 8*len(pts))
	for _, p := range pts {
		var buf [8]byte
		binary.LittleEndian.PutUint32(buf[0:4], uint32(int32(p[1]*1e7)))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(int32(p[0]*1e7)))
		b = append(b, buf[:]...)
	}
	return b
}

func TestApplyControl(t *testing.T) {
	base := []int{1, 2, 3, 4, 5}
	cases := []struct {
		name  string
		items []int
		ctl   updateControl
		want  []int
	}{
		{"insert before first", []int{9}, updateControl{UpdateInsert, 1, 1}, []int{9, 1, 2, 3, 4, 5}},
		{"insert middle", []int{8, 9}, updateControl{UpdateInsert, 3, 2}, []int{1, 2, 8, 9, 3, 4, 5}},
		{"insert append", []int{9}, updateControl{UpdateInsert, 6, 1}, []int{1, 2, 3, 4, 5, 9}},
		{"delete middle", nil, updateControl{UpdateDelete, 2, 3}, []int{1, 5}},
		{"delete last", nil, updateControl{UpdateDelete, 5, 1}, []int{1, 2, 3, 4}},
		{"modify middle", []int{7, 8}, updateControl{UpdateModify, 2, 2}, []int{1, 7, 8, 4, 5}},
	}
	for _, c := range cases {
		got, err := applyControl(base, c.items, c.ctl)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !reflect.DeepEqual(base, []int{1, 2, 3, 4, 5}) {
		t.Errorf("base mutated: %v", base)
	}

	for _, bad := range []updateControl{
		{UpdateInsert, 0, 1},
		{UpdateInsert, 7, 1},
		{UpdateDelete, 4, 3},
		{UpdateModify, 5, 2},
		{UpdateInstruction(9), 1, 1},
	} {
		if _, err := applyControl(base, []int{1, 2}, bad); err == nil {
			t.Errorf("%+v: expected error", bad)
		}
	}
}

// An edge MODIFY carrying SGCC modify 1 coordinate must change only that
// vertex, not collapse the edge to the update's single coordinate.
func TestSpatialModifySGCC(t *testing.T) {
	key := spatialKey{RCNM: int(spatialTypeEdge), RCID: 7}
	chart := &chartData{spatialRecords: map[spatialKey]*spatialRecord{
		key: {ID: 7, RecordType: spatialTypeEdge, Coordinates: [][]float64{{1, 1}, {2, 2}, {3, 3}}},
	}}
	params := datasetParams{COMF: 10000000, SOMF: 10}

	modify := &iso8211.DataRecord{Fields: map[string][]byte{
		"VRID": vrid(spatialTypeEdge, 7, UpdateModify),
		"SGCC": ctlBytes(UpdateModify, 2, 1),
		"SG2D": sg2d([2]float64{2.5, 2.5}),
	}}
	if err := applySpatialUpdate(chart, modify, modify.Fields["VRID"], params); err != nil {
		t.Fatal(err)
	}
	if got, want := chart.spatialRecords[key].Coordinates, [][]float64{{1, 1}, {2.5, 2.5}, {3, 3}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after SGCC modify: got %v, want %v", got, want)
	}

	// DELETE of a coordinate carries SGCC and no SG2D at all.
	del := &iso8211.DataRecord{Fields: map[string][]byte{
		"VRID": vrid(spatialTypeEdge, 7, UpdateModify),
		"SGCC": ctlBytes(UpdateDelete, 1, 1),
	}}
	if err := applySpatialUpdate(chart, del, del.Fields["VRID"], params); err != nil {
		t.Fatal(err)
	}
	if got, want := chart.spatialRecords[key].Coordinates, [][]float64{{2.5, 2.5}, {3, 3}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after SGCC delete: got %v, want %v", got, want)
	}
}

// Feature updates are addressed by FRID RCID. NOAA DELETE records carry no
// FOID, so keying on FOID silently skipped them and left the feature pointing
// at edges the same update deleted.
func TestFeatureDeleteByRCIDWithoutFOID(t *testing.T) {
	keep := &featureRecord{RCID: 60, FIDN: 111}
	gone := &featureRecord{RCID: 61, FIDN: 222}
	chart := &chartData{
		features:       []*featureRecord{keep, gone},
		featuresByRCID: map[int64]*featureRecord{60: keep, 61: gone},
	}
	del := &iso8211.DataRecord{Fields: map[string][]byte{"FRID": frid(61, UpdateDelete)}}
	if err := applyFeatureUpdate(chart, del, del.Fields["FRID"]); err != nil {
		t.Fatal(err)
	}
	if len(chart.features) != 1 || chart.features[0] != keep {
		t.Fatalf("features after delete: %+v", chart.features)
	}
	if _, ok := chart.featuresByRCID[61]; ok {
		t.Fatal("deleted feature still indexed")
	}
}

// ATTF in a MODIFY changes only the attributes it names; 0x7F deletes one.
func TestFeatureModifyMergesAttributes(t *testing.T) {
	f := &featureRecord{RCID: 5, Attributes: map[string]interface{}{"OBJNAM": "Old", "INFORM": "keep me"}}
	chart := &chartData{features: []*featureRecord{f}, featuresByRCID: map[int64]*featureRecord{5: f}}

	attf := func(code uint16, val string) []byte {
		b := []byte{0, 0}
		binary.LittleEndian.PutUint16(b, code)
		return append(append(b, val...), 0x1F)
	}
	objnam := uint16(116)
	modify := &iso8211.DataRecord{Fields: map[string][]byte{
		"FRID": frid(5, UpdateModify),
		"ATTF": attf(objnam, "New"),
	}}
	if err := applyFeatureUpdate(chart, modify, modify.Fields["FRID"]); err != nil {
		t.Fatal(err)
	}
	if want := map[string]interface{}{"OBJNAM": "New", "INFORM": "keep me"}; !reflect.DeepEqual(f.Attributes, want) {
		t.Fatalf("after modify: got %v, want %v", f.Attributes, want)
	}

	del := &iso8211.DataRecord{Fields: map[string][]byte{
		"FRID": frid(5, UpdateModify),
		"ATTF": attf(objnam, attrDeleteValue),
	}}
	if err := applyFeatureUpdate(chart, del, del.Fields["FRID"]); err != nil {
		t.Fatal(err)
	}
	if want := map[string]interface{}{"INFORM": "keep me"}; !reflect.DeepEqual(f.Attributes, want) {
		t.Fatalf("after delete marker: got %v, want %v", f.Attributes, want)
	}
}

// A feature MODIFY with FSPC inserts pointers positionally.
func TestFeatureModifyFSPC(t *testing.T) {
	f := &featureRecord{RCID: 5, SpatialRefs: []spatialRef{{RCNM: 130, RCID: 1}, {RCNM: 130, RCID: 3}}}
	chart := &chartData{features: []*featureRecord{f}, featuresByRCID: map[int64]*featureRecord{5: f}}
	fspt := []byte{130, 2, 0, 0, 0, 1, 255, 255}
	modify := &iso8211.DataRecord{Fields: map[string][]byte{
		"FRID": frid(5, UpdateModify),
		"FSPC": ctlBytes(UpdateInsert, 2, 1),
		"FSPT": fspt,
	}}
	if err := applyFeatureUpdate(chart, modify, modify.Fields["FRID"]); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range f.SpatialRefs {
		ids = append(ids, r.RCID)
	}
	if !reflect.DeepEqual(ids, []int64{1, 2, 3}) {
		t.Fatalf("FSPT after insert: %v", ids)
	}
}

// RCIDs are unique only within a record type. A pointer to a deleted edge must
// not resolve to a connected node that shares its RCID.
func TestLookupSpatialRefHonoursRCNM(t *testing.T) {
	node := &spatialRecord{ID: 640, RecordType: spatialTypeConnectedNode, Coordinates: [][]float64{{1, 1}}}
	records := map[spatialKey]*spatialRecord{
		{RCNM: int(spatialTypeConnectedNode), RCID: 640}: node,
	}
	edgeRef := spatialRef{RCNM: int(spatialTypeEdge), RCID: 640}
	if got := lookupSpatialRef(edgeRef, records, spatialTypeEdge, spatialTypeConnectedNode); got != nil {
		t.Fatalf("edge ref resolved to %+v; want nil", got)
	}
	untyped := spatialRef{RCID: 640}
	if got := lookupSpatialRef(untyped, records, spatialTypeEdge, spatialTypeConnectedNode); got != node {
		t.Fatalf("untyped ref: got %+v, want the node", got)
	}

	f := &featureRecord{GeomPrim: 2, SpatialRefs: []spatialRef{edgeRef}}
	g, err := constructLineStringGeometry(f, records)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Coordinates) != 0 {
		t.Fatalf("line from deleted edge got coordinates %v", g.Coordinates)
	}
}
