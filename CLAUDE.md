# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build/Test Commands
- Build: `go build -o net-init main.go`
- Build with flags: `CGO_ENABLED=0 go build -ldflags="-w -s" -o net-init main.go`
- Run tests: `go test -v ./...`
- Run single test: `go test -v -run=TestName` (e.g., `go test -v -run=TestParseConfig`)
- Build multi-platform: `./build.sh -t your-image:tag` (supports `--platforms`, `--push`, etc.)
- Integration test: `cd integration-test && ./run-test.sh`
- Format code: `gofmt -s -w *.go`

## Code Style Guidelines
- Format: Standard Go formatting (gofmt -s)
- Imports: Group standard library first, then external packages
- Error handling: Always check errors, use formatted errors with context
- Logging: Use the structured logger (slog) with appropriate levels
- Variable naming: Use camelCase, descriptive names
- Function naming: Follow Go conventions (e.g., exported functions are PascalCase)
- Comments: Document exported functions, types, and non-obvious logic
- Tests: Write tests for all new functionality
- Dependency injection: Prefer explicit dependency passing over globals
- Code complexity: Keep cyclomatic complexity below 15 per function
- Memory usage: Extract reusable logic to helper functions to reduce duplication