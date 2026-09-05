package noaa

import "testing"

func TestFishHavenSurfacesEarlierThanOtherObstructions(t *testing.T) {
	haven := map[string]any{"CATOBS": 5}
	wellhead := map[string]any{"CATOBS": 2}

	if got := MinZoomForFeature("OBSTRN", haven); got != FishHavenMinZoom {
		t.Errorf("fish haven minZoom = %d, want %d", got, FishHavenMinZoom)
	}
	if got := MinZoomForFeature("OBSTRN", wellhead); got != MinZoomForObjectClass("OBSTRN") {
		t.Errorf("wellhead minZoom = %d, want the class default", got)
	}
	if got := MinZoomForFeature("OBSTRN", nil); got != MinZoomForObjectClass("OBSTRN") {
		t.Errorf("attribute-less obstruction minZoom = %d, want the class default", got)
	}
	// BSON hands numbers back as int32/float64 depending on the driver path;
	// the classifier has to see a fish haven either way.
	for _, v := range []any{int32(5), float64(5), int64(5)} {
		if got := MinZoomForFeature("OBSTRN", map[string]any{"CATOBS": v}); got != FishHavenMinZoom {
			t.Errorf("CATOBS as %T: minZoom = %d, want %d", v, got, FishHavenMinZoom)
		}
	}
}

func TestMinZoomForDrawNeverTightens(t *testing.T) {
	// The draw guard may surface a feature earlier than its class allows but
	// must never drop one the query deliberately surfaced below its stored
	// minZoom (coarse contours at overview zoom, named wrecks as landmarks).
	for _, class := range []string{"DEPCNT", "SOUNDG", "WRECKS", "OBSTRN", "LNDARE"} {
		attrs := map[string]any{"VALDCO": 1.5, "CATOBS": 1}
		if got, want := MinZoomForDraw(class, attrs), MinZoomForObjectClass(class); got > want {
			t.Errorf("%s: draw guard %d is tighter than class %d", class, got, want)
		}
	}
	if got := MinZoomForDraw("OBSTRN", map[string]any{"CATOBS": 5}); got != FishHavenMinZoom {
		t.Errorf("fish haven draw guard = %d, want %d", got, FishHavenMinZoom)
	}
}

func TestPseudoClassOnlyRenamesFishHavens(t *testing.T) {
	if got := PseudoClass("OBSTRN", map[string]any{"CATOBS": 5}); got != ClassFishHaven {
		t.Errorf("fish haven presents as %q, want %q", got, ClassFishHaven)
	}
	if got := PseudoClass("OBSTRN", map[string]any{"CATOBS": 1}); got != "OBSTRN" {
		t.Errorf("snag presents as %q, want OBSTRN", got)
	}
	if got := PseudoClass("WRECKS", map[string]any{"CATOBS": 5}); got != "WRECKS" {
		t.Errorf("a wreck is never a fish haven, got %q", got)
	}
}
