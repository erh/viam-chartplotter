// wind-publisher is the standalone CLI that builds a tiled publish of
// an ECMWF Open Data cycle: fetch + decode + crop into the fixed tile
// grid, gzip each tile, and write the manifest. It's the cron-driven
// in-module wind-publisher Viam resource's --dry-run twin: same
// pipeline, output goes to a local directory instead of R2 so we can
// inspect / diff / vendor a known-good publish into testdata when
// the wire format changes.
//
// Usage:
//
//	# Dry-run: build a cycle to local disk only.
//	wind-publisher publish --out /tmp/publish ecmwf
//
//	# Upload to R2 (credentials via env: R2_ACCOUNT_ID, R2_ACCESS_KEY_ID,
//	# R2_SECRET_ACCESS_KEY, R2_BUCKET).
//	wind-publisher publish --r2 ecmwf
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	vc "github.com/erh/viam-chartplotter"
)

func main() {
	cmd := "publish"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "publish":
		os.Args = os.Args[1:]
		runPublish()
	case "tiles":
		os.Args = os.Args[1:]
		runTiles()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (try: publish | tiles)\n", cmd)
		os.Exit(2)
	}
}

func runPublish() {
	var (
		out      = flag.String("out", "./publish-out", "output directory for dry-run publish")
		r2Flag   = flag.Bool("r2", false, "upload to Cloudflare R2 (requires R2_* env vars)")
		timeout  = flag.Duration("timeout", 30*time.Minute, "overall timeout for the cycle build")
		cacheDir = flag.String("cache-dir", "", "raw-GRIB cache directory (default: $WIND_PUBLISHER_CACHE_DIR or ~/Library/Caches/viam-chartplotter-wind-publisher/raw-ecmwf)")
		noCache  = flag.Bool("no-cache", false, "force re-fetch from ECMWF even when cached bytes are present (debugging only)")
	)
	flag.Parse()

	// Wire the project-wide raw-bytes cache so a crash mid-publish
	// doesn't cost us a full ECMWF re-fetch on the next run. ECMWF
	// data is immutable per (cycle, fh), so the cache never needs
	// invalidation. --no-cache disables this for debugging.
	if !*noCache {
		dir := *cacheDir
		if dir == "" {
			dir = os.Getenv("WIND_PUBLISHER_CACHE_DIR")
		}
		if dir == "" {
			base, err := os.UserCacheDir()
			if err != nil {
				base = os.TempDir()
			}
			dir = filepath.Join(base, "viam-chartplotter-wind-publisher", "raw-ecmwf")
		}
		if err := vc.SetECMWFRawCacheDir(dir); err != nil {
			log.Fatalf("cache dir: %v", err)
		}
		log.Printf("raw-grib cache: %s", dir)
	} else {
		log.Printf("raw-grib cache: DISABLED (--no-cache)")
	}
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "missing model name (e.g. 'ecmwf')")
		os.Exit(2)
	}
	modelName := args[0]
	m := vc.FindWeatherModelForPublish(modelName)
	if m == nil {
		fmt.Fprintf(os.Stderr, "unknown weather model: %s\n", modelName)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := &http.Client{Timeout: 120 * time.Second}
	t0 := time.Now()
	cycle, err := vc.BuildECMWFCycle(ctx, client, m)
	if err != nil {
		log.Fatalf("build cycle: %v", err)
	}
	log.Printf("built cycle %s for model=%s in %s (%d fhs × %d tiles = %d blobs)",
		cycle.CycleTime.Format("20060102T15"), cycle.Model, time.Since(t0).Round(time.Second),
		len(cycle.FHs), len(cycle.Tiles), len(cycle.FHs)*len(cycle.Tiles))

	if *r2Flag {
		up, err := vc.NewR2UploaderFromEnv()
		if err != nil {
			log.Fatalf("r2: %v", err)
		}
		if err := up.UploadCycle(ctx, cycle); err != nil {
			log.Fatalf("r2 upload: %v", err)
		}
		log.Printf("r2 publish ok")
		return
	}

	if err := writeCycleToDisk(*out, cycle); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote dry-run cycle to %s", *out)
}

// writeCycleToDisk mirrors the R2 path layout under a local directory
// so a dry-run produces the same tree we'd otherwise push to R2 — same
// keys, same content. Letting the user diff two runs (e.g. "did
// tweaking the crop bbox change anything?") without standing up R2.
func writeCycleToDisk(root string, cycle *vc.PublishedCycle) error {
	cycleStr := cycle.CycleTime.UTC().Format("20060102T15")
	cycleDir := filepath.Join(root, "wind", cycle.Model, cycleStr)
	if err := os.MkdirAll(cycleDir, 0o755); err != nil {
		return err
	}
	totalBytes := 0
	for _, fh := range cycle.FHs {
		fhDir := filepath.Join(cycleDir, fmt.Sprintf("f%03d", fh))
		if err := os.MkdirAll(fhDir, 0o755); err != nil {
			return err
		}
		for _, tile := range cycle.Tiles {
			blob, ok := cycle.TileBlobFor(fh, tile.Key)
			if !ok {
				return fmt.Errorf("missing blob fh=%d tile=%s", fh, tile.Key)
			}
			path := filepath.Join(fhDir, tile.Key+".json.gz")
			if err := os.WriteFile(path, blob.GzippedJSON, 0o644); err != nil {
				return err
			}
			totalBytes += len(blob.GzippedJSON)
		}
	}
	// Manifest + latest pointer.
	manifestPath := filepath.Join(root, "wind", cycle.Model, "manifest", cycleStr+".json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return err
	}
	if err := vc.WriteManifest(manifestPath, cycle); err != nil {
		return err
	}
	latestPath := filepath.Join(root, "wind", cycle.Model, "latest.json")
	if err := vc.WriteManifest(latestPath, cycle); err != nil {
		return err
	}
	log.Printf("dry-run: wrote %d tile blobs (%.1f MB total gzipped)",
		len(cycle.FHs)*len(cycle.Tiles), float64(totalBytes)/(1024*1024))
	return nil
}

func runTiles() {
	tiles := vc.AllTiles()
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].Row != tiles[j].Row {
			return tiles[i].Row < tiles[j].Row
		}
		return tiles[i].Col < tiles[j].Col
	})
	for _, t := range tiles {
		fmt.Printf("%-22s  nominal=%v  published=%v\n", t.Key, t.NominalBbox, t.PublishedBbox)
	}
}
