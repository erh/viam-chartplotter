.PHONY: all test build clean docs docs-build docs-check-examples

# Default target
all: test build

# Run all tests
test:
	go test -v ./...

# Build the library
build:
	go build ./...

# Clean build artifacts
clean:
	go clean
	rm -f coverage.out

# Build documentation site
docs-build:
	cd docs && npm install && npm run build

# Check that all documentation examples compile
docs-check-examples:
	@echo "Checking documentation examples..."
	@for dir in docs/examples/*/; do \
		echo "Compiling $$dir..."; \
		cd "$$dir" && go build -o /dev/null . || exit 1; \
		cd - > /dev/null; \
	done
	@echo "✓ All examples compile successfully"

# Full docs validation: build site and check examples
docs: docs-check-examples docs-build
	@echo "✓ Documentation validated successfully"
