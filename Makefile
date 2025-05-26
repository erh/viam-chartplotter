
ifneq (,$(wildcard test.make))
	include test.make
    export $(shell sed 's/=.*//' test.make)
endif

module: bin/viamchartplottermodule dist/index.html
	tar czf module.tar.gz bin/viamchartplottermodule meta.json dist

run: dist/index.html  Makefile
	go run cmd/run/cmd-run.go

dist/index.html: *.json src/*.css src/*.ts src/*.svelte src/lib/*.ts node_modules
	NODE_ENV=development npm run build

lint:
	gofmt -w .

bin/viamchartplottermodule: bin *.go cmd/module/*.go *.mod Makefile dist/index.html
	go build -o bin/viamchartplottermodule cmd/module/cmd.go

updaterdk:
	go get go.viam.com/rdk@latest
	go mod tidy


bin:
	-mkdir bin

node_modules: package.json
	npm install

setup-linux:
	which npm > /dev/null 2>&1 || apt -y install nodejs
