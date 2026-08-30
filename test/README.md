# Conformance tests

The executable conformance suite lives beside the internal package it exercises so `go test ./...`
runs it automatically. It covers canonical encoding and corruption, locator containment, snapshots,
SHA-1 and SHA-256 push/fetch round trips, conditional cache no-ops, budgeted and geometric
maintenance, cold bootstrap, GC, eight-writer CAS races, and lost publication responses.

`test/fixtures` contains stable external-format examples. CI does not require AWS credentials or a
live bucket: repository behavior uses the in-memory Store adapter, and S3 request construction,
pagination, conditional operations, multipart behavior, and error mapping use an injected AWS SDK
mock client.
