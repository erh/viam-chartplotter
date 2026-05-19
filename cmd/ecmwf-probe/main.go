// ecmwf-probe is a diagnostic CLI for the in-tree CCSDS/AEC decoder.
// It downloads a real ECMWF Open Data wind message via the .index
// sidecar and runs it through the GRIB walker + CCSDS unpacker with
// block-by-block tracing, so we can iterate on spec-interpretation
// bugs against captured wire data.
//
// Usage:
//
//	go run ./cmd/ecmwf-probe                  # most recent default cycle, step 0, 10u
//	go run ./cmd/ecmwf-probe -date 20260518 -cycle 0 -step 6 -param 10v
//	go run ./cmd/ecmwf-probe -file /path/to/captured.grib2
//
// The -file form skips the HTTP fetch and parses a local file —
// useful when running offline (the sandbox CI environment can't
// reach data.ecmwf.int but a developer can drop a local file in
// place after fetching it elsewhere).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	vc "github.com/erh/viam-chartplotter"
)

const ecmwfBaseURL = "https://data.ecmwf.int/forecasts/%s/%02dz/ifs/0p25/oper/%s%02d0000-%dh-oper-fc"

type indexEntry struct {
	Param   string `json:"param"`
	LevType string `json:"levtype"`
	Step    string `json:"step"`
	Offset  int64  `json:"_offset"`
	Length  int64  `json:"_length"`
}

func main() {
	var (
		date  = flag.String("date", "", "cycle date YYYYMMDD (default: today UTC)")
		cycle = flag.Int("cycle", 0, "cycle hour (0/6/12/18)")
		step  = flag.Int("step", 0, "forecast step in hours")
		param = flag.String("param", "10u", "parameter name (10u/10v/2t/...)")
		file  = flag.String("file", "", "local .grib2 file to parse (skips HTTP fetch)")
		quiet = flag.Bool("quiet", false, "suppress per-block AEC trace lines")
	)
	flag.Parse()

	if !*quiet {
		vc.AECDebug = log.New(os.Stderr, "", 0)
	}

	var grib []byte
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			log.Fatalf("read %s: %v", *file, err)
		}
		grib = b
		log.Printf("loaded local file %s (%d bytes)", *file, len(grib))
	} else {
		if *date == "" {
			// ECMWF Open Data publishes a few hours after the cycle;
			// fall back to "yesterday at this cycle" to dodge the
			// publish-lag race for the most recent run.
			t := time.Now().UTC().Add(-24 * time.Hour)
			*date = t.Format("20060102")
		}
		b, err := fetchECMWFMessage(*date, *cycle, *step, *param)
		if err != nil {
			log.Fatalf("fetch: %v", err)
		}
		grib = b
		log.Printf("fetched %s %02dz step=%d %s (%d bytes)", *date, *cycle, *step, *param, len(grib))
	}

	if len(grib) < 4 || string(grib[:4]) != "GRIB" {
		log.Fatalf("not a GRIB2 stream (first 4 bytes: %q)", string(grib[:min(4, len(grib))]))
	}

	// DebugDumpGRIB is the in-package helper that walks each
	// message, prints per-section diagnostics, and invokes the
	// CCSDS decoder. With vc.AECDebug wired to stderr above, we
	// also get one line per block decoded so it's easy to bisect a
	// failure to a specific block id / position.
	if err := vc.DebugDumpGRIB(grib, os.Stdout); err != nil {
		log.Fatalf("dump: %v", err)
	}
}

func fetchECMWFMessage(date string, cycle, step int, param string) ([]byte, error) {
	base := fmt.Sprintf(ecmwfBaseURL, date, cycle, date, cycle, step)
	indexURL := base + ".index"
	gribURL := base + ".grib2"

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(indexURL)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("index %s: HTTP %d", indexURL, resp.StatusCode)
	}
	idxBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("index read: %w", err)
	}

	var hit indexEntry
	var ok bool
	sc := bufio.NewScanner(bytes.NewReader(idxBody))
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e indexEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.LevType == "sfc" && e.Param == param {
			hit = e
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("no surface entry for param=%q in index", param)
	}

	log.Printf("index hit: param=%s offset=%d length=%d", hit.Param, hit.Offset, hit.Length)

	req, err := http.NewRequest("GET", gribURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", hit.Offset, hit.Offset+hit.Length-1))
	resp2, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("range get: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK && resp2.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("range get %s: HTTP %d", gribURL, resp2.StatusCode)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode == http.StatusOK && int64(len(body)) > hit.Length {
		body = body[hit.Offset : hit.Offset+hit.Length]
	}
	return body, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
