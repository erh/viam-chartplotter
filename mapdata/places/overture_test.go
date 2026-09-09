package places

import (
	"testing"

	"go.viam.com/test"
)

func row(over func(*OvertureRow)) OvertureRow {
	r := OvertureRow{
		Name:       "Cobb's Marina",
		Category:   "marina",
		Street:     "4524 Dunning Rd",
		City:       "Norfolk",
		State:      "VA",
		Lng:        -76.1786,
		Lat:        36.9203,
		Confidence: 0.99,
	}
	if over != nil {
		over(&r)
	}
	return r
}

func TestPlaceFromOverture(t *testing.T) {
	p := PlaceFromOverture(row(nil))
	test.That(t, p, test.ShouldNotBeNil)
	test.That(t, p.Source, test.ShouldEqual, SourceOverture)
	test.That(t, p.Name, test.ShouldEqual, "Cobb's Marina")
	test.That(t, p.Class, test.ShouldEqual, "marina")
	test.That(t, p.Street, test.ShouldEqual, "4524 Dunning Rd")
	test.That(t, p.City, test.ShouldEqual, "Norfolk")
	test.That(t, p.State, test.ShouldEqual, "VA")
	test.That(t, p.BBox, test.ShouldResemble, [4]float64{-76.1786, 36.9203, -76.1786, 36.9203})
	// Idempotent identity: the same row maps to the same document id.
	test.That(t, p.ID, test.ShouldEqual, PlaceFromOverture(row(nil)).ID)
}

func TestPlaceFromOvertureRejectsJunk(t *testing.T) {
	test.That(t, PlaceFromOverture(row(func(r *OvertureRow) { r.Name = "" })), test.ShouldBeNil)
	test.That(t, PlaceFromOverture(row(func(r *OvertureRow) { r.Category = "" })), test.ShouldBeNil)
	// Conflation noise sits below the confidence floor.
	test.That(t, PlaceFromOverture(row(func(r *OvertureRow) { r.Confidence = 0.3 })), test.ShouldBeNil)
	// Null island and out-of-range coordinates.
	test.That(t, PlaceFromOverture(row(func(r *OvertureRow) { r.Lat, r.Lng = 0, 0 })), test.ShouldBeNil)
	test.That(t, PlaceFromOverture(row(func(r *OvertureRow) { r.Lat = 95 })), test.ShouldBeNil)
	// A place with no address is still a place.
	test.That(t, PlaceFromOverture(row(func(r *OvertureRow) { r.Street, r.City, r.State = "", "", "" })), test.ShouldNotBeNil)
}
