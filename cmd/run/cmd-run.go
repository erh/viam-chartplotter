package main

import (
	"context"
	"time"
	
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	
	"github.com/erh/vmodutils"

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

	ws, err := vmodutils.NewWebModuleAndStart(generic.Named("foo"), fs, logger, 8888)
	if err != nil {
		return err
	}
	defer ws.Close(ctx)
	
	time.Sleep(time.Minute)

	return nil
}
