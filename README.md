# git3

git3 is a Git remote helper that stores a complete, ordinary Git repository in an S3 bucket you
control. There is no server, account system, mounted filesystem, or custom local object database.
After a fetch, stock Git owns the installed native packs.

> Direct write access to the reserved S3 prefix is repository-administrator access. git3 does not
> provide branch protection against an AWS principal that can replace repository metadata.

## Requirements and installation

git3 requires Git 2.38 or newer and an existing S3 general-purpose bucket. Official releases target
Linux and macOS on amd64 and arm64. Install a verified release into `~/.local/bin`:

```sh
curl -fsSL https://github.com/robertpitt/git3/releases/latest/download/install.sh | sh
```

For a higher-assurance installation, download a versioned archive, `checksums.txt`, and its Sigstore
bundle from the release page; verify the bundle and GitHub artifact attestation before extraction.

Build from source with `go build ./cmd/git3`, then install the result as `git3` and create
`git-s3` and `git-remote-s3` symlinks beside it.

## Quick start

The bucket must already exist. AWS credentials and region come from the standard AWS SDK chain.

```sh
git remote add origin s3://my-bucket/repos/example
git push -u origin HEAD:refs/heads/main
git clone s3://my-bucket/repos/example
git s3 doctor origin
```

The first valid branch push initializes a missing repository prefix. The URL never contains
credentials, region, profile, endpoint, or encryption configuration.

## Configuration

Settings use `git3.*` Git configuration, corresponding `remote.<name>.git3*` keys, or `GIT3_*`
environment variables. Environment values take precedence. Important settings include
`GIT3_REGION`, `GIT3_ENDPOINT`, `GIT3_PATH_STYLE`, `GIT3_SSE` (`inherit`, `s3`, or `kms`),
`GIT3_KMS_KEY_ID`, transfer sizes/concurrency, retry attempts, and compaction thresholds. Byte
quantities accept bytes or `KiB`, `MiB`, `GiB`, and `TiB`. HTTP endpoints require
`GIT3_ALLOW_INSECURE_ENDPOINT=true` and are intended only for local testing.

Repository encryption policy is fixed at initialization. Conflicting writer settings fail rather
than silently changing it.

## Operations

```text
git3 doctor <remote-or-url> [--json] [--write-test]
git3 fsck <remote-or-url> [--full]
git3 maintenance <remote-or-url> [--max-bytes N] [--all]
git3 gc <remote-or-url> [--json]
git3 gc <remote-or-url> --execute --older-than 30d
git3 gc <remote-or-url> --resume <plan-id>
git3 gc <remote-or-url> --abort <plan-id>
git3 set-head <remote-or-url> refs/heads/main
```

Maintenance runs Git computation in a complete local clone and advances the physical bootstrap
state without changing the logical transaction. GC is dry-run by default; deletion requires an
explicit cutoff and a published, resumable barrier. Normal clone, fetch, push, and maintenance do
not need S3 list or delete permission.

See [configuration](docs/configuration.md), [operations](docs/operations.md), [IAM examples](docs/iam.md), and the normative [SPEC](SPEC.md).

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

The in-memory S3 implementation supports deterministic engine, concurrency, and fault tests. AWS
release qualification must additionally use an isolated real S3 bucket.

Licensed under Apache-2.0.
