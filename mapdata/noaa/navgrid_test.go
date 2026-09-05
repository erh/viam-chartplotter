package noaa

import (
	"math"
	"testing"

	"go.viam.com/test"
)

func TestNavTileCompressionRoundTrips(t *testing.T) {
	n := NavTileSize * NavTileSize
	depth := make([]int16, n)
	flags := make([]uint8, n)
	for i := range depth {
		switch i % 4 {
		case 0:
			depth[i] = NavDepthUncharted
		case 1:
			depth[i] = int16(i % 3000)
		case 2:
			depth[i] = -15 // a drying area
		default:
			depth[i] = math.MaxInt16
		}
		flags[i] = uint8(i % 32)
	}

	cd, err := compressInt16(depth)
	test.That(t, err, test.ShouldBeNil)
	cf, err := compressBytes(flags)
	test.That(t, err, test.ShouldBeNil)

	gotDepth, err := decompressInt16(cd, n)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotDepth, test.ShouldResemble, depth)

	gotFlags, err := decompressBytes(cf, n)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotFlags, test.ShouldResemble, flags)
}

func TestNavTileCompressionShrinksRealisticTiles(t *testing.T) {
	// A real tile is mostly runs of the same value — open water, solid land.
	// The storage estimate this design rests on depends on that compressing.
	n := NavTileSize * NavTileSize
	depth := make([]int16, n)
	for i := range depth {
		if i < n/2 {
			depth[i] = 180 // 18 m of open water
		} else {
			depth[i] = NavDepthUncharted
		}
	}
	c, err := compressInt16(depth)
	test.That(t, err, test.ShouldBeNil)
	// Raw is 128 KB; anything near that would make the whole approach pointless.
	test.That(t, len(c), test.ShouldBeLessThan, n*2/50)
}

func TestDecompressRejectsWrongSize(t *testing.T) {
	// A truncated or foreign document must be rejected, not silently decoded
	// into a tile that reads as uncharted water.
	c, err := compressInt16(make([]int16, 16))
	test.That(t, err, test.ShouldBeNil)
	_, err = decompressInt16(c, NavTileSize*NavTileSize)
	test.That(t, err, test.ShouldNotBeNil)

	_, err = decompressBytes([]byte("not zlib"), 10)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestNavTileIDIsVersioned(t *testing.T) {
	// The version is in the id so a meaning change rebuilds rather than
	// silently trusting stale tiles, and a rollback finds its own.
	id := NavTileID(12, 1234, 1543)
	test.That(t, id, test.ShouldContainSubstring, "12/1234/1543")
	test.That(t, id, test.ShouldStartWith, "v")
	test.That(t, NavTileID(12, 1234, 1543), test.ShouldEqual, id)
	test.That(t, NavTileID(12, 1234, 1544), test.ShouldNotEqual, id)
}
