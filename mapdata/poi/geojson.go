package poi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Reading a feed.
//
// Everything here speaks GeoJSON, because every one of these publishers serves
// it: the NOAA and state feeds are ArcGIS services with `f=geojson`, and the
// bulk downloads convert to it in one ogr2ogr. Shapefile parsing would be a
// dependency and a decoder to maintain for no coverage we don't already have.

// Feature is one GeoJSON feature, decoded only as far as we need: a geometry
// we reduce to a point and a free-form property bag the adapters read.
type Feature struct {
	Type       string          `json:"type"`
	Geometry   *Geometry       `json:"geometry"`
	Properties map[string]any  `json:"properties"`
	ID         json.RawMessage `json:"id"`
}

// Geometry is a GeoJSON geometry with its coordinates left as raw JSON —
// Point, LineString, Polygon and the Multi variants all nest differently, and
// RepresentativePoint walks whichever shape arrived rather than modelling four.
type Geometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

// FeatureCollection is the top level of a GeoJSON document.
type FeatureCollection struct {
	Type     string    `json:"type"`
	Features []Feature `json:"features"`
	// ExceededTransferLimit is ArcGIS's flag for "there are more records than
	// this response holds". It is how paging knows to keep going even when a
	// server caps a page below the requested size.
	ExceededTransferLimit bool `json:"exceededTransferLimit"`
	// Error is ArcGIS's in-band error object: a failed query comes back as
	// HTTP 200 with this set and no features, which without a check reads as
	// "the dataset is empty" and quietly ingests nothing.
	Error *struct {
		Code    int      `json:"code"`
		Message string   `json:"message"`
		Details []string `json:"details"`
	} `json:"error"`
}

// RepresentativePoint reduces any geometry to the single lon/lat a symbol
// draws at: the coordinate for a point, the centre of the extent for anything
// else. Reefs and obstruction areas arrive as polygons; a fisherman wants the
// mark, not the permit boundary.
func (g *Geometry) RepresentativePoint() (lng, lat float64, ok bool) {
	if g == nil || len(g.Coordinates) == 0 {
		return 0, 0, false
	}
	var (
		minLon, minLat = 181.0, 91.0
		maxLon, maxLat = -181.0, -91.0
		found          bool
	)
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			// A coordinate pair is a list whose first two entries are numbers;
			// anything else is a level of nesting to descend through.
			if len(t) >= 2 {
				x, xok := t[0].(float64)
				y, yok := t[1].(float64)
				if xok && yok {
					found = true
					minLon, maxLon = min(minLon, x), max(maxLon, x)
					minLat, maxLat = min(minLat, y), max(maxLat, y)
					return
				}
			}
			for _, e := range t {
				walk(e)
			}
		}
	}
	var raw any
	if err := json.Unmarshal(g.Coordinates, &raw); err != nil {
		return 0, 0, false
	}
	walk(raw)
	if !found {
		return 0, 0, false
	}
	return (minLon + maxLon) / 2, (minLat + maxLat) / 2, true
}

// ReadFile decodes a GeoJSON FeatureCollection from disk.
func ReadFile(path string) ([]Feature, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeFeatures(f)
}

// DecodeFeatures decodes a FeatureCollection from a reader.
func DecodeFeatures(r io.Reader) ([]Feature, error) {
	var fc FeatureCollection
	if err := json.NewDecoder(r).Decode(&fc); err != nil {
		return nil, fmt.Errorf("poi: decode geojson: %w", err)
	}
	if fc.Error != nil {
		return nil, fmt.Errorf("poi: upstream error %d: %s", fc.Error.Code, fc.Error.Message)
	}
	return fc.Features, nil
}

// FetchOptions configures a paged ArcGIS fetch.
type FetchOptions struct {
	// PageSize is how many records to ask for per request. Servers cap this
	// themselves (2000 is a common ceiling); asking for more is harmless.
	PageSize int
	// BBox, when set, restricts the query to [minLon, minLat, maxLon, maxLat].
	BBox *[4]float64
	// Client is the HTTP client to use; nil gets one with a sane timeout.
	Client *http.Client
	// Progress, when set, is called after each page with the running total.
	Progress func(fetched int)
}

const defaultPageSize = 1000

// FetchArcGIS pulls every record of an ArcGIS feature layer as GeoJSON,
// paging until the server stops handing back new features.
//
// layerURL is the layer endpoint, e.g.
// ".../rest/services/public_wrecks/Wrecks_And_Obstructions/MapServer/0" —
// with or without a trailing "/query", which is appended when absent.
//
// Paging is by resultOffset, and the loop stops on three conditions rather
// than one: an empty page, a page that returns no NEW ids, or a server that
// ignores resultOffset entirely (some older MapServer deployments do, and
// without the id check that is an infinite loop appending the same page).
func FetchArcGIS(ctx context.Context, layerURL string, opts FetchOptions) ([]Feature, error) {
	if opts.PageSize <= 0 {
		opts.PageSize = defaultPageSize
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	base := strings.TrimSuffix(layerURL, "/")
	if !strings.HasSuffix(base, "/query") {
		base += "/query"
	}

	var (
		out  []Feature
		seen = map[string]bool{}
	)
	for offset := 0; ; offset += opts.PageSize {
		q := url.Values{}
		q.Set("f", "geojson")
		q.Set("outFields", "*")
		q.Set("outSR", "4326")
		q.Set("returnGeometry", "true")
		q.Set("resultOffset", strconv.Itoa(offset))
		q.Set("resultRecordCount", strconv.Itoa(opts.PageSize))
		if opts.BBox != nil {
			b := *opts.BBox
			q.Set("geometryType", "esriGeometryEnvelope")
			q.Set("inSR", "4326")
			q.Set("spatialRel", "esriSpatialRelIntersects")
			q.Set("geometry", fmt.Sprintf("%g,%g,%g,%g", b[0], b[1], b[2], b[3]))
		} else {
			q.Set("where", "1=1")
		}
		page, err := fetchPage(ctx, client, base+"?"+q.Encode())
		if err != nil {
			return out, err
		}
		if len(page) == 0 {
			return out, nil
		}
		fresh := 0
		for _, f := range page {
			k := featureKey(f)
			if k != "" && seen[k] {
				continue
			}
			if k != "" {
				seen[k] = true
			}
			fresh++
			out = append(out, f)
		}
		if opts.Progress != nil {
			opts.Progress(len(out))
		}
		// A page of nothing new means the server is ignoring resultOffset;
		// stop rather than loop forever on page one.
		if fresh == 0 {
			return out, nil
		}
	}
}

func fetchPage(ctx context.Context, client *http.Client, u string) ([]Feature, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poi: fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("poi: fetch %s: http %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return DecodeFeatures(resp.Body)
}

// featureKey is the identity used to detect a repeated page: the feed's own id
// when it has one, else the whole property bag rendered stably enough to
// compare. Empty means "no usable identity" and the feature is always kept.
func featureKey(f Feature) string {
	if len(f.ID) > 0 && string(f.ID) != "null" {
		return string(f.ID)
	}
	for _, k := range []string{"OBJECTID", "objectid", "FID", "fid"} {
		if v, ok := f.Properties[k]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}
