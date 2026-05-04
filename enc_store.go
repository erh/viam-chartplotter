package vc

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
)

// CellManifestEntry records what we have on disk for a given ENC cell so we know
// whether NOAA's catalog has a newer edition or update to pull.
type CellManifestEntry struct {
	Name         string    `json:"name"`
	Edition      int       `json:"edition"`
	Update       int       `json:"update"`
	DownloadedAt time.Time `json:"downloaded_at"`
	SourceSize   int64     `json:"source_size"`
}

// ENCStore manages on-disk copies of NOAA ENC cells: download, extract, version
// tracking, and exposing where the .000 file lives for downstream renderers.
type ENCStore struct {
	rootDir string
	catalog *ENCCatalog
	client  *http.Client
	logger  logging.Logger

	mu       sync.Mutex
	manifest map[string]CellManifestEntry

	// cellLocks serialises downloadAndExtract calls per cell so two concurrent
	// SyncBBox requests that both want the same cell don't race on RemoveAll →
	// MkdirAll → file extraction. Without this, worker A's RemoveAll(target)
	// can run between worker B's MkdirAll and B's first extractFlat, and B
	// then fails with "no such file or directory" on a dir that did exist
	// moments earlier.
	cellLocks sync.Map // map[string]*sync.Mutex
}

func NewENCStore(rootDir string, catalog *ENCCatalog, logger logging.Logger) (*ENCStore, error) {
	if err := os.MkdirAll(filepath.Join(rootDir, "cells"), 0o755); err != nil {
		return nil, fmt.Errorf("enc store: mkdir %q: %w", rootDir, err)
	}
	s := &ENCStore{
		rootDir:  rootDir,
		catalog:  catalog,
		client:   &http.Client{Timeout: 5 * time.Minute},
		logger:   logger,
		manifest: map[string]CellManifestEntry{},
	}
	s.loadManifest()
	return s, nil
}

// lockCell returns a per-cell mutex (lazily created). Hold it for the duration
// of any download / extract / manifest update for that cell.
func (s *ENCStore) lockCell(name string) *sync.Mutex {
	v, _ := s.cellLocks.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *ENCStore) manifestPath() string { return filepath.Join(s.rootDir, "cells.json") }

func (s *ENCStore) cellDir(name string) string { return filepath.Join(s.rootDir, "cells", name) }

// S57Path returns the path to the cell's primary S-57 file (.000) on disk, or "" if
// the cell isn't downloaded yet.
func (s *ENCStore) S57Path(name string) string {
	p := filepath.Join(s.cellDir(name), name+".000")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func (s *ENCStore) loadManifest() {
	data, err := os.ReadFile(s.manifestPath())
	if err != nil {
		return
	}
	var m map[string]CellManifestEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	s.mu.Lock()
	s.manifest = m
	s.mu.Unlock()
}

func (s *ENCStore) saveManifestLocked() error {
	data, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.manifestPath())
}

// needsDownload returns true if we don't have this cell, or NOAA has a newer
// edition/update than what's recorded locally.
func (s *ENCStore) needsDownload(c ENCCell) bool {
	s.mu.Lock()
	entry, ok := s.manifest[c.Name]
	s.mu.Unlock()
	if !ok {
		return true
	}
	if c.Edition > entry.Edition {
		return true
	}
	if c.Edition == entry.Edition && c.Update > entry.Update {
		return true
	}
	return s.S57Path(c.Name) == ""
}

