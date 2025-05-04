

run: dist/index.html 
	go run cmd/run/cmd-run.go

dist/index.html: *.json src/*.css src/*.ts src/*.svelte src/lib/*.ts
	npm run build

lint:
	gofmt -w .
