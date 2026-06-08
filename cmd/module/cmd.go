package main

import (
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/navigation"

	"github.com/erh/viam-chartplotter"
	"github.com/erh/viam-chartplotter/weather"
)

func main() {
	module.ModularMain(
		resource.APIModel{generic.API, vc.Model},
		resource.APIModel{navigation.API, vc.NavModel},
		// datasync keeps the noaa collection current (periodic catalog
		// refresh + ENC sync→ingest of every cell worldwide); weathersync
		// populates the weather collection from GRIB. Both write to the
		// shared Mongo the chartplotter / tileserver read from.
		resource.APIModel{generic.API, vc.DataSyncModel},
		resource.APIModel{generic.API, weather.WeatherSyncModel},
	)

}
