package vc

import (
	"context"
	"embed"
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
	return StartChartplotterServer(config.ResourceName(), dist, logger, port, cacheDir, cacheMaxBytes)
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
func StartChartplotterServer(
	name resource.Name,
	dist fs.FS,
	logger logging.Logger,
	port int,
	cacheRoot string,
	cacheMaxBytes int64,
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
	NewENCHandlers(catalog, encStore).Register(mux)
	logger.Infof("noaa enc store: %s", encDir)

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

func (r *chartplotterResource) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}
