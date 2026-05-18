package vc

import "testing"

func TestSplitPathOnLongJumpsKeepsSingleRun(t *testing.T) {
	// All vertices within 50 m — no split.
	coords := [][]float64{
		{-76.6731, 34.7198},
		{-76.6731, 34.7199},
		{-76.6732, 34.7200},
		{-76.6732, 34.7201},
	}
	runs, split := splitPathOnLongJumps(coords, structurePhantomJumpM)
	if split {
		t.Fatalf("expected split=false, got true")
	}
	if len(runs) != 1 || len(runs[0]) != 4 {
		t.Fatalf("expected 1 run of 4 vertices, got %d run(s)", len(runs))
	}
}

func TestSplitPathOnLongJumpsAllPhantomZigzag(t *testing.T) {
	// BRIDGE id=293089860 from US5MRHDB — five km-scale phantom edges; every
	// fragment is a single vertex, so split=true but runs is empty. Drawing
	// path must skip this feature entirely (no fall-through to the old code).
	coords := [][]float64{
		{-76.6802657, 34.7163293},
		{-76.6517934, 34.7123529},
		{-76.6725778, 34.6969866},
		{-76.6725650, 34.7214490},
		{-76.6517743, 34.6993027},
		{-76.6802657, 34.7163293},
	}
	runs, split := splitPathOnLongJumps(coords, structurePhantomJumpM)
	if !split {
		t.Fatalf("expected split=true for pathological zigzag")
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 surviving runs, got %d", len(runs))
	}
}

func TestSplitPathOnLongJumpsPreservesPartialRun(t *testing.T) {
	// Two close vertices, then a km jump, then two more close vertices.
	// Expect two surviving runs of length 2 each.
	coords := [][]float64{
		{-76.670, 34.720},
		{-76.6705, 34.7202},
		{-76.660, 34.720}, // ~915 m jump
		{-76.6605, 34.7202},
	}
	runs, split := splitPathOnLongJumps(coords, structurePhantomJumpM)
	if !split {
		t.Fatalf("expected split=true")
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	for i, r := range runs {
		if len(r) != 2 {
			t.Fatalf("run %d: expected 2 vertices, got %d", i, len(r))
		}
	}
}

func TestSplitPathOnLongJumpsLeavesSubKmJumpsAlone(t *testing.T) {
	// 78 m jump inside BRIDGE id=963991579 — sub-threshold, should NOT split.
	coords := [][]float64{
		{-76.6733460, 34.7206379},
		{-76.6731346, 34.7199553}, // 78 m
		{-76.6730947, 34.7198290},
	}
	runs, split := splitPathOnLongJumps(coords, structurePhantomJumpM)
	if split {
		t.Fatalf("expected split=false for sub-threshold jumps")
	}
	if len(runs) != 1 || len(runs[0]) != 3 {
		t.Fatalf("expected 1 run of 3 vertices, got %d run(s)", len(runs))
	}
}

func TestHasPhantomEdgeBUAARERingFromDump(t *testing.T) {
	// BUAARE id=213351659 from US4NC15M — the yellow-X feature over Town
	// Marsh in the Beaufort tile. The polygon has dense detail on its
	// long axis plus four sub-tile phantom jumps (614/530/532/916 m).
	// hasPhantomEdge must return true so the fill is skipped.
	coords := [][]float64{
		{-76.663651, 34.722425},
		{-76.663566, 34.722407},
		{-76.663526, 34.722409},
		{-76.662184, 34.722113},
		{-76.655555, 34.721266}, // 614 m jump
		{-76.649766, 34.721111}, // 530 m jump
		{-76.643977, 34.720649}, // 532 m jump
		{-76.642017, 34.719647},
		{-76.645084, 34.711812}, // 916 m jump
	}
	if !hasPhantomEdge(coords, structurePhantomJumpM) {
		t.Fatalf("expected hasPhantomEdge=true for BUAARE-with-phantom-jumps")
	}
}

func TestCellBoundaryClipDetector(t *testing.T) {
	// Snapshot from LNDARE id=4208869652 (US3SC10M, 1:350k overview cell):
	// outer ring threads along lat=35.5 and lon=-80.0 for tens of km —
	// consecutive vertices share an exact coordinate. Detector must fire.
	coords := [][]float64{
		{-80.000001, 33.608500},
		{-80.000001, 34.408279}, // same lon, ~89 km
		{-80.000001, 35.333333}, // same lon, ~103 km
		{-80.000001, 35.500000},
		{-79.905332, 35.500000}, // same lat, ~9 km
		{-77.385456, 35.500000}, // same lat, ~126 km
	}
	if !hasCellBoundaryClipEdge(coords, overviewClipEdgeM) {
		t.Fatalf("expected hasCellBoundaryClipEdge to fire on cell-clipped overview ring")
	}
}

func TestCellBoundaryClipDetectorSparesRealCoastline(t *testing.T) {
	// Real coastline — no two consecutive vertices share an exact lat
	// or lon, so the detector must stay quiet even though some segments
	// are long. Threshold here exists to give the detector something to
	// reject; it's an alignment check, not a length check.
	coords := [][]float64{
		{-76.6802, 34.7163},
		{-76.6517, 34.7123},
		{-76.6725, 34.6969},
		{-76.6725, 34.7214}, // NOTE: same lon -76.6725 as previous!
		{-76.6517, 34.6993},
		{-76.6802, 34.7163},
	}
	// Two coords share -76.6725 lon — distance ~2.7km — under the 5km
	// threshold so the detector should not fire. Confirms threshold is
	// what enforces the discrimination (alignment alone is not enough).
	if hasCellBoundaryClipEdge(coords, overviewClipEdgeM) {
		t.Fatalf("detector wrongly fired on short coincidence-aligned edge")
	}
}

func TestCellBoundaryClipDetectorIgnoresOffAxis(t *testing.T) {
	// Long but diagonal edges — neither lon nor lat shared. Common in
	// any real polygon. Detector must not fire regardless of distance.
	coords := [][]float64{
		{-77.0, 34.0},
		{-78.0, 35.0}, // ~140 km diagonal
		{-79.0, 36.0}, // ~140 km diagonal
	}
	if hasCellBoundaryClipEdge(coords, overviewClipEdgeM) {
		t.Fatalf("detector wrongly fired on diagonal long edges")
	}
}

func TestHasPhantomEdgeRejectsCompactRing(t *testing.T) {
	// All vertices within 50 m of their neighbours — no phantom.
	coords := [][]float64{
		{-76.6731, 34.7198},
		{-76.6731, 34.7199},
		{-76.6732, 34.7200},
		{-76.6732, 34.7201},
		{-76.6731, 34.7198},
	}
	if hasPhantomEdge(coords, structurePhantomJumpM) {
		t.Fatalf("expected hasPhantomEdge=false for compact ring")
	}
}

func TestSplitPathOnLongJumpsOverviewLineString(t *testing.T) {
	// BRIDGE id=20938887 from US3SC10M (1:350k overview) — 3 vertices, both
	// adjacent pairs > 500 m. Same pathology as the polygon case: split=true,
	// runs is empty, do not draw.
	coords := [][]float64{
		{-76.6822669, 34.7211975},
		{-76.6882127, 34.7204436},
		{-76.6958523, 34.7203257},
	}
	runs, split := splitPathOnLongJumps(coords, structurePhantomJumpM)
	if !split {
		t.Fatalf("expected split=true")
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs, got %d", len(runs))
	}
}
