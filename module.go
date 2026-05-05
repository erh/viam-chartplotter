package vc

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/erh/vmodutils"
)

//go:embed dist
var staticFS embed.FS

func DistFS() (fs.FS, error) {
	return fs.Sub(staticFS, "dist")
}

var Model = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("chartplotter")

func init() {
	resource.RegisterComponent(
		generic.API,
		Model,
		resource.Registration[resource.Resource, resource.NoNativeConfig]{
			Constructor: newServer,
		})
}

func newServer(ctx context.Context, deps resource.Dependencies, config resource.Config, logger logging.Logger) (resource.Resource, error) {
	dist, err := DistFS()
	if err != nil {
		return nil, err
	}
	port := config.Attributes.Int("port", 8888)
	cacheDir := config.Attributes.String("noaa_cache_dir")
	cacheMaxBytes := int64(config.Attributes.Int("noaa_cache_max_bytes", 0))
	safeDepthFt := config.Attributes.Float64("safe_depth_ft", 6)
	myBoatIcon := config.Attributes.String("myboat_icon_path")
	return StartChartplotterServer(config.ResourceName(), dist, logger, port, cacheDir, cacheMaxBytes, safeDepthFt, myBoatIcon)
}

// resolveCacheRoot picks the parent directory under which both the WMS proxy cache
// (noaa-wms/) and the ENC store (noaa-enc/) live. An explicit path wins; otherwise
// we use the OS user cache dir, falling back to the temp dir if HOME is unset.
func resolveCacheRoot(configured string) string {
	if configured != "" {
		return configured
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "viam-chartplotter")
}

// StartChartplotterServer wires the static frontend, the NOAA WMS caching proxy, and
// the ENC catalog/store handlers, and starts an HTTP server on the given port.
// safeDepthFt is the default safety contour (feet) for tile rendering; the
// per-request `?sd=` query param overrides it.
func StartChartplotterServer(
	name resource.Name,
	dist fs.FS,
	logger logging.Logger,
	port int,
	cacheRoot string,
	cacheMaxBytes int64,
	safeDepthFt float64,
	myBoatIconPath string,
) (resource.Resource, error) {
	mux, server, err := vmodutils.PrepInModuleServer(dist, logger.Sublogger("accessLog"))
	if err != nil {
		return nil, err
	}

	root := resolveCacheRoot(cacheRoot)

	wmsCache, err := NewNoaaCache(filepath.Join(root, "noaa-wms"), cacheMaxBytes, logger.Sublogger("noaaCache"))
	if err != nil {
		return nil, err
	}
	wmsCache.Register(mux)
	logger.Infof("noaa wms cache: %s (max %d bytes, stale after %s)",
		wmsCache.cacheDir, wmsCache.maxBytes, wmsCache.staleAfter)

	encDir := filepath.Join(root, "noaa-enc")
	catalog, err := NewENCCatalog(encDir, logger.Sublogger("encCatalog"))
	if err != nil {
		return nil, err
	}
	encStore, err := NewENCStore(encDir, catalog, logger.Sublogger("encStore"))
	if err != nil {
		return nil, err
	}
	encRenderer := NewENCRenderer(catalog, encStore, logger.Sublogger("encRender"))
	encTileCache, err := NewENCTileCache(filepath.Join(encStore.RootDir(), "tiles"))
	if err != nil {
		return nil, err
	}
	NewENCHandlers(catalog, encStore, encRenderer, encTileCache, wmsCache, safeDepthFt).Register(mux)
	logger.Infof("noaa enc store: %s (default safe_depth_ft=%.1f)", encDir, safeDepthFt)

	// Per-process instance ID. The frontend polls /version and reloads when it
	// changes, so the browser picks up a new build/restart without manual refresh.
	instanceID := newInstanceID()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"instance": instanceID})
	})

	// Optional override for the user's-own-boat marker icon. Resolved once at
	// startup; if the file is missing or unreadable we log and fall back to the
	// frontend's bundled default. AIS markers are unaffected.
	if myBoatIconPath != "" {
		abs, err := filepath.Abs(myBoatIconPath)
		if err != nil {
			logger.Warnf("myboat_icon_path %q: %v — falling back to default", myBoatIconPath, err)
		} else if info, err := os.Stat(abs); err != nil || info.IsDir() {
			logger.Warnf("myboat_icon_path %q not a readable file — falling back to default", abs)
		} else {
			mux.HandleFunc("/myboat-icon", func(w http.ResponseWriter, r *http.Request) {
				// Match the file's mtime in the ETag/If-Modified-Since flow that
				// http.ServeFile already implements, but no long-lived cache —
				// the user can swap the file and a reload picks it up.
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFile(w, r, abs)
			})
			logger.Infof("myboat icon: %s", abs)
		}
	}

	server.Addr = fmt.Sprintf(":%d", port)
	logger.Infof("going to listen on %v", server.Addr)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Errorf("error ListenAndServe: %v", err)
		}
	}()

	return &chartplotterResource{name: name, server: server}, nil
}

type chartplotterResource struct {
	resource.AlwaysRebuild

	name   resource.Name
	server *http.Server
}

func (r *chartplotterResource) Name() resource.Name { return r.name }

func (r *chartplotterResource) Close(ctx context.Context) error {
	return r.server.Close()
}

func (r *chartplotterResource) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	return nil, nil
}

func newInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails on a broken OS RNG; fall back to a fixed
		// string so the endpoint still responds (and reload-on-change still
		// works on the next successful start).
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}
