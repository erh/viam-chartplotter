package vc

import (
	"context"
	"encoding/xml"
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

const (
	encProdCatURL   = "https://charts.noaa.gov/ENCs/ENCProdCat.xml"
	encDownloadBase = "https://charts.noaa.gov/ENCs/"

	// NOAA publishes new editions weekly. Refresh anything older than this.
	catalogTTL = 7 * 24 * time.Hour
)

// ENCCell describes one S-57 cell from NOAA's product catalog. Only the fields we
// actually consume are unmarshaled; unknown XML elements are ignored. Coverage is
// reduced to a simple bbox after parsing.
type ENCCell struct {
	Name      string      `xml:"name"`
	LName     string      `xml:"lname"`
	CScale    int         `xml:"cscale"`
	Status    string      `xml:"status"`
	Edition   int         `xml:"edtn"`
	Update    int         `xml:"updn"`
	IssueDate string      `xml:"isdt"`
	ZipFile   string      `xml:"zipfile_location"`
	ZipSize   int64       `xml:"zipfile_size"`
	Coverage  encCoverage `xml:"cov"`

	MinLon float64 `xml:"-"`
	MinLat float64 `xml:"-"`
	MaxLon float64 `xml:"-"`
	MaxLat float64 `xml:"-"`
}

type encCoverage struct {
	Panels []encPanel `xml:"panel"`
}

type encPanel struct {
	Vertices []encVertex `xml:"vertex"`
}

type encVertex struct {
	Lat float64 `xml:"lat"`
	Lon float64 `xml:"long"`
}

type encProductCatalog struct {
	XMLName xml.Name  `xml:"EncProductCatalog"`
	Cells   []ENCCell `xml:"cell"`
}

func (c *ENCCell) computeBBox() {
	c.MinLon, c.MinLat = 180, 90
	c.MaxLon, c.MaxLat = -180, -90
	for _, p := range c.Coverage.Panels {
		for _, v := range p.Vertices {
			if v.Lon < c.MinLon {
				c.MinLon = v.Lon
			}
			if v.Lon > c.MaxLon {
				c.MaxLon = v.Lon
			}
			if v.Lat < c.MinLat {
				c.MinLat = v.Lat
			}
			if v.Lat > c.MaxLat {
				c.MaxLat = v.Lat
			}
		}
	}
}

func (c *ENCCell) hasCoverage() bool { return c.MaxLon >= c.MinLon && c.MaxLat >= c.MinLat }

// Overlaps returns true if the cell's coverage bbox intersects the given lon/lat box.
func (c *ENCCell) Overlaps(minLon, minLat, maxLon, maxLat float64) bool {
	return !(c.MaxLon < minLon || c.MinLon > maxLon || c.MaxLat < minLat || c.MinLat > maxLat)
}

// ENCCatalog manages NOAA's ENC product catalog: fetching, on-disk persistence, and
// fast bbox-overlap queries against the parsed cell list.
type ENCCatalog struct {
	cacheDir string
	client   *http.Client
	logger   logging.Logger

	mu      sync.RWMutex
	cells   []ENCCell
	fetched time.Time
}

func NewENCCatalog(cacheDir string, logger logging.Logger) (*ENCCatalog, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("enc catalog: mkdir %q: %w", cacheDir, err)
	}
	cat := &ENCCatalog{
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 60 * time.Second},
		logger:   logger,
	}
	// Best-effort: load any catalog already on disk so we have something to query
	// before the first refresh completes.
	_ = cat.loadFromDisk()
	return cat, nil
}

func (c *ENCCatalog) catalogPath() string {
	return filepath.Join(c.cacheDir, "ENCProdCat.xml")
}

func (c *ENCCatalog) loadFromDisk() error {
	info, err := os.Stat(c.catalogPath())
	if err != nil {
		return err
	}
	data, err := os.ReadFile(c.catalogPath())
	if err != nil {
		return err
	}
	cells, err := parseCatalog(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cells = cells
	c.fetched = info.ModTime()
	c.mu.Unlock()
	return nil
}

// EnsureFresh refetches the catalog if the local copy is missing or older than
// catalogTTL. Always safe to call repeatedly.
func (c *ENCCatalog) EnsureFresh(ctx context.Context) error {
	c.mu.RLock()
	fresh := !c.fetched.IsZero() && time.Since(c.fetched) < catalogTTL
	c.mu.RUnlock()
	if fresh {
		return nil
	}
	return c.refresh(ctx)
}

func (c *ENCCatalog) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, encProdCatURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("catalog upstream %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	cells, err := parseCatalog(body)
	if err != nil {
		return fmt.Errorf("catalog parse: %w", err)
	}
	tmp := c.catalogPath() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.catalogPath()); err != nil {
		return err
	}

	c.mu.Lock()
	c.cells = cells
	c.fetched = time.Now()
	c.mu.Unlock()
	c.logger.Infof("noaa enc catalog: %d cells", len(cells))
	return nil
}

func parseCatalog(data []byte) ([]ENCCell, error) {
	var cat encProductCatalog
	if err := xml.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	out := make([]ENCCell, 0, len(cat.Cells))
	for _, cell := range cat.Cells {
		if cell.Status != "" && !strings.EqualFold(cell.Status, "Active") {
			continue
		}
		cell.computeBBox()
		if !cell.hasCoverage() {
			continue
		}
		out = append(out, cell)
	}
	return out, nil
}

// CellsForBBox returns active cells whose coverage overlaps the given lon/lat box,
// optionally filtered to those whose CScale is in [minScale, maxScale]. Pass 0 to
// disable a scale bound.
func (c *ENCCatalog) CellsForBBox(minLon, minLat, maxLon, maxLat float64, minScale, maxScale int) []ENCCell {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []ENCCell
	for _, cell := range c.cells {
		if !cell.Overlaps(minLon, minLat, maxLon, maxLat) {
			continue
		}
		if minScale > 0 && cell.CScale < minScale {
			continue
		}
		if maxScale > 0 && cell.CScale > maxScale {
			continue
		}
		out = append(out, cell)
	}
	return out
}

// CellCount returns the number of active cells in the parsed catalog.
func (c *ENCCatalog) CellCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cells)
}

// FetchedAt returns when the catalog was last fetched (zero if never).
func (c *ENCCatalog) FetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetched
}