// SyncBBox ensures every active ENC cell whose coverage overlaps the given lon/lat
// box is present at the latest edition+update on disk. Cells already up to date are
// skipped. Concurrency is bounded by parallel (default 4 if <= 0).
func (s *ENCStore) SyncBBox(
	ctx context.Context,
	minLon, minLat, maxLon, maxLat float64,
	minScale, maxScale, parallel int,
) (downloaded, skipped int, err error) {
	start := time.Now()
	if err := s.catalog.EnsureFresh(ctx); err != nil {
		s.logger.Warnf("enc catalog refresh failed: %v", err)
	}
	cells := s.catalog.CellsForBBox(minLon, minLat, maxLon, maxLat, minScale, maxScale)
	if parallel <= 0 {
		parallel = 4
	}

	// Count needed up-front so the start log is informative even before any
	// individual cell fetches finish.
	var toDownload []ENCCell
	for _, c := range cells {
		if s.needsDownload(c) {
			toDownload = append(toDownload, c)
		}
	}
	skipped = len(cells) - len(toDownload)

	s.logger.Infof("enc sync bbox=[%.4f,%.4f,%.4f,%.4f]: %d cells overlap, %d already current, %d to download (parallel=%d)",
		minLon, minLat, maxLon, maxLat,
		len(cells), skipped, len(toDownload), parallel)

	if len(toDownload) == 0 {
		s.logger.Infof("enc sync bbox=[%.4f,%.4f,%.4f,%.4f]: nothing to fetch (took %s)",
			minLon, minLat, maxLon, maxLat, time.Since(start).Round(time.Millisecond))
		return 0, skipped, nil
	}

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed int

	for _, c := range toDownload {
		c := c
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if dlErr := s.downloadAndExtract(ctx, c); dlErr != nil {
				s.logger.Warnf("enc cell %s: %v", c.Name, dlErr)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			downloaded++
			mu.Unlock()
		}()
	}
	wg.Wait()

	s.logger.Infof("enc sync bbox=[%.4f,%.4f,%.4f,%.4f]: done — %d downloaded, %d skipped, %d failed (%s)",
		minLon, minLat, maxLon, maxLat,
		downloaded, skipped, failed, time.Since(start).Round(time.Millisecond))
	return downloaded, skipped, nil
}

func (s *ENCStore) downloadAndExtract(ctx context.Context, c ENCCell) error {
	// Serialise per-cell so concurrent SyncBBox callers don't race on
	// RemoveAll/MkdirAll for the same target directory.
	cellLock := s.lockCell(c.Name)
	cellLock.Lock()
	defer cellLock.Unlock()

	// Re-check after taking the lock — another goroutine may have just pulled
	// the same cell while we were waiting.
	if !s.needsDownload(c) {
		return nil
	}

	url := c.ZipFile
	if !strings.HasPrefix(url, "http") {
		url = encDownloadBase + strings.TrimPrefix(url, "ENCs/")
	}
	start := time.Now()
	s.logger.Infof("enc cell %s: downloading edition %d update %d (%s)", c.Name, c.Edition, c.Update, url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}

	tmpZip, err := os.CreateTemp(s.rootDir, "cell-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmpZip.Name())
	n, err := io.Copy(tmpZip, resp.Body)
	if err != nil {
		tmpZip.Close()
		return err
	}
	if err := tmpZip.Close(); err != nil {
		return err
	}

	target := s.cellDir(c.Name)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}

	zr, err := zip.OpenReader(tmpZip.Name())
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !shouldExtract(f.Name) {
			continue
		}
		if err := extractFlat(f, target); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.manifest[c.Name] = CellManifestEntry{
		Name:         c.Name,
		Edition:      c.Edition,
		Update:       c.Update,
		DownloadedAt: time.Now(),
		SourceSize:   n,
	}
	saveErr := s.saveManifestLocked()
	s.mu.Unlock()

	s.logger.Infof("enc cell %s: ok (edition %d update %d, %.1f KB, %s)",
		c.Name, c.Edition, c.Update, float64(n)/1024, time.Since(start).Round(time.Millisecond))
	return saveErr
}

// shouldExtract keeps the .000 cell, .NNN update files (.001 ..), and any .txt
// metadata. Everything else (signatures, indexes, unrelated subdirs) is skipped.
func shouldExtract(name string) bool {
	base := filepath.Base(name)
	lower := strings.ToLower(base)
	if len(lower) >= 4 && lower[len(lower)-4] == '.' {
		ext := lower[len(lower)-3:]
		allDigits := true
		for _, r := range ext {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return strings.HasSuffix(lower, ".txt")
}

// extractFlat writes the zip entry into target/<basename>, ignoring any directory
// structure inside the zip. This sidesteps zip-slip and matches NOAA's flat layout
// inside each cell archive.
func extractFlat(f *zip.File, target string) error {
	name := filepath.Base(f.Name)
	if name == "" || name == "." || name == string(os.PathSeparator) {
		return nil
	}
	out, err := os.Create(filepath.Join(target, name))
	if err != nil {
		return err
	}
	defer out.Close()
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(out, rc)
	return err
}

// Stats returns the number of cells we have on disk and the total bytes in the
// store directory.
func (s *ENCStore) Stats() (cells int, bytes int64) {
	s.mu.Lock()
	cells = len(s.manifest)
	s.mu.Unlock()
	_ = filepath.Walk(s.rootDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		bytes += info.Size()
		return nil
	})
	return cells, bytes
}

// RootDir returns the directory backing this store.
func (s *ENCStore) RootDir() string { return s.rootDir }
