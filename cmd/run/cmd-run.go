package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"

	"github.com/erh/viam-chartplotter"
)

func main() {
	err := realMain()
	if err != nil {
		panic(err)
	}
}

func realMain() error {
	ctx := context.Background()
	logger := logging.NewLogger("cmd-run")

	fs, err := vc.DistFS()
	if err != nil {
		return err
	}

	ws, err := vc.StartChartplotterServer(generic.Named("foo"), fs, logger, 8888, "", 0, 6)
	if err != nil {
		return err
	}
	defer ws.Close(ctx)

	// Block until Ctrl+C / SIGTERM. The previous behaviour was a 60-second
	// timer, which silently killed the server right when you started panning
	// around the chart and made every tile appear "stale" because the browser
	// kept painting OpenLayers' in-memory cache.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	logger.Info("running; ctrl+c to exit")
	<-sigs
	logger.Info("shutting down")
	return nil
}
