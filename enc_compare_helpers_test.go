package vc

import (
	"math"
	"os"
	"strconv"
	"testing"
)

// Test helpers local to the WMS-compare integration test. The render package
// has its own copies of these (the dump/regression tests moved there with the
// renderer); compare_test stays in package vc because it needs the WMS cache
// (*NoaaCache), so it carries its own small copies here.

const feetPerMetre = 3.28084

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrFloat(t *testing.T, k string, def float64) float64 {
	raw := os.Getenv(k)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s=%q: not a float: %v", k, raw, err)
	}
	return v
}

func mustUserCacheDir(t *testing.T) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("user cache dir: %v", err)
	}
	return dir
}

// mercToLonLat converts Web-Mercator metres to lon/lat degrees.
func mercToLonLat(x, y float64) (lon, lat float64) {
	lon = x / mercatorMax * 180.0
	lat = math.Atan(math.Sinh(y/mercatorMax*math.Pi)) * 180.0 / math.Pi
	return
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
