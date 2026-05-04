package vc

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"

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
	return StartChartplotterServer(config.ResourceName(), dist, logger, port, cacheDir)
}

// StartChartplotterServer wires the static frontend together with the NOAA WMS caching
// proxy and starts an HTTP server on the given port.
func StartChartplotterServer(name resource.Name, dist fs.FS, logger logging.Logger, port int, cacheDir string) (resource.Resource, error) {
	mux, server, err := vmodutils.PrepInModuleServer(dist, logger.Sublogger("accessLog"))
	if err != nil {
		return nil, err
	}

	cache, err := NewNoaaCache(cacheDir, logger.Sublogger("noaaCache"))
	if err != nil {
		return nil, err
	}
	cache.Register(mux)
	logger.Infof("noaa cache dir: %s", cache.cacheDir)

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
