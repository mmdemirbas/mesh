# Contributing to mesh

## Getting Started

1. Install Go 1.26+ and [Task](https://taskfile.dev/)
2. Clone the repo and run `task build`
3. Run tests: `task test`

## Development Workflow

```bash
task build          # Build to build/mesh
task test           # Run all tests
task test:v         # Verbose test output
task bench          # Run benchmarks
```

## Submitting Changes

1. Fork the repository
2. Create a feature branch from `main`
3. Make your changes
4. Ensure all tests pass: `go test -race ./...`
5. Open a pull request with a clear description

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep functions focused and small
- Add tests for new functionality
- Use table-driven tests where appropriate
- Avoid unnecessary abstractions — simple and direct is preferred

## Reporting Issues

- Use GitHub Issues for bug reports and feature requests
- For security vulnerabilities, see [SECURITY.md](SECURITY.md)
- Include steps to reproduce, expected vs actual behavior, and your platform/Go version

## Testing

- Unit tests: `go test ./...`
- Race detector: `go test -race ./...`
- Benchmarks: `go test -bench=. -benchmem ./...`

All PRs must pass the full test suite with the race detector enabled.
