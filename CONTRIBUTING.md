# Contributing

Open an issue before changing the remote format. Go code follows the
[Google Go Style Guide](https://google.github.io/styleguide/go/), its
[style decisions](https://google.github.io/styleguide/go/decisions), and its
[best practices](https://google.github.io/styleguide/go/best-practices). In particular:

- Optimize for clarity, simplicity, and maintainability.
- Format all Go source with `gofmt`.
- Give every exported declaration a complete doc comment.
- Wrap errors with useful operation context and preserve errors that callers inspect.
- Prefer focused, table-driven tests with actionable failure messages.

Before submitting a change, run `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`, and
`staticcheck ./...`. Changes to canonical formats, publication boundaries, conditional requests,
or GC require corruption and fault-injection tests. Commits must include Apache-2.0-compatible work.
Please follow the Code of Conduct and avoid real customer data in fixtures.
