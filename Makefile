
ifneq (,$(wildcard test.make))
	include test.make
    export $(shell sed 's/=.*//' test.make)
endif

run: dist/index.html  Makefile
	go run cmd/run/cmd-run.go

dist/index.html: *.json src/*.css src/*.ts src/*.svelte src/lib/*.ts
	NODE_ENV=development npm run build

lint:
	gofmt -w .

bin/viamchartplottermodule: bin *.go cmd/module/*.go *.mod Makefile
	go build -o bin/viamchartplottermodule cmd/module/cmd.go

updaterdk:
	go get go.viam.com/rdk@latest
	go mod tidy

module: bin/viamchartplottermodule
	tar czf module.tar.gz bin/viamchartplottermodule meta.json

bin:
	-mkdir bin
