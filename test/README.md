# Conformance tests

The executable conformance suite lives beside the internal package it exercises so `go test ./...`
runs it automatically. It covers canonical encoding and corruption, locator containment, snapshots,
SHA-1 and SHA-256 push/fetch round trips, conditional cache no-ops, budgeted and geometric
maintenance, cold bootstrap, GC, eight-writer CAS races, and lost publication responses.

`test/fixtures` contains stable external-format examples. Real-AWS release qualification supplies an
isolated bucket through protected CI configuration; it is intentionally not runnable against an
unscoped user bucket.
