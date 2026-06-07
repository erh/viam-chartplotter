// osm-ingest streams an .osm.pbf and reports how each feature would
// classify under the self-hosted-tiles filter (see osmtiler package).
// It does not yet write an index — this is the v0.1 ingest skeleton,
// used to validate the keep/drop tag rules end-to-end against real
// data before we commit to the on-disk feature format.
//
// Usage:
//
//	go run ./cmd/osm-ingest -in monaco-latest.osm.pbf
//	go run ./cmd/osm-ingest -in us-northeast-latest.osm.pbf -procs 8
//
// Tip: grab a small Geofabrik extract (Monaco ~500KB, Delaware ~80MB,
// us-northeast ~1.5GB) to iterate without waiting on planet-scale runs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"

	"github.com/erh/viam-chartplotter/mapdata/osmtiler"
)

func main() {
	in := flag.String("in", "", "path to input .osm.pbf (required)")
	procs := flag.Int("procs", runtime.NumCPU(), "decoder workers")
	flag.Parse()

	if *in == "" {
		flag.Usage()
		os.Exit(2)
	}

	f, err := os.Open(*in)
	if err != nil {
		log.Fatalf("open %s: %v", *in, err)
	}
	defer f.Close()

	start := time.Now()

	var (
		totalNodes, totalWays, totalRels int64
		droppedNoTags                    int64
		droppedSkipWithTags              int64
		keptByClass                      [osmtiler.ClassCount]int64
	)

	sc := osmpbf.New(context.Background(), f, *procs)
	defer sc.Close()

	for sc.Scan() {
		var tags osm.Tags
		switch e := sc.Object().(type) {
		case *osm.Node:
			totalNodes++
			tags = e.Tags
		case *osm.Way:
			totalWays++
			tags = e.Tags
		case *osm.Relation:
			totalRels++
			tags = e.Tags
		default:
			continue
		}

		if len(tags) == 0 {
			droppedNoTags++
			continue
		}
		class := osmtiler.Classify(tags)
		if class == osmtiler.ClassSkip {
			droppedSkipWithTags++
			continue
		}
		keptByClass[class]++
	}
	if err := sc.Err(); err != nil {
		log.Fatalf("scanner: %v", err)
	}

	elapsed := time.Since(start)
	totalKept := int64(0)
	for c := osmtiler.Class(1); c < osmtiler.ClassCount; c++ {
		totalKept += keptByClass[c]
	}

	fmt.Printf("input:        %s\n", *in)
	fmt.Printf("elapsed:      %s\n", elapsed.Round(time.Millisecond))
	fmt.Println()
	fmt.Println("element totals")
	fmt.Printf("  nodes:                %d\n", totalNodes)
	fmt.Printf("  ways:                 %d\n", totalWays)
	fmt.Printf("  relations:            %d\n", totalRels)
	fmt.Println()
	fmt.Println("classifier outcome")
	fmt.Printf("  dropped (no tags):    %d\n", droppedNoTags)
	fmt.Printf("  dropped (water/skip): %d\n", droppedSkipWithTags)
	fmt.Printf("  kept:                 %d\n", totalKept)
	fmt.Println()
	fmt.Println("kept by class")
	for c := osmtiler.Class(1); c < osmtiler.ClassCount; c++ {
		fmt.Printf("  %-10s %d\n", c, keptByClass[c])
	}
}
