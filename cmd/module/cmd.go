package main

import (
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	"github.com/erh/viam-chartplotter"
)


func main() {
	module.ModularMain(
		resource.APIModel{generic.API, vc.Model},
	)

}
