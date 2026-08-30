# git3 — The S3 Backend for Git Specification

**Status:** Draft for implementation  
**Specification version:** 1.0.0-draft  
**Remote format version:** 1  
**Date:** 2026-08-29  
**Reference implementation:** Go, Apache License 2.0

## 1. Purpose

git3 is an open-source Git remote helper that stores a complete Git repository in a bucket owned
and operated by the user. After installation, ordinary Git commands MUST recognize `s3://` remotes
without a daemon, mounted filesystem, hosted Git provider, or git3-specific object database.

The system synchronizes clients against a logical, repository-wide transaction sequence. Native Git
packfiles are physical storage and transfer artifacts; their identities MUST NOT be part of a
client's logical synchronization cursor. Remote compaction MAY replace packfiles without creating a
Git transaction or forcing an up-to-date client to download repository data again.

The defining portability test is:

> After a successful clone or fetch, removing git3 MUST leave an ordinary local Git repository whose
> objects and refs can be inspected, verified, repacked, and used by stock Git.

## 2. Normative language and definitions

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHOULD**, **SHOULD NOT**, and **MAY** are to be
interpreted as described by RFC 2119 and RFC 8174 when, and only when, they appear in uppercase.

**Implementation-defined** means a choice that an implementation may make, but which it must
document and keep compatible with the externally visible contracts in this specification.

Terms used throughout this specification:

- **Repository locator:** A normalized S3 bucket and key prefix obtained from an `s3://` URL.
- **Reserved prefix:** `<repository-prefix>/.git/git3/`, or `.git/git3/` when the repository prefix is empty.
- **HEAD object:** The mutable S3 object at `<reserved-prefix>HEAD`. It is distinct from Git's local
  `.git/HEAD` file.
- **Logical generation:** A gap-free unsigned integer that advances once for each published,
  non-empty ref transaction.
- **Manifest revision:** A gap-free unsigned integer that advances on every replacement of the HEAD
  object, including physical maintenance and default-branch changes.
- **Transaction:** One atomic set of accepted Git ref updates and its optional object-data pack.
- **Transaction pack:** An immutable, normally thin Git pack containing data for one transaction.
- **Packset:** An immutable manifest for non-thin native Git packs used to bootstrap new, damaged, or
  stale clients.
- **Cursor:** Small local metadata asserting a verified logical generation and transaction ID. It
  never identifies local or remote packfiles.
- **Publication:** A successful conditional creation or replacement of the HEAD object.
- **Object reference:** A manifest value containing an S3 key, byte length, and SHA-256 digest.

## 3. Problem statement

Developers and organizations need a Git hosting mode in which repository data remains in their own
S3 buckets and access is controlled by their own AWS credentials and IAM policy. A naive mapping of
Git objects or branches to independent S3 keys performs too many small, latency-dependent requests.
A single ever-growing pack manifest creates unbounded metadata, while repeated monolithic
checkpoints create unacceptable write amplification for large repositories.

Git is also free to repack a local object database, and remote maintenance is free to repack S3
objects. A synchronization design that records physical pack identity therefore mistakes benign
maintenance for new repository content. git3 instead separates:

1. a logical plane consisting of atomic ref transactions; and
2. a physical plane consisting of geometrically compacted native Git packs.

S3 provides storage and a single-key compare-and-swap primitive through conditional requests. A
user-controlled workstation or CI runner provides all Git computation.

## 4. Goals and non-goals

### 4.1 Goals

The complete target implementation MUST:

1. Support `git clone`, `git fetch`, `git pull`, `git push`, branch and tag creation, branch and tag
   deletion, forced updates, force-with-lease behavior, and atomic multi-ref pushes over `s3://`.
2. Create a missing repository prefix automatically on the first valid push, while requiring the S3
   bucket itself to exist already.
3. Support SHA-1 and SHA-256 Git repositories without translating between object formats.
4. Publish ref changes atomically through one conditional HEAD replacement.
5. Ensure that published refs never depend on absent or unverified remote object data.
6. Make a no-op fetch require one conditional S3 read, zero Git-object bytes, and no material local
   writes.
7. Transfer active clients by logical transactions, independently of remote and local repacking.
8. Provide bounded mutable S3 metadata and a geometrically compacted packset for cold clients.
9. Install native Git packs directly into `.git/objects/pack` and use Git plumbing for pack parsing,
   indexing, connectivity, MIDX, and local maintenance.
10. Avoid S3 `LIST` calls during clone, fetch, push, ref advertisement, and compaction.
11. Use the standard AWS credential chain and IAM as the authentication and authorization boundary.
12. Provide `doctor`, `fsck`, `maintenance`, `gc`, and default-branch administration commands.
13. Ship statically linked release binaries for Linux and macOS on x86-64 and ARM64, with checksums,
    signatures, and build provenance.
14. Remain usable at a design envelope of 1 TiB of reachable Git data and 100,000 refs.

### 4.2 Non-goals

The following are explicitly outside the version 1 core contract:

- Creating S3 buckets, configuring bucket policy, configuring lifecycle policy, or selecting a data
  retention period.
- Shallow clone, partial clone, promisor-object operation, and serverless random retrieval of an
  individual missing Git object.
- Integrated Git LFS storage. Users may configure an independent LFS endpoint.
- Server-side hooks, protected branches, review rules, merge queues, or policy that purports to be
  authoritative against a principal with direct write access to the reserved S3 prefix.
- Client-side encryption. Server-side encryption is the version 1 encryption boundary.
- A coordinator, always-on service, web UI, user database, or custom identity system.
- Per-object S3 storage, cross-repository deduplication, or a second complete cache outside `.git`.
- Translation between SHA-1 and SHA-256 repositories.
- Windows binaries in the initial release.
- Compatibility with the storage layouts of other projects that use the `git-remote-s3` executable
  name.
- `git archive --remote`, Git wire-protocol server emulation, or direct `upload-pack`/`receive-pack`
  service endpoints.
- Guaranteed high write throughput beyond the single repository-wide S3 compare-and-swap point.

Ordinary Git submodules MAY use git3 URLs. Users are responsible for allowing the `s3` protocol in
their Git configuration and for ensuring that git3 is installed in the submodule environment.

## 5. Users, actors, and trust boundaries

### 5.1 Actors

- **Git user:** Runs ordinary Git commands and reads progress or errors on stderr.
- **Repository writer:** An AWS principal allowed to read and write the reserved prefix.
- **Repository reader:** An AWS principal allowed to read the reserved prefix.
- **Maintainer:** A repository writer with a complete local object database and enough local space to
  compact selected packs.
- **GC operator:** A privileged maintainer additionally allowed to list and delete reserved-prefix
  objects.
- **Release maintainer:** A GitHub project role allowed to approve and publish tagged releases.

### 5.2 Trust model

AWS credentials, IAM, bucket policy, KMS policy, TLS, and S3 integrity controls form the remote trust
boundary. git3 MUST NOT maintain a second authentication layer.

All clients with `s3:PutObject` access to `.git/git3/HEAD` are trusted to follow this protocol. Repository
metadata cannot provide authoritative branch protection against such a client. Signed commits and
signed tags remain ordinary opaque Git objects and MUST be preserved.

The local working tree, local Git object database, Git configuration, remote metadata, S3 responses,
custom endpoint responses, and release downloads MUST be treated as untrusted input. AWS secret
access keys and session tokens MUST NOT be written to Git config, repository metadata, logs, process
arguments, or URLs.

## 6. System overview

### 6.1 Components

The reference distribution contains one Go executable installed under three names:

- `git3`: direct administrative CLI;
- `git-s3`: enables `git s3 <command>` through Git's dashed-command discovery; and
- `git-remote-s3`: automatically invoked by Git for `s3://` remotes.

The executable MUST dispatch by `argv[0]`. The installer SHOULD use relative symlinks when supported
and MUST fall back to copies if symlink creation is unavailable.

Internal responsibility boundaries are:

1. **Remote-helper adapter:** Implements Git's line protocol and writes no diagnostics to stdout.
2. **Repository engine:** Validates refs, transactions, invariants, state transitions, and cursors.
3. **Git plumbing adapter:** Executes bounded, argument-vector Git subprocesses; it never invokes a
   command through a shell.
4. **S3 store:** Performs conditional reads and writes, immutable uploads, multipart transfer, range
   download, checksums, and normalized error mapping.
5. **Local state manager:** Owns `.git/git3`, atomic cursor files, temporary downloads, and locks.
6. **Maintenance engine:** Creates ref snapshots, compacts packs, publishes packsets, and advances the
   transaction-log floor.
7. **GC engine:** Discovers unreachable immutable objects, publishes a deletion barrier, and performs
   conditional deletion only after revalidation.
8. **Release system:** Builds, tests, attests, signs, and publishes versioned binaries and installer
   assets from GitHub Actions.

### 6.2 External dependencies

- Git 2.38 or newer, available on `PATH`;
- an AWS S3 general purpose bucket;
- AWS SDK for Go v2 in the reference implementation;
- standard AWS credential and shared-configuration sources;
- GitHub Actions and GitHub Releases for official project distribution; and
- `curl` or `wget`, a POSIX shell, and `sha256sum` or `shasum` for the convenience installer.

AWS S3 is normative. S3-compatible custom endpoints are optional and conform only if they provide
strong read-after-write behavior, atomic single-key replacement, conditional `If-Match` and
`If-None-Match` writes, byte-range reads, multipart upload, and the error semantics required here.

### 6.3 Fundamental invariants

The following invariants MUST always hold:

1. The HEAD object is the only mutable object used by normal repository operation.
2. The S3 ETag of the last-read HEAD object is the compare-and-swap token; it is not treated as a
   content digest.
3. An immutable object is uploaded and verified before any published manifest may reference it.
4. A successful ref transaction advances `logicalGeneration` by exactly one.
5. Physical maintenance preserves `logicalGeneration` and `transactionId`.
6. Every HEAD replacement advances `manifestRevision` by exactly one and assigns a new
   `publicationId`.
7. Transactions form one gap-free repository-wide parent chain and may contain multiple refs.
8. The current packset plus every transaction after its generation contains the complete object
   closure required by the current ref map.
9. The current ref snapshot plus every transaction after its generation reconstructs the exact
   current ref map.
10. A cursor is valid only when its generation and transaction ID match a verified remote chain and
    required local object connectivity has passed.
11. A local or remote pack filename is never part of logical cursor validity.
12. A reader never uses `LIST` to discover live repository state; it follows authenticated manifest
    references from HEAD.

## 7. Repository locator and S3 namespace

### 7.1 URL contract

The accepted form is:

```text
s3://<bucket>[/<repository-prefix>]
```

The URL parser MUST:

1. require the scheme to be exactly `s3` and a non-empty bucket;
2. reject user information, query parameters, fragments, NULs, control characters, backslashes,
   `.` segments, `..` segments, and empty interior path segments;
3. percent-decode each path segment exactly once;
4. remove the URL's leading slash and any trailing slash;
5. preserve the decoded bytes and case of all remaining prefix segments; and
6. allow an empty prefix, which reserves `.git/git3/` at bucket root.

Credentials, profile names, regions, endpoints, and encryption options MUST NOT be embedded in the
URL. Two different locators MAY identify the same repository; `repositoryId` in remote metadata is
the stable logical identity.

### 7.2 Reserved namespace

All managed objects MUST be below `<repository-prefix>/.git/git3/`:

```text
.git/git3/
  HEAD
  wal/<transaction-id>.pack
  transactions/<generation>-<transaction-id>.json
  log-pages/<page-id>.json
  refs/<snapshot-id>.refs
  packsets/<packset-id>.json
  packs/pack-<git-pack-checksum>.pack
  packs/pack-<git-pack-checksum>.idx
  gc/<plan-id>.json
  probes/<probe-id>
```

`<generation>` MUST be a 20-digit, zero-padded decimal value. UUIDs MUST use lowercase canonical
UUID textual form. An implementation MUST reject any referenced key that is outside the exact
reserved prefix, contains an empty segment, or contains `.` or `..` as a segment.

Objects outside the reserved prefix are not owned by git3 and MUST NOT be read, modified, listed, or
deleted. Existing unrelated objects below the repository prefix do not prevent initialization.

### 7.3 Mutability classes

- `.git/git3/HEAD` is mutable and MUST be written with a conditional request.
- Every other managed object is immutable. Creation MUST use `If-None-Match: *`.
- If immutable creation returns a precondition failure, the client MUST read the existing object and
  continue only if its byte length and SHA-256 digest exactly match the intended object.
- A normal operation MUST NOT overwrite, copy over, or delete an immutable object.

## 8. Encoding and common data types

### 8.1 JSON

All JSON objects defined by this specification MUST be UTF-8 and serialized using RFC 8785 JSON
Canonicalization Scheme without trailing whitespace. Producers MUST emit integers, not floating
point values, for counters and byte sizes. Version 1 counters MUST be in the range `0` through
`9,007,199,254,740,991` so that they remain exact in common JSON implementations.

Readers MUST reject duplicate object keys, invalid UTF-8, non-canonical numeric values, unexpected
nulls, invalid identifiers, invalid object IDs, and data that violates an invariant. Readers MUST
ignore unknown fields unless the HEAD object's `requiredFeatures` contains a value they do not
support. A client that rewrites HEAD MUST preserve unknown optional fields it did not intentionally
modify. Readers MUST reject an unknown `formatVersion`.

Timestamps MUST use UTC RFC 3339 with whole seconds and a `Z` suffix. Timestamps are informational;
they MUST NOT determine transaction ordering or CAS correctness.

Version 1 conformance limits are:

| Item | Maximum |
| --- | ---: |
| Current direct refs | 100,000 |
| Ref updates in one transaction | 100,000 |
| Canonical transaction descriptor | 64 MiB |
| Canonical log page | 128 MiB and 32 transaction envelopes |
| Ref snapshot | 128 MiB |
| Packset manifest | 64 MiB |
| HEAD | 2 MiB |
| Executing GC plan | 100,000 candidates and 256 MiB |

Implementations MAY support larger repositories, but MUST enforce documented input limits before
unbounded allocation. GC with more than 100,000 eligible candidates MUST execute in separately
barriered, independently revalidated plans; it MUST NOT silently truncate the reported dry run.

### 8.2 Object IDs and refs

- `objectFormat` is `sha1` or `sha256`.
- SHA-1 object IDs are 40 lowercase hexadecimal characters.
- SHA-256 object IDs are 64 lowercase hexadecimal characters.
- A JSON null old or new value denotes absence of that ref.
- Ref names MUST start with `refs/`, pass `git check-ref-format`, be valid UTF-8, and be preserved
  byte-for-byte without Unicode normalization.
- `HEAD` is the only supported symbolic ref and its target MUST be a present `refs/heads/*` ref.
- Transaction update lists and ref snapshots MUST be sorted by raw UTF-8 ref-name bytes and contain
  no duplicates.

### 8.3 Object references

An object reference has this exact required shape, with optional future fields permitted:

```json
{
  "key": ".git/git3/example/object",
  "size": 1234,
  "sha256": "64-lowercase-hex-characters"
}
```

`key` is relative to the repository prefix, so it begins with `.git/git3/`. `size` is the exact content
length. `sha256` covers the exact stored bytes after transport decryption and before any local
transformation.

## 9. Remote data model

### 9.1 HEAD object

The HEAD object has the following semantic shape:

```json
{
  "formatVersion": 1,
  "requiredFeatures": [],
  "repositoryId": "73bcb050-8b53-4e47-a5a4-e661bd5c8faf",
  "objectFormat": "sha1",
  "logicalGeneration": 1824,
  "transactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
  "manifestRevision": 294,
  "publicationId": "9d0a1c11-bf68-475c-8bfa-a019028d7e18",
  "headSymref": "refs/heads/main",
  "storagePolicy": {
    "serverSideEncryption": "inherit",
    "kmsKeyId": null,
    "bucketKeyEnabled": null
  },
  "refSnapshot": {
    "snapshotId": "be16f159-6a19-4db1-a050-fb25fa4d9b07",
    "generation": 1824,
    "transactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
    "object": {
      "key": ".git/git3/refs/be16f159-6a19-4db1-a050-fb25fa4d9b07.refs",
      "size": 108219,
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  },
  "packset": {
    "packsetId": "04ce3e30-f29d-40f0-868c-798b91c57083",
    "generation": 1824,
    "transactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
    "object": {
      "key": ".git/git3/packsets/04ce3e30-f29d-40f0-868c-798b91c57083.json",
      "size": 4096,
      "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    }
  },
  "log": {
    "floorGeneration": 1824,
    "floorTransactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
    "tipPage": null,
    "tail": []
  },
  "gcBarrier": null
}
```

The following additional invariants apply:

- For generation zero, `transactionId`, `headSymref`, `refSnapshot.transactionId`,
  `packset.transactionId`, and `log.floorTransactionId` are null.
- `packset.generation` MUST equal `log.floorGeneration`.
- `packset.transactionId` MUST equal `log.floorTransactionId`.
- `refSnapshot.generation` MUST be between `log.floorGeneration` and `logicalGeneration`, inclusive.
- `tipPage` followed by `tail` MUST describe a consecutive chain ending at
  `logicalGeneration`/`transactionId` and beginning immediately after the log floor.
- `headSymref`, when non-null, MUST resolve in the reconstructed current ref map.
- The serialized HEAD object MUST NOT exceed 2 MiB.
- The serialized transaction records in `log.tail` MUST NOT exceed 1 MiB or 32 records. A push that
  would exceed either limit MUST seal the proposed tail into a log page before publication.

`gcBarrier`, when non-null, is defined in section 18. HEAD replacements performed during a barrier
MUST preserve it byte-for-byte unless performed by the owning GC operation.

### 9.2 Transaction descriptor

Each effective push produces one canonical transaction descriptor:

```json
{
  "formatVersion": 1,
  "repositoryId": "73bcb050-8b53-4e47-a5a4-e661bd5c8faf",
  "objectFormat": "sha1",
  "generation": 1843,
  "transactionId": "8af84623-3cd0-4077-bc74-a2402805189c",
  "parentGeneration": 1842,
  "parentTransactionId": "cb356bf4-1fc6-409b-83ef-81fbc3c4ac16",
  "createdAt": "2026-08-29T14:18:31Z",
  "writerVersion": "1.0.0",
  "updates": [
    {
      "ref": "refs/heads/main",
      "old": "7e8b6c0000000000000000000000000000000000",
      "new": "20f5530000000000000000000000000000000000",
      "kind": "fast-forward"
    }
  ],
  "objectData": {
    "object": {
      "key": ".git/git3/wal/8af84623-3cd0-4077-bc74-a2402805189c.pack",
      "size": 87342,
      "sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    },
    "gitPackChecksum": "dddddddddddddddddddddddddddddddddddddddd",
    "thin": true,
    "baseGeneration": 1842,
    "baseTransactionId": "cb356bf4-1fc6-409b-83ef-81fbc3c4ac16"
  }
}
```

`kind` MUST be one of `create`, `fast-forward`, `force`, or `delete`. `objectData` MUST be null for a
deletion-only or ref-only transaction that generated no pack bytes. A transaction descriptor MUST
contain at least one effective update.

The descriptor is stored at
`.git/git3/transactions/<generation>-<transaction-id>.json`. Its parent fields MUST match the parent HEAD
that the writer pinned. `gitPackChecksum` is the checksum in the native Git pack trailer and has the
length implied by `objectFormat`; SHA-256 remains the independent S3 payload checksum.

### 9.3 Transaction envelope and log pages

A transaction envelope embeds the complete transaction and a reference to its separately stored
descriptor. The following shows the wrapper; `transaction` MUST contain every field from section 9.2,
not only the abbreviated field shown here:

```json
{
  "descriptor": {
    "key": ".git/git3/transactions/00000000000000001843-8af84623-3cd0-4077-bc74-a2402805189c.json",
    "size": 1102,
    "sha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
  },
  "transaction": { "formatVersion": 1 }
}
```

The elided `transaction` value MUST be byte-for-byte semantically equal to the referenced canonical
descriptor after parsing. Readers MAY avoid fetching individual descriptors when a verified HEAD or
log page contains their envelopes.

A log page has this shape:

```json
{
  "formatVersion": 1,
  "repositoryId": "73bcb050-8b53-4e47-a5a4-e661bd5c8faf",
  "pageId": "b32c4b34-5650-4570-9436-c5d95d515b2a",
  "previous": null,
  "baseGeneration": 1824,
  "baseTransactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
  "firstGeneration": 1825,
  "lastGeneration": 1825,
  "transactions": [
    {
      "descriptor": {
        "key": ".git/git3/transactions/00000000000000001825-f71bd3c4-0ff0-40e2-8b61-e64915f479a0.json",
        "size": 1002,
        "sha256": "5555555555555555555555555555555555555555555555555555555555555555"
      },
      "transaction": {
        "formatVersion": 1,
        "repositoryId": "73bcb050-8b53-4e47-a5a4-e661bd5c8faf",
        "objectFormat": "sha1",
        "generation": 1825,
        "transactionId": "f71bd3c4-0ff0-40e2-8b61-e64915f479a0",
        "parentGeneration": 1824,
        "parentTransactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
        "createdAt": "2026-08-29T14:00:00Z",
        "writerVersion": "1.0.0",
        "updates": [
          {
            "ref": "refs/heads/topic",
            "old": "6666666666666666666666666666666666666666",
            "new": null,
            "kind": "delete"
          }
        ],
        "objectData": null
      }
    }
  ]
}
```

`transactions` MUST contain consecutive envelopes in generation order. `previous` is null when the
page begins immediately after the current or an historical log floor; otherwise it is an object
reference plus `pageId`, `firstGeneration`, and `lastGeneration` for the preceding page. HEAD's
`tipPage` uses the same page-pointer shape.

Page traversal is newest-to-oldest through `previous`, but records are applied oldest-to-newest. A
reader MUST stop at `floorGeneration` even if a live page contains a historical `previous` pointer
below that floor. Such a pointer does not make below-floor data live for GC purposes.

### 9.4 Ref snapshot

A ref snapshot is an immutable UTF-8 text file with LF endings and this exact grammar:

```text
git3-ref-snapshot 1
repository <repository-uuid>
object-format <sha1|sha256>
generation <unsigned-decimal>
transaction <transaction-uuid|->

<object-id> <ref-name>
<object-id> <ref-name>
```

The header ends at the first empty line. At generation zero, `transaction` is `-` and there are no
ref records. Ref records MUST be sorted and validated as defined in section 8. `HEAD` is not present
in the file; the symbolic default branch lives in the mutable HEAD object.

### 9.5 Packset manifest

A packset manifest has this semantic shape:

```json
{
  "formatVersion": 1,
  "repositoryId": "73bcb050-8b53-4e47-a5a4-e661bd5c8faf",
  "objectFormat": "sha1",
  "packsetId": "04ce3e30-f29d-40f0-868c-798b91c57083",
  "generation": 1824,
  "transactionId": "2bc4993f-404d-419d-8c28-b6c7427fce37",
  "levels": [
    {
      "level": 0,
      "packs": [
        {
          "gitPackChecksum": "ffffffffffffffffffffffffffffffffffffffff",
          "objectCount": 4312,
          "pack": {
            "key": ".git/git3/packs/pack-ffffffffffffffffffffffffffffffffffffffff.pack",
            "size": 44028102,
            "sha256": "1111111111111111111111111111111111111111111111111111111111111111"
          },
          "index": {
            "key": ".git/git3/packs/pack-ffffffffffffffffffffffffffffffffffffffff.idx",
            "size": 121804,
            "sha256": "2222222222222222222222222222222222222222222222222222222222222222"
          }
        }
      ]
    }
  ]
}
```

An empty generation-zero packset has an empty `levels` array. Each pack MUST be a native non-thin Git
pack with a matching native index. Level numbers MUST be unique, ascending non-negative integers;
pack entries MUST be sorted by `gitPackChecksum`; and pack keys MUST match the checksum. The union of
all packs MAY contain duplicate or currently unreachable Git objects, but MUST provide the closure
required by refs at the packset generation.

Packset manifests MUST NOT include local MIDX or commit-graph files. Clients create local derived
indexes from installed native packs.

## 10. Configuration contract

### 10.1 Precedence

Configuration precedence, from highest to lowest, is:

1. administrative CLI flags, where applicable;
2. configuration environment variables, including `AWS_PROFILE` and `GIT3_*`;
3. `remote.<name>.git3*` configuration for a named Git remote;
4. repository-local, global, and system `git3.*` configuration in normal Git precedence order;
5. documented defaults.

AWS credentials and shared AWS settings retain the AWS SDK's own standard precedence. git3 MUST NOT
copy AWS credentials into its own configuration layer. Configuration is loaded once per process;
dynamic reload is not required.

### 10.2 Defined settings

| Git configuration | Environment | Default | Contract |
| --- | --- | --- | --- |
| `git3.profile` | `AWS_PROFILE` | AWS resolution | Optional shared AWS configuration profile. |
| `git3.region` | `GIT3_REGION` | AWS resolution | Optional signing and bucket region. |
| `git3.endpoint` | `GIT3_ENDPOINT` | AWS endpoint | Optional absolute custom endpoint URL. |
| `git3.pathStyle` | `GIT3_PATH_STYLE` | `false` | Use path-style S3 addressing. |
| `git3.allowInsecureEndpoint` | `GIT3_ALLOW_INSECURE_ENDPOINT` | `false` | Permit an `http://` custom endpoint. |
| `git3.sse` | `GIT3_SSE` | `inherit` | `inherit`, `s3`, or `kms` at initialization. |
| `git3.kmsKeyId` | `GIT3_KMS_KEY_ID` | unset | KMS key ARN, ID, or alias when `sse=kms`. |
| `git3.bucketKeyEnabled` | `GIT3_BUCKET_KEY_ENABLED` | unset | Optional S3 Bucket Key choice for KMS. |
| `git3.multipartThreshold` | `GIT3_MULTIPART_THRESHOLD` | `100MiB` | Minimum object size for multipart when size is known. |
| `git3.partSize` | `GIT3_PART_SIZE` | `128MiB` | Multipart upload part size. |
| `git3.downloadChunkSize` | `GIT3_DOWNLOAD_CHUNK_SIZE` | `64MiB` | Byte-range chunk size. |
| `git3.downloadConcurrency` | `GIT3_DOWNLOAD_CONCURRENCY` | `4` | Concurrent range reads per object. |
| `git3.maxAttempts` | `GIT3_MAX_ATTEMPTS` | `5` | Maximum attempts for a retryable request. |
| `git3.logFormat` | `GIT3_LOG_FORMAT` | `human` | `human` or `json`. |
| `git3.compactionFanout` | `GIT3_COMPACTION_FANOUT` | `4` | Size-tiered pack promotion fanout. |
| `git3.compactAfterTransactions` | `GIT3_COMPACT_AFTER_TRANSACTIONS` | `32` | Maintenance-due transaction threshold. |
| `git3.compactAfterBytes` | `GIT3_COMPACT_AFTER_BYTES` | `128MiB` | Maintenance-due WAL-byte threshold. |

For a named remote, the corresponding keys are, for example,
`remote.origin.git3Region`, `remote.origin.git3Endpoint`, and
`remote.origin.git3DownloadConcurrency`. The exact camel-case mapping MUST be documented by the
implementation and covered by tests.

Byte quantities accept an unsigned integer plus `KiB`, `MiB`, `GiB`, or `TiB`; bare integers mean
bytes. Booleans accept only Git's standard boolean spellings. Invalid known settings are fatal.
Unknown `git3.*` settings SHOULD be ignored with a debug-level message for forward compatibility.

`partSize` MUST be at least the S3 minimum part size and large enough that the maximum supported
1 TiB object cannot exceed 10,000 parts. The reference default of 128 MiB satisfies that boundary.
Concurrency and byte settings MUST be bounded to prevent integer overflow or unbounded memory use.

### 10.3 Repository storage policy

On initialization, local `sse`, `kmsKeyId`, and `bucketKeyEnabled` settings become the remote
`storagePolicy`. Thereafter the published remote policy is authoritative for all managed object
writes:

- `inherit` serializes as `inherit`, sends no explicit encryption header, and uses the bucket default;
- `s3` serializes as `AES256` and sends the SSE-S3 algorithm; and
- `kms` serializes as `aws:kms` and sends SSE-KMS plus the optional configured key and bucket-key
  selection.

`storagePolicy.serverSideEncryption` MUST be exactly `inherit`, `AES256`, or `aws:kms`.
`kmsKeyId` MUST be null unless the algorithm is `aws:kms`; a null KMS key means the AWS-managed S3 KMS
key. `bucketKeyEnabled` MUST be null or a boolean and MUST be null unless the algorithm is `aws:kms`.

A writer with conflicting local encryption settings MUST fail with a clear configuration error; it
MUST NOT silently change the repository policy. A future administrative policy-change command is
outside format version 1. Reads MUST rely on S3 to decrypt and MUST NOT send write-only encryption
headers.

## 11. Git remote-helper contract

### 11.1 Invocation and stdout discipline

Git invokes `git-remote-s3 <remote-name-or-url> <s3-url>`. The helper MUST implement the current Git
remote-helper line protocol supported by Git 2.38. Protocol responses alone go to stdout. Progress,
warnings, diagnostics, and structured logs go to stderr.

The helper MUST advertise:

```text
fetch
push
option
check-connectivity
object-format

```

It MUST NOT advertise `connect`, `stateless-connect`, `import`, or `export`.

### 11.2 Options

The helper MUST implement these option results:

| Option | Required behavior |
| --- | --- |
| `verbosity` | Accept non-negative integers and adjust stderr detail. |
| `progress` | Accept booleans and enable or suppress progress. |
| `cloning` | Accept booleans and optimize empty-local-repository handling. |
| `check-connectivity` | Accept booleans and emit `connectivity-ok` only after verification. |
| `force` | Accept booleans; forced ref commands remain the authoritative per-ref signal. |
| `atomic` | Accept booleans and apply section 14.4. |
| `dry-run` | Accept booleans and perform all validation without remote writes. |
| `followtags` | Accept booleans; full-replica fetch already transfers required tag objects. |
| `object-format` | Accept `true`, `sha1`, or `sha256` according to Git's protocol. |

Requests for `depth`, `deepen-since`, `deepen-not`, `deepen-relative`, `update-shallow`,
`from-promisor`, or `no-dependents` MUST return an option error explaining that shallow and partial
operation is unsupported. The helper MUST fail rather than silently perform a full clone when the
user explicitly requested a storage-reducing mode. `pushcert`, arbitrary `push-option`, and unknown
options return `unsupported` unless a later specification defines them.

### 11.3 Ref advertisement

For an absent repository, `list` and `list for-push` MUST return an empty list and MUST NOT create S3
objects. This permits cloning an empty remote locator without initializing it.

For an initialized repository, the helper MUST:

1. read HEAD, using `If-None-Match` when a cached ETag exists;
2. validate HEAD and all required manifests;
3. reconstruct refs from the referenced snapshot plus transactions in generation order;
4. advertise direct refs sorted by name;
5. advertise `HEAD` as `@<headSymref>` when non-null; and
6. emit the object-format keyword when Git requested object-format negotiation.

An HTTP 304 response allows use of the cached ref map only when the cached map belongs to the same
repository ID, publication ID, and ETag and its local checksum validates. A missing cache after 304
is an internal error and MUST cause an unconditional reread.

### 11.4 Fetch command

A batch of `fetch <oid> <ref>` commands MUST make all requested advertised objects and their required
closure available in the local object database. git3 MAY materialize more than the requested refs;
the version 1 strategy is a complete replica of current reachable remote state.

When Git requests connectivity checking, the helper MUST output `connectivity-ok` only after all
current advertised tips pass local object connectivity. When a newly installed pack is protected by
a `.keep` file, the helper MUST emit the remote-helper `lock <path>` response where applicable so Git
owns removal after ref update.

### 11.5 Push command

The helper MUST buffer the complete push batch before performing remote writes. Each destination
receives one `ok <dst>` or `error <dst> <reason>` result as required by Git's protocol. The helper
MUST NOT print success before it has confirmed publication as defined in section 14.

## 12. Local repository and workspace contract

### 12.1 Layout

git3 MUST use the existing Git directory and MUST NOT create a standalone `.git3` directory. The
following example is for an ordinary working-tree repository:

```text
.git/
  objects/
    pack/
      pack-<git-checksum>.pack
      pack-<git-checksum>.idx
      pack-<git-checksum>.keep
      multi-pack-index
  git3/
    <repository-id>/
      cursor.json
      refs.snapshot
      head.etag
      state.lock
      downloads/
```

The local git3 directory contains only metadata, locks, and incomplete transfer files. It MUST NOT
contain a second complete copy of an installed packset. Temporary files MUST be on the same
filesystem as their atomic rename target.

All actual paths MUST be resolved through Git (for example, `git rev-parse --git-path`) rather than by
assuming a literal `.git` directory. In a bare repository, `git3/` is below the resolved Git directory;
an alternate object directory MUST likewise be honored for native pack installation.

### 12.2 Local cursor

`cursor.json` is canonical JSON containing:

```json
{
  "formatVersion": 1,
  "repositoryId": "73bcb050-8b53-4e47-a5a4-e661bd5c8faf",
  "objectFormat": "sha1",
  "logicalGeneration": 1843,
  "transactionId": "8af84623-3cd0-4077-bc74-a2402805189c",
  "lastPublicationId": "9d0a1c11-bf68-475c-8bfa-a019028d7e18",
  "lastHeadEtag": "opaque-etag-value",
  "cachedRefsSha256": "3333333333333333333333333333333333333333333333333333333333333333"
}
```

The cursor MUST NOT list packfiles. It asserts that the local object database was verified through a
logical point; it does not assert that local tracking refs were updated successfully afterward.

Local Git may repack, prune redundant packs, build MIDX files, or import objects from another remote.
Before using a cursor to skip data, git3 MUST verify that every object requested by Git exists. Before
advancing a cursor, it MUST verify the full current advertised-tip connectivity.

If the local cursor is missing, corrupt, divergent, ahead of the remote, or references a transaction
below the available log floor, git3 MUST discard the optimization and use the bootstrap path. If the
ordinary local Git database already contains the complete current advertised closure, git3 MAY seed
the cursor directly at the current transaction without downloading the packset.

### 12.3 Locking and atomic local updates

One process at a time may mutate a given local repository's git3 metadata or install packs for the
same repository ID. `state.lock` MUST be acquired atomically. Waiting MUST be bounded to 30 seconds by
default, after which the command fails with a local-lock error.

Cursor, cached refs, and ETag files MUST be written to a sibling temporary file, flushed, atomically
renamed, and followed by a best-effort parent-directory flush. A crash may lose the latest cursor but
MUST NOT create a cursor for unverified objects.

### 12.4 Installing compacted packs

A compacted pack is downloaded to:

```text
.git/objects/pack/pack-<git-checksum>.pack.part
.git/objects/pack/pack-<git-checksum>.idx.part
```

The downloader SHOULD use concurrent, non-overlapping S3 range reads into the final sparse temporary
file. It MUST verify expected length and SHA-256, verify the native pack trailer checksum, validate
the index against the pack with Git plumbing, and then atomically rename the pair into place. An
already present final pack/index pair MAY be reused only after validation. A partial, mismatched, or
unpaired final file MUST be quarantined or reported; it MUST NOT be silently overwritten.

### 12.5 Installing transaction packs

Transaction packs MUST stream from S3 through a SHA-256 verifier into an invocation equivalent to:

```text
git index-pack --stdin --fix-thin --strict --keep=git3-<operation-id>
```

Git, not git3, owns pack parsing, thin-pack completion, native indexing, and the final installed pack
name. The `.keep` protects data until Git has updated refs. The helper MUST clean stale keeps that it
owns after a failed operation, but MUST NOT remove an unknown process's keep file.

If thin-pack completion fails because a required base is missing, the logical cursor is invalid for
that operation. git3 MUST fall back to the current packset and reapply the bounded transaction tail;
it MUST NOT merely edit the cursor or claim the failed generation.

### 12.6 Local maintenance

git3 SHOULD run `git multi-pack-index write` after installing enough packs to make it beneficial and
MAY write a commit-graph. It MUST NOT perform an unbounded foreground full repack merely because the
user ran fetch or push. Ordinary Git maintenance remains valid and MUST NOT invalidate the logical
cursor solely by changing pack identity.

## 13. Repository initialization

### 13.1 Preconditions

An absent repository is defined only by a 404 response for `.git/git3/HEAD`. A 403 response is an
authorization failure and MUST NOT be treated as absence. The bucket MUST exist; git3 MUST NOT call a
bucket-creation API.

Only a push may initialize. The initial accepted batch MUST contain at least one non-deletion update
to `refs/heads/*`. The local object format becomes the permanent remote `objectFormat`.

Default branch selection is deterministic:

1. If exactly one branch is created, it becomes `headSymref`.
2. If several branches are created, the destination derived from the local symbolic `HEAD` MUST
   become `headSymref`.
3. If that mapping is unavailable or ambiguous, initialization MUST fail before publication. git3
   MUST NOT guess based on names such as `main` or `master`.

### 13.2 Initialization algorithm

The initializing writer MUST:

1. validate the complete push batch and local object connectivity;
2. generate a repository UUID, transaction UUID, and generation-one transaction;
3. create a generation-zero empty ref snapshot and empty packset for that repository UUID;
4. stream and upload generation-one object data, if any;
5. upload the immutable transaction descriptor;
6. create a log page when the transaction cannot fit within the HEAD tail limits;
7. assemble HEAD with generation `1`, manifest revision `1`, the selected default branch, the chosen
   storage policy, generation-zero snapshot and packset, and the transaction in its log;
8. validate the complete prospective state locally; and
9. create HEAD using `If-None-Match: *`.

All preparatory immutable writes are harmless orphans until step 9. If step 9 loses an initialization
race, the client MUST read and validate the winner and fail the push as a conflict; it MUST NOT
silently combine two independently initialized histories.

If HEAD exists but is corrupt, has an unsupported format, or identifies a different object format,
initialization MUST fail without overwriting it. Existing unreferenced objects below `.git/git3/` do
not establish a repository and are ignored unless a generated immutable key collides with different
content.

## 14. Push workflow and concurrency

### 14.1 Pinning and validation

`list for-push` pins the parsed HEAD, its ETag, publication ID, transaction tip, and reconstructed ref
map for the helper process. Before upload, the helper MUST validate each command:

- the destination is a valid direct ref under `refs/` and is not `HEAD`;
- no destination appears twice;
- a non-delete source resolves to one object in the local object format;
- all required local objects are readable;
- the transaction's `old` value equals the pinned advertised value;
- creation requires an absent old value;
- deletion requires a present old value;
- a normal update to `refs/heads/*` is a fast-forward according to `git merge-base --is-ancestor`;
- replacement of an existing tag or other non-branch ref requires force; and
- any non-fast-forward branch update requires force.

An unchanged old/new pair is a per-ref success but not an effective update. A batch with no effective
updates MUST return success without advancing the generation or writing S3 objects.

### 14.2 Transaction pack generation

For the accepted non-delete tips, the helper MUST invoke native Git plumbing equivalent to:

```text
git pack-objects --revs --thin --stdout
```

Positive revisions are the accepted new tips. Negative revisions MAY include any pinned remote tips
that are present locally; they MUST NOT include an object the receiver is not guaranteed to possess
at the pinned parent transaction. A missing remote tip locally is omitted from the negative set,
which may send more data but remains correct.

The stream MUST feed an S3 upload without first spooling the complete thin pack. While streaming,
git3 MUST compute content length, SHA-256, and the native pack checksum. Objects at or above the
multipart threshold SHOULD use multipart upload. Parts MUST be numbered consecutively from one,
checksummed, and retried independently. Cancellation or generation failure MUST trigger a best-effort
multipart abort.

Transaction pack keys use the transaction UUID because the final digest is unknown when streaming
begins. The key MUST be created conditionally. After the pack completes, the descriptor and any log
page are uploaded before HEAD.

### 14.3 Publication

For an existing repository, the new HEAD MUST be written with `If-Match: <pinned-etag>`. The
prospective state has:

- logical generation `parent + 1`;
- the new transaction ID;
- manifest revision `parent + 1`;
- a new publication ID;
- the same packset, ref snapshot, storage policy, and GC barrier;
- the new transaction appended to the log tail or a newly sealed page; and
- the current `headSymref`, except that deleting its target in the accepted transaction MUST set it
  to null.

git3 MUST NOT guess a replacement default branch after deletion. A later `git3 set-head` command may
select an existing branch.

Only a successful conditional HEAD replacement publishes the transaction. The helper MUST report
success only after either receiving the successful write response or rereading a later valid HEAD
whose canonical transaction chain contains the exact new transaction at its intended generation.

### 14.4 Partial and atomic batches

Without `option atomic true`, independently invalid refs MAY be rejected while the remaining accepted
updates publish together in one transaction. All accepted updates are still atomic with respect to
one another.

With `option atomic true`, any invalid ref MUST reject the entire batch before object upload. A CAS
conflict or publication ambiguity also fails every ref in an atomic batch. Because repository meaning
always changes through one HEAD replacement, a valid atomic batch cannot be partially published.

### 14.5 Concurrent pushes and force-with-lease

Two writers may upload immutable objects concurrently, but only one writer based on a given HEAD
ETag may publish. A precondition failure is a logical conflict, not a retryable transport error.

After a conflict, git3 MAY refresh and retry a batch of normal fast-forward creations or updates no
more than two times, but only after reconstructing the new ref map and revalidating every old value,
fast-forward relation, and atomicity condition. A forced update or deletion MUST fail on the first CAS
conflict so that the user explicitly refreshes the lease. No conflict path may blindly replace HEAD.

Git's frontend compares an explicit or implicit `--force-with-lease` expectation with the advertised
ref before issuing a forced helper command. git3 strengthens the race boundary by publishing only
against the exact HEAD ETag used for that advertisement. It does not claim to distinguish plain
`--force` from `--force-with-lease` on the remote-helper command stream.

### 14.6 Log tail sealing

Before publication, the writer evaluates `current tail + new transaction`. If the result exceeds 32
records, 1 MiB of canonical transaction-envelope bytes, or the 2 MiB HEAD maximum, it MUST:

1. create one immutable page containing the proposed consecutive records;
2. link that page to the previous tip page;
3. set HEAD's `tipPage` to the new page; and
4. publish an empty tail.

Otherwise it appends the envelope directly to the tail. Page creation is preparatory and a losing CAS
leaves only an unreachable immutable page.

## 15. Fetch and clone workflow

### 15.1 Fetch-path selection

After ref advertisement, git3 selects exactly one of these paths:

1. **No-op:** The local cursor matches the current generation and transaction ID, and all requested
   objects exist locally.
2. **Incremental:** The cursor is on the canonical chain at or above the current log floor and the
   required thin-pack bases are available.
3. **Bootstrap:** No usable cursor exists, the cursor is below the log floor or divergent, local data
   is incomplete, or incremental thin-pack application fails.
4. **Cursor seeding:** No usable cursor exists, but current advertised-tip connectivity already passes
   in the local Git object database.

The selected path is an optimization only. Every successful path has the same final object and
connectivity contract.

### 15.2 No-op fetch

Ref advertisement SHOULD issue:

```text
GET .git/git3/HEAD
If-None-Match: <cached-etag>
```

When S3 reports not modified and requested objects exist locally, git3 MUST perform no object-data
GET and no material local write. The target cost is one conditional GET, zero downloaded Git-object
bytes, and zero pack changes.

### 15.3 Incremental fetch

For a cursor at generation `G` and remote generation `H`, where `floor <= G < H`, git3 MUST:

1. prove that `(G, transactionId)` is on the current canonical chain;
2. collect envelopes `G+1` through `H` from the HEAD tail and as few log pages as necessary;
3. validate page hashes, consecutive generations, transaction IDs, and every parent link;
4. apply transaction packs in generation order, skipping descriptors with null object data;
5. verify every current advertised tip and its reachable closure; and
6. atomically advance the cursor and cached refs to `H`.

If required log data returns 404, fails a checksum, has a gap, or refers below the floor, the helper
MUST attempt bootstrap. Corruption of data that is also required by the current bootstrap state is a
fatal remote-integrity error.

### 15.4 Bootstrap clone or fetch

The bootstrap path MUST:

1. read and validate the current HEAD;
2. read and validate the referenced ref snapshot and packset manifest;
3. download or validate each pack/index pair in ascending level order;
4. atomically install those native pairs in `.git/objects/pack`;
5. apply all transactions after the packset generation in order;
6. reconstruct current refs independently from the ref snapshot and subsequent transactions;
7. verify all advertised-tip connectivity; and
8. atomically write local cursor and cached-ref metadata.

If HEAD changes while immutable data is being downloaded, the client MAY finish against its pinned
state and then incrementally catch up. It MUST NOT mix manifests from different HEAD reads without
validating the transaction ancestry between them.

Pack/index pairs MAY download concurrently. Transaction packs MUST be applied in generation order.
Downloaded payload is approximately the current packset plus the uncompacted transaction tail, not a
sequence of historical monolithic repository checkpoints.

### 15.5 Divergence, rollback, and local pruning

Generation numbers alone do not prove ancestry. A matching generation with a different transaction
ID is divergent and MUST invalidate the cursor. A lower remote generation may indicate external S3
version restoration or unauthorized mutation; git3 MUST warn, avoid deleting local objects, and use
bootstrap or cursor seeding after verifying the restored state.

Local `git gc`, repack, or pruning does not invalidate a cursor automatically. Missing requested
objects do. The helper MUST detect the missing data and repair through bootstrap rather than trusting
the cursor file.

## 16. Remote maintenance and geometric compaction

### 16.1 Operational model

S3 does not execute Git. `git3 maintenance` MUST run on a user-controlled workstation or CI runner
inside a complete local clone of the target repository. The command MUST refuse to compact from a
shallow, partial, corrupt, or incomplete local object database.

The transaction-count and WAL-byte thresholds mark maintenance as due; they are not remote-format
constants. Normal push MUST remain correct when maintenance is overdue. The logarithmic cold-clone
pack-count and bounded-WAL scaling targets assume that operators run maintenance with reasonable
frequency. `doctor` and successful pushes SHOULD warn when either threshold is exceeded.

### 16.2 Pack construction

Transaction data after the current packset generation is compacted into one new level-zero native,
non-thin pack. The object set MUST contain the objects needed to move from the packset-generation ref
state to the selected target-generation ref state. The implementation SHOULD derive this with Git
revision plumbing and MUST validate closure against the full packset after construction.

Existing levels use size-tiered promotion with configurable fanout `B`, default 4:

1. Add a new pack to level zero.
2. When a level contains `B` or more packs, merge all packs in that level into one pack in the next
   level.
3. Repeat promotion until every level contains fewer than `B` packs.
4. Physical maintenance MAY merge a smaller selection when measured sizes are severely imbalanced,
   but the resulting manifest MUST preserve complete closure.

Selected remote packs MUST be present and verified locally. If local Git previously repacked them
away, the maintainer MAY redownload them. The reference implementation SHOULD use
`git pack-objects --stdin-packs` or equivalent Git plumbing to merge selected native packs and avoid
implementing pack parsing.

The new pack and index are first created within `.git/objects/pack`, fully verified, and then uploaded
under content-derived immutable keys. Compaction MAY require temporary disk approximately equal to
the selected merge, but MUST NOT require a second complete remote cache or rewrite older unselected
levels.

### 16.3 Maintenance publication

A maintenance operation MUST:

1. read and pin HEAD and its ETag;
2. acquire the local state lock and verify local completeness at the pinned transaction;
3. fail if an active GC barrier lists any object it intends to reference;
4. select and construct at most one bounded geometric merge unless `--all` was explicitly supplied;
5. upload new pack/index pairs conditionally and verify them;
6. write a new immutable packset manifest;
7. write a full sorted ref snapshot at the current logical generation;
8. assemble a HEAD that preserves logical generation, transaction ID, default branch, storage policy,
   and GC barrier, advances manifest revision and publication ID, references the new physical state,
   and advances the log floor no further than the packset generation;
9. locally validate the prospective bootstrap path; and
10. publish with `If-Match: <pinned-etag>`.

When compaction includes the full current transaction tail, the target generation SHOULD be the
current logical generation, the log floor advances to it, and the new HEAD has no live log page or
tail records. Physical-only merges at the existing floor preserve that floor.

When a bounded run stops at an intermediate target, HEAD MUST retain exactly the transactions after
that target. It may do so by filtering the mutable tail and by retaining page records above the new
floor; records and page pointers at or below the floor have no live semantics. The resulting chain
MUST still satisfy the consecutive-generation invariant before publication.

A CAS conflict MUST abort maintenance publication. The implementation MUST NOT automatically adapt
a packset to a newer logical transaction. Uploaded immutable results remain unreferenced and are
eligible for a future operator-directed GC.

### 16.4 Remote write amplification

With fanout `B` and reasonably sized level-zero inputs, an object SHOULD be rewritten only when enough
newer data accumulates to promote its level. The intended amplification is logarithmic in repository
growth, not proportional to current repository size times checkpoint count. Force-push patterns,
unreachable objects, delta selection, and operator cadence make this a target rather than a strict
per-object guarantee.

## 17. Ref snapshots and default branch administration

Maintenance MUST publish a ref snapshot whenever it advances the log floor. It MAY publish a newer
snapshot without changing the packset or floor when ref replay becomes expensive. Such a physical
publication preserves logical generation and transaction ID while advancing manifest revision.

`git3 set-head <remote-or-url> <refs/heads/name>` MUST:

1. read and pin HEAD;
2. reconstruct the current ref map;
3. require the target branch to exist;
4. preserve logical generation, transaction ID, log, packset, snapshot, storage policy, and barrier;
5. advance manifest revision and publication ID; and
6. replace HEAD conditionally.

Changing the symbolic default branch is administrative metadata, not a logical ref transaction. A
CAS conflict fails and requires an explicit retry.

## 18. Remote garbage collection

### 18.1 Scope and retention boundary

git3 defines safe discovery and deletion mechanics but does not choose a retention period. Bucket
versioning, lifecycle rules, legal holds, Object Lock, recovery windows, and the age at which an
unreferenced object may be removed are operator policy and outside this specification.

`git3 gc` MUST be a dry run unless `--execute` is present. Execution MUST additionally require an
explicit `--older-than <duration-or-RFC3339-time>` supplied by the operator; git3 MUST have no hidden
or default retention cutoff.

### 18.2 Mark set

GC is the only workflow allowed to use S3 `LIST`. It MUST list only the exact reserved prefix and MUST
never inspect or delete objects outside it. Starting from one pinned HEAD, the live mark set includes:

- the HEAD key itself;
- the current ref snapshot;
- the current packset manifest and every referenced pack/index;
- log pages and transaction envelopes strictly above the log floor;
- referenced transaction descriptors and WAL packs;
- an active GC plan; and
- any future object reachable through a supported required feature.

Historical `previous` page pointers below `floorGeneration` do not mark below-floor data. A candidate
is a listed immutable object absent from the live mark set, older than the operator cutoff, and not a
probe or in-progress multipart upload owned by another live process.

Dry-run output MUST include repository ID, pinned publication ID and ETag, candidate key, size, ETag,
last-modified time, category, and total counts/bytes. It MUST make no S3 writes or deletes.

### 18.3 GC plan and barrier

A mutating GC run MUST prevent a concurrent conforming publisher from reviving a candidate between
marking and deletion. It therefore uses a published barrier:

1. Recompute the mark and candidate sets against a fresh HEAD.
2. Write an immutable canonical plan to `.git/git3/gc/<plan-id>.json`. The plan contains repository
   ID, source publication ID and ETag, operator cutoff, and the exact sorted candidate keys, sizes,
   ETags, and last-modified values.
3. CAS-replace HEAD with a non-null `gcBarrier` referencing the plan, while preserving logical state
   and advancing manifest revision/publication ID.
4. Re-read HEAD and the plan immediately before deletion and recompute current live reachability.
5. Abort if the barrier changed, if any candidate became live, or if any candidate metadata differs.
6. Delete each candidate conditionally using its current ETag.
7. Record per-key outcomes, then CAS-clear the barrier and advance manifest revision/publication ID.

`gcBarrier` has this shape:

```json
{
  "planId": "19b90f2c-1618-47e8-9f4b-1879b46c8434",
  "createdAt": "2026-08-29T16:02:00Z",
  "plan": {
    "key": ".git/git3/gc/19b90f2c-1618-47e8-9f4b-1879b46c8434.json",
    "size": 8831,
    "sha256": "4444444444444444444444444444444444444444444444444444444444444444"
  }
}
```

Every publisher that reads a non-null barrier MUST preserve it and MUST NOT introduce a reference to
a listed candidate. Pushes normally create UUID-addressed objects and MAY continue. Compaction or
another GC operation SHOULD fail with `GC_BARRIER_ACTIVE` rather than wait indefinitely. A writer
that read HEAD before barrier publication loses its HEAD CAS and cannot publish stale references.

### 18.4 Interrupted GC

If GC stops after barrier publication, the barrier remains visible. `git3 gc --resume <plan-id>` MUST
revalidate and continue safely. `git3 gc --abort <plan-id>` MAY clear the barrier by CAS after proving
that the plan ID matches; already deleted unreferenced objects remain deleted. Neither command may
infer success from process-local state alone.

A failed conditional delete, changed HEAD, changed ETag, 409, 412, or ambiguous response MUST be
reported and left for a later revalidated run. GC MUST never convert an error into unconditional
deletion.

## 19. Administrative CLI

The following commands are core:

```text
git3 version
git3 doctor <remote-or-s3-url> [--json] [--write-test]
git3 fsck <remote-or-s3-url> [--full] [--json]
git3 maintenance <remote-or-s3-url> [--max-bytes <n>] [--all]
git3 gc <remote-or-s3-url> [--json]
git3 gc <remote-or-s3-url> --execute --older-than <value>
git3 gc <remote-or-s3-url> --resume <plan-id>
git3 gc <remote-or-s3-url> --abort <plan-id>
git3 set-head <remote-or-s3-url> <refs/heads/name>
```

The same subcommands MUST be available as `git s3 ...`. Commands taking a remote name resolve its URL
through Git config; commands taking an S3 URL use it directly.

### 19.1 Doctor

`doctor` is read-only by default and MUST report:

- tool, Go runtime, and Git versions;
- URL normalization and effective non-secret configuration;
- credential-provider success without printing credential values;
- bucket/region and endpoint reachability;
- repository absence, identity, format, generation, revision, default branch, and barrier state;
- HEAD, snapshot, packset, and log structural validity;
- local cursor status when run inside a Git repository;
- missing required Git plumbing features;
- maintenance-due thresholds; and
- minimum IAM actions implied by the requested operation.

`--write-test` MAY create and conditionally delete a unique object below `.git/git3/probes/`; it MUST
announce the mutation, use no repository manifests, and clean up best-effort. It MUST NOT alter HEAD.

### 19.2 Fsck

Default `fsck` MUST validate the complete manifest graph and the size/checksum metadata available
without downloading all pack payloads. `--full` MUST additionally verify every current pack/index,
apply or inspect every live transaction pack in an isolated or already complete local object
database, and prove current ref connectivity. `fsck` MUST be read-only.

### 19.3 Maintenance and GC output

Mutating administrative commands MUST print a prospective work summary before their first remote
write when attached to a terminal. `--json` output MUST be machine-readable and contain stable error
codes. `gc --execute` MUST display the exact cutoff and candidate totals before barrier publication.

## 20. S3 integration contract

### 20.1 Required request behavior

AWS S3 general purpose buckets are the normative store. git3 relies on strong read-after-write
consistency and atomic single-key updates. Required operations are:

- `GetObject` with conditional and range headers;
- `PutObject` with `If-Match` or `If-None-Match`;
- multipart create/upload/complete/abort for large immutable data;
- `HeadObject` where metadata validation avoids payload transfer;
- `ListObjectsV2` only for GC; and
- conditional `DeleteObject` or `DeleteObjects` only for GC.

HEAD is small and MUST use a single `PutObject`, never multipart upload. Conditional create returns
412 when the key exists. Conditional replace returns 412 when ETags differ. 409, 404 during a
conditional mutation, and a transport timeout after request transmission MUST be treated as
potential races or ambiguous outcomes and resolved by a fresh read, not by an unconditional write.

### 20.2 Integrity

Every upload MUST request an S3-supported transport checksum through the current SDK. git3's own
SHA-256 in manifests remains REQUIRED and is independent of ETag and multipart checksum shape.

Every download MUST verify byte length and SHA-256. Pack downloads additionally verify native Git
checksums and structure. Metadata checksum failure is fatal; it MUST NOT be retried indefinitely or
silently accepted from a different key.

### 20.3 Retry and timeout policy

Retryable reads, immutable creates, range parts, and multipart parts use exponential backoff with full
jitter, a 200 ms base, a 20 second cap, and at most `maxAttempts` attempts, default five. SDK behavior
MAY implement an equivalent bounded policy.

The following are not ordinary retries:

- HEAD 412: logical CAS conflict;
- immutable-object 412: read and compare exact content;
- 401/403: authentication or authorization failure;
- unsupported API or malformed response: endpoint incompatibility;
- checksum mismatch: integrity failure; and
- disk-full or Git-plumbing failure: local failure.

No retry loop may be unbounded. A Ctrl-C or termination signal MUST cancel child Git processes and
network requests, attempt to abort owned multipart uploads, preserve completed immutable objects, and
exit nonzero.

### 20.4 Minimum IAM shapes

Normal read requires `s3:GetObject` on `<prefix>/.git/git3/*`. Normal push and maintenance additionally
require `s3:PutObject` and, for multipart cleanup, `s3:AbortMultipartUpload`. Conditional writes
require the caller to have both get and put access to the target key.

GC additionally requires prefix-scoped `s3:ListBucket`, `s3:DeleteObject`, and `s3:GetObject` for
conditional deletion. SSE-KMS requires the applicable KMS encrypt, decrypt, data-key, and key-policy
permissions. The project documentation MUST provide least-privilege example policies separated into
reader, writer/maintainer, and GC-operator roles.

Normal Git operations MUST NOT require `s3:ListBucket` or `s3:DeleteObject`.

## 21. Failure model and crash consistency

### 21.1 Stable error categories

The engine MUST normalize failures into these stable categories:

| Code | Meaning | Retry posture |
| --- | --- | --- |
| `CONFIG_INVALID` | Invalid URL, option, or incompatible local configuration. | Fix input. |
| `AUTH_FAILED` | Credentials absent, expired, or unauthorized. | Refresh credentials/policy. |
| `BUCKET_NOT_FOUND` | The required bucket does not exist. | Create/configure bucket externally. |
| `REPOSITORY_CORRUPT` | HEAD or a required manifest violates schema or invariants. | Run fsck; no writes. |
| `FORMAT_UNSUPPORTED` | Remote/Git format or required feature is unsupported. | Upgrade or use compatible client. |
| `CAS_CONFLICT` | Another publisher replaced HEAD. | Refresh and revalidate. |
| `GC_BARRIER_ACTIVE` | Maintenance conflicts with an active GC plan. | Resume/finish/abort GC. |
| `INTEGRITY_FAILED` | Size, checksum, pack, index, or connectivity validation failed. | Do not trust payload. |
| `NETWORK_EXHAUSTED` | Bounded retry policy ended. | Retry command later. |
| `PUBLISH_AMBIGUOUS` | Publication could not be confirmed or disproved. | Run doctor/read HEAD. |
| `LOCAL_GIT_FAILED` | Git plumbing rejected input or failed. | Inspect stderr/local repo. |
| `LOCAL_RESOURCE_FAILED` | Lock, memory limit, or disk-space failure. | Free resource/retry. |
| `CANCELLED` | User or supervisor cancelled operation. | Safe to rerun. |

Administrative CLI exit codes are: `0` success, `1` general failure, `2` usage/config, `3` auth or
bucket, `4` corrupt/unsupported, `5` conflict/barrier, `6` integrity, `7` local resource/Git, and `8`
ambiguous publication. The remote helper may use any nonzero process code required by Git but MUST
preserve the stable code in stderr/JSON diagnostics.

### 21.2 Push crash matrix

| Last completed step | Remote meaning | Required recovery |
| --- | --- | --- |
| No immutable upload | Unchanged | Abort owned multipart work. |
| WAL pack only | Unchanged; orphan pack | Safe retry with same transaction ID/content or new attempt. |
| Descriptor only after pack | Unchanged; orphan pair | Safe retry; future GC may collect. |
| Log page after descriptor | Unchanged; orphan graph | Safe retry; future GC may collect. |
| HEAD CAS returns 412 | Unchanged by this writer | Refresh and revalidate or fail. |
| HEAD CAS succeeds | Transaction published | Report success. |
| HEAD response lost | Unknown | Reread; find exact transaction in canonical chain. |
| Later writer already advanced HEAD | Published only if chain contains exact transaction | Traverse from current tip to intended generation. |

A visible ref pointing to absent object data is a protocol violation. An unreachable immutable object
is not.

### 21.3 Fetch crash matrix

- A partial `.part` file has no Git meaning and MAY be resumed only after validating remote key,
  expected ETag/size, and completed byte ranges.
- A completed pack without a validated index MUST NOT be made final.
- A pack installed with `.keep` but before cursor update is safe and may be reused after validation.
- A cursor not updated after successful installation causes redundant work but no corruption.
- A cursor updated before connectivity validation is forbidden.

### 21.4 Maintenance and GC crash behavior

Maintenance immutable uploads before HEAD CAS are orphans. A successful maintenance CAS changes only
physical bootstrap state and must preserve logical state. An ambiguous maintenance CAS is resolved by
matching the new `publicationId` or packset ID in a fresh HEAD.

GC is resumable through its published barrier. The barrier, plan, conditional per-object deletes, and
final barrier-clear CAS are the remote recovery record; process-local state is never authoritative.

## 22. Security and operational safety

1. All AWS traffic MUST use TLS. An HTTP custom endpoint requires explicit
   `allowInsecureEndpoint=true`, MUST emit a warning, and SHOULD be limited to loopback or test use.
2. The reference implementation MUST use the AWS SDK default credential chain, including environment,
   shared profiles, IAM Identity Center, web identity, and compute roles as supported by that SDK.
3. Static credentials MUST NOT be accepted in `s3://` URLs or git3-specific config.
4. Secrets, authorization headers, session tokens, KMS plaintext, and signed request query strings
   MUST be redacted from all output.
5. Manifest keys MUST be containment-checked before filesystem or S3 use. Remote data may not choose
   an arbitrary local path or command argument outside the expected grammar.
6. Git plumbing MUST be executed without a shell, with explicit argv, controlled stdin, inherited
   Git directory, and cancellation.
7. Temporary files MUST use restrictive permissions and unpredictable names. Installed executables
   MUST not be group/world writable.
8. Ref names, object counts, JSON sizes, recursion depth, page traversal, and manifest fanout MUST
   have documented resource bounds. A repeated page ID or parent transaction is corruption, not a
   loop to follow.
9. HEAD and manifest readers MUST cap response size before allocating. The HEAD cap is 2 MiB;
   implementation-defined larger caps for snapshots/pages/manifests MUST support 100,000 refs while
   preventing unbounded allocation.
10. Bucket versioning, lifecycle, Object Lock, replication, CloudTrail, and data retention are
    operator choices. git3 MUST function without relying on them.
11. Server-side encryption defaults to the bucket policy. SSE-S3 and SSE-KMS are supported as defined
    in section 10; client-side encryption is not implied.
12. Direct S3 write access is equivalent to repository-administrator trust. Documentation MUST state
    this prominently and MUST NOT market client-side branch rules as an authorization boundary.

## 23. Observability

### 23.1 Human output

Progress goes to stderr and SHOULD include operation, phase, bytes transferred, total when known,
rate, retry notice, and compaction level without obscuring Git's own final result. Quiet mode emits
only warnings and errors.

### 23.2 Structured logs

JSON logs MUST emit one object per line with, where applicable:

```text
timestamp, level, code, message, operation, operationId,
repositoryId, logicalGeneration, transactionId, publicationId,
objectCategory, bytes, durationMs, attempt, awsRequestId, git3Version
```

Bucket and prefix MAY be logged only in an explicitly verbose mode and MUST be sanitized. Credential
provider names MAY be logged; credential values MUST NOT. stdout remains reserved for remote-helper
or requested command JSON output.

### 23.3 Command summaries and auditability

Every mutating command MUST conclude with the confirmed publication ID or state that publication was
not confirmed. Transactions provide an immutable logical audit trail while retained; AWS CloudTrail
is the recommended external identity-level audit trail. git3 itself does not infer an AWS principal
identity or persist host/user identity in transaction records.

The project has no daemon and therefore exposes no mandatory metrics endpoint. CI and operators MAY
derive metrics from stable JSON summaries.

## 24. Performance and scaling contract

With maintenance performed at the documented thresholds, the target asymptotic behavior is:

| Operation/state | Target |
| --- | --- |
| Mutable S3 state | Bounded independently of history; HEAD at most 2 MiB. |
| Per-push metadata | `O(number of updated refs)`. |
| Per-push data | Approximately new compressed reachable objects not known at the pinned parent. |
| No-op fetch | One conditional HEAD GET; zero object bytes. |
| Active fetch | Transactions and bytes after the verified cursor. |
| Cold clone bytes | Current reachable packset plus bounded WAL tail. |
| Cold clone pack requests | `O(log repository size + bounded tail)` under geometric maintenance. |
| Remote write amplification | Approximately logarithmic under geometric promotion. |
| Local steady-state disk | Ordinary Git object database plus small metadata and bounded temporary work. |

The 1 TiB/100,000-ref design envelope requires:

- all byte counts and offsets use checked 64-bit arithmetic;
- pack upload and checksum computation stream rather than buffer full objects;
- large pack downloads use bounded range concurrency;
- ref snapshots may use memory proportional to ref count but not repository object bytes;
- transaction pages prevent metadata GET count from equaling historical push count; and
- compaction work is explicitly budgetable and never hidden inside an unbounded foreground fetch.

The single HEAD CAS deliberately serializes logical pushes. A future coordinator may batch writes but
is an extension and may not change version 1 transaction semantics.

## 25. Release, installation, and open-source repository

### 25.1 Repository requirements

The public GitHub repository MUST contain at least:

```text
.github/
  workflows/ci.yml
  workflows/release.yml
cmd/git3/
internal/
docs/
test/
go.mod
go.sum
LICENSE
NOTICE
README.md
SECURITY.md
CONTRIBUTING.md
CODE_OF_CONDUCT.md
CHANGELOG.md
install.sh
SPEC.md
```

The reference implementation MUST use Go and the Apache License 2.0. The module path and GitHub owner
are deployment-defined project metadata; examples use `<owner>/git3`. Public API stability is not
required for `internal/` Go packages, but remote format version 1 and the administrative JSON output
contracts require explicit compatibility review before change.

### 25.2 Versioning

Project releases use semantic versions and signed Git tags of the form `vMAJOR.MINOR.PATCH`.

- Patch releases preserve remote format and CLI compatibility.
- Minor releases may add optional fields, commands, and features that old readers can ignore.
- A new required remote behavior MUST use a recognized `requiredFeatures` value or a new format
  version and MUST fail safely on old clients.
- Major releases may change CLI contracts but MUST provide an explicit remote-format migration plan
  before writing an incompatible HEAD.

The binary MUST embed version, source commit, build time or reproducible-source epoch, and dirty-state
marker. Official release binaries MUST report `dirty=false`.

### 25.3 Continuous integration

Every pull request and main-branch push MUST run:

- formatting verification;
- `go vet` and `staticcheck`;
- unit and property/state-machine tests;
- race-enabled tests on supported host platforms where practical;
- remote-helper protocol fixtures against the minimum and current Git versions;
- fault-injection tests with an instrumented S3 fake;
- S3-compatible smoke tests for the optional endpoint layer;
- installer shell lint and platform-selection tests;
- license and dependency-policy checks; and
- `govulncheck` plus dependency-review scanning for Go modules and the built release inputs.

AWS-specific integration tests MUST run against a real, isolated, general purpose S3 bucket before an
official release. GitHub Actions SHOULD obtain short-lived AWS credentials using OIDC rather than
long-lived repository secrets. Test prefixes MUST be unique and cleanup MUST be scoped to those exact
prefixes.

### 25.4 Release workflow

Pushing a valid version tag from the protected release process triggers one GitHub Actions workflow
that MUST:

1. check out the exact tag with full provenance context;
2. rerun the release test gate;
3. build with `CGO_ENABLED=0`, `-trimpath`, deterministic module inputs, and explicit version
   metadata for:
   - `linux/amd64`,
   - `linux/arm64`,
   - `darwin/amd64`, and
   - `darwin/arm64`;
4. package one `git3` executable plus license and notice in each tarball;
5. publish asset names of the form `git3_<version>_<os>_<arch>.tar.gz`;
6. produce a sorted `checksums.txt` with SHA-256 for every distributable asset;
7. produce a keyless Sigstore signature/bundle for the checksum file and release assets;
8. generate GitHub artifact attestations establishing build provenance;
9. attach the release-specific `install.sh`, checksums, signatures, bundles, and attestations to a
   GitHub Release; and
10. fail without publishing a partial final release if any matrix build, checksum, signature,
    attestation, or smoke test fails.

Workflow permissions MUST be least privilege. Source checkout and third-party actions MUST be pinned
to immutable commits or trusted major-version policies documented by the project. Release jobs MUST
use a protected GitHub environment when human approval is required.

### 25.5 Installer contract

The documented convenience commands are:

```bash
curl -fsSL https://github.com/<owner>/git3/releases/latest/download/install.sh | sh
```

and:

```bash
wget -qO- https://github.com/<owner>/git3/releases/latest/download/install.sh | sh
```

The release-specific installer MUST:

1. run under a POSIX shell with `set -eu` and a restrictive temporary directory;
2. accept `GIT3_VERSION` to pin a semantic version and otherwise resolve the latest release;
3. detect only supported OS/architecture pairs and fail clearly on all others;
4. download the matching tarball and `checksums.txt` over HTTPS from the same resolved release;
5. verify the tarball SHA-256 with `sha256sum` or `shasum -a 256` before extraction;
6. extract only expected flat filenames and reject absolute paths, `..`, devices, links, or extra
   executable payloads in the archive;
7. install by default into `${GIT3_INSTALL_DIR:-$HOME/.local/bin}` without `sudo`;
8. atomically place `git3` and create `git-s3` and `git-remote-s3` relative symlinks, falling back to
   verified copies;
9. never edit shell startup files or global Git configuration;
10. warn and print exact instructions when the install directory is absent from `PATH`;
11. print installed version and provenance-verification instructions; and
12. clean temporary data on success, failure, or signal.

The installer checksum protects against transfer corruption but is delivered through the same
GitHub trust boundary. Documentation MUST provide an independent verification path using the
published Sigstore bundle and GitHub artifact attestation before execution or installation. A
version-pinned manual download is the recommended high-assurance path.

## 26. Reference algorithms

### 26.1 Reconstruct current refs

```text
function reconstruct_refs(head):
    validate(head)
    snapshot = get_and_verify(head.refSnapshot.object)
    refs = parse_snapshot(snapshot)
    assert snapshot.generation == head.refSnapshot.generation

    txs = collect_transactions(
        after_generation = snapshot.generation,
        through_generation = head.logicalGeneration,
        head.log
    )

    expected_generation = snapshot.generation + 1
    expected_parent_id = snapshot.transactionId
    for tx in txs oldest_to_newest:
        assert tx.generation == expected_generation
        assert tx.parentTransactionId == expected_parent_id
        for update in tx.updates:
            assert refs.get(update.ref) == update.old
            if update.new is null:
                delete refs[update.ref]
            else:
                refs[update.ref] = update.new
        expected_generation += 1
        expected_parent_id = tx.transactionId

    assert expected_generation - 1 == head.logicalGeneration
    assert expected_parent_id == head.transactionId
    assert head.headSymref is null or head.headSymref in refs
    return refs
```

### 26.2 Publish a push

```text
function publish_push(batch, atomic):
    parent, etag, refs = pin_head_and_refs()
    results = validate_updates(batch, refs)

    if atomic and any result rejected:
        return reject_all(results)
    accepted = effective_accepted_updates(results)
    if accepted is empty:
        return report(results)

    tx_id = random_uuid()
    wal = stream_thin_pack(accepted, locally_present_remote_bases(refs))
    if wal.non_empty:
        immutable_put(wal.key(tx_id), wal.bytes, if_none_match="*")

    tx = build_transaction(parent, tx_id, accepted, wal.metadata_or_null)
    tx_ref = immutable_put(transaction_key(tx), canonical(tx), if_none_match="*")
    envelope = { descriptor: tx_ref, transaction: tx }

    candidate = append_or_seal(parent, envelope)
    candidate.logicalGeneration = parent.logicalGeneration + 1
    candidate.transactionId = tx_id
    candidate.manifestRevision = parent.manifestRevision + 1
    candidate.publicationId = random_uuid()
    validate_complete_state(candidate)

    result = conditional_put_HEAD(candidate, if_match=etag)
    if result.success:
        return report_success(results)
    if result.precondition_failed:
        return refresh_revalidate_or_conflict(batch, results)

    observed = bounded_reread_HEAD()
    if canonical_chain_contains(observed, tx):
        return report_success(results)
    if publication_definitively_absent(observed, tx):
        return report_failure(results)
    return PUBLISH_AMBIGUOUS
```

### 26.3 Fetch with fallback

```text
function fetch(requested):
    head, etag = conditional_or_full_read_HEAD()
    refs = reconstruct_refs(head)

    if cursor_matches(head) and local_has(requested):
        return success_without_object_transfer()

    if cursor_on_chain_at_or_above_floor(head):
        if apply_transactions_after_cursor(head) succeeds:
            if verify_connectivity(refs):
                atomic_update_local_state(head, etag, refs)
                return success

    install_verified_packset(head.packset)
    apply_transactions(head.packset.generation + 1, head.logicalGeneration)
    verify_connectivity_or_fail(refs)
    atomic_update_local_state(head, etag, refs)
    return success
```

### 26.4 Compact and publish

```text
function maintenance(budget):
    head, etag, refs = pin_head_and_refs()
    assert local_repository_complete(refs)
    assert no_conflicting_gc_barrier(head)

    target, new_l0 = compact_transactions_after(head.packset.generation, budget)
    levels = add_and_promote_geometrically(head.packset.levels, new_l0, fanout)
    target_refs = reconstruct_refs_at(target)
    verify_union_connectivity(levels, target_refs)

    packset_ref = immutable_put(new_packset(target, levels))
    snapshot_ref = immutable_put(new_ref_snapshot(head.logicalGeneration, refs))
    candidate = replace_physical_state(head, packset_ref, snapshot_ref)
    candidate.log = retain_transactions_strictly_after(head.log, target)
    candidate.log.floor = transaction_at(target)
    candidate.manifestRevision += 1
    candidate.publicationId = random_uuid()

    validate_bootstrap(candidate)
    conditional_put_HEAD(candidate, if_match=etag) or CAS_CONFLICT
```

### 26.5 Delete under a GC barrier

```text
function execute_gc(operator_cutoff):
    head, etag = read_HEAD()
    candidates = list_minus_live_mark(head, operator_cutoff)
    plan_ref = immutable_put(canonical_plan(head, candidates))
    barrier_head = publish_barrier(head, etag, plan_ref)

    current, current_etag = read_HEAD()
    assert current.gcBarrier.planId == plan_ref.planId
    assert no_candidate_is_live(current, candidates)
    assert listed_metadata_still_matches(candidates)

    for candidate in candidates:
        conditional_delete(candidate.key, if_match=candidate.etag)

    latest, latest_etag = read_HEAD()
    assert latest.gcBarrier.planId == plan_ref.planId
    assert no_candidate_is_live(latest, candidates)
    conditional_clear_barrier(latest, latest_etag)
```

## 27. Validation and conformance matrix

### 27.1 Format and model tests

| ID | Test | Required result |
| --- | --- | --- |
| FMT-01 | Canonical round trip for every JSON type | Byte-stable canonical form. |
| FMT-02 | Duplicate keys, invalid UTF-8, overflow, bad UUID/OID | Rejected without writes. |
| FMT-03 | Object reference escapes reserved prefix | Rejected before GET or filesystem use. |
| FMT-04 | Snapshot with unsorted/duplicate/invalid refs | Rejected as corrupt. |
| FMT-05 | Transaction gap, wrong parent, duplicate ref update | Rejected as corrupt. |
| FMT-06 | Packset pack/index mismatch | Integrity failure. |
| FMT-07 | Unknown optional field | Ignored and preserved where HEAD is rewritten. |
| FMT-08 | Unknown required feature or format | Safe unsupported-format failure. |

### 27.2 Git behavior tests

| ID | Test | Required result |
| --- | --- | --- |
| GIT-01 | First single-branch push to missing prefix | Repository created; branch becomes HEAD. |
| GIT-02 | First multi-branch push including local HEAD mapping | Deterministic default branch. |
| GIT-03 | First ambiguous multi-branch push | Fails with no HEAD creation. |
| GIT-04 | Clone, fetch, pull, normal fast-forward push | Matches local filesystem remote semantics. |
| GIT-05 | Branch/tag create and delete | Exact advertised ref map after fetch. |
| GIT-06 | Non-fast-forward without force | Rejected. |
| GIT-07 | Forced branch and tag update | Published when CAS succeeds. |
| GIT-08 | Force-with-lease after competing update | Fails; no lost update. |
| GIT-09 | Atomic batch with one invalid ref | No ref from batch changes. |
| GIT-10 | Non-atomic batch with one invalid ref | Valid subset publishes in one transaction. |
| GIT-11 | SHA-1 and SHA-256 repositories | Each round-trips; cross-format use fails. |
| GIT-12 | Signed commits/tags | Object bytes and verification remain valid. |
| GIT-13 | Explicit shallow or partial request | Clear failure, never silent full clone. |
| GIT-14 | Remove git3 after fetch | Stock Git fsck, log, checkout, and repack work. |

### 27.3 Synchronization and concurrency tests

| ID | Test | Required result |
| --- | --- | --- |
| SYN-01 | No-op fetch with valid cache | One conditional HEAD GET; zero object bytes. |
| SYN-02 | Three transactions after cursor | Only required metadata pages and three WAL packs fetched. |
| SYN-03 | Remote repack at same logical tip | Up-to-date client fetches no packset. |
| SYN-04 | Local full repack after fetch | Cursor remains usable after requested-object checks. |
| SYN-05 | Missing thin-pack base | Incremental fails safely, bootstrap succeeds. |
| SYN-06 | Cursor below floor | Direct bootstrap; no attempt to assert stale cursor. |
| SYN-07 | Existing complete local object database, no cursor | Cursor seeds without packset download. |
| SYN-08 | Eight writers pinned to same HEAD | Exactly one initial CAS succeeds; no lost update. |
| SYN-09 | Retryable normal pushes after unrelated winner | Revalidated results only; bounded retries. |
| SYN-10 | Forced/deletion push loses CAS | Fails immediately. |

### 27.4 Crash and fault-injection tests

Inject process termination, network timeout, 409, 412, 404, short read, corrupt body, disk full, and
Git child failure before and after every numbered push, fetch, maintenance, and GC step. Required
properties are:

- no HEAD references an absent immutable object;
- no cursor names an unverified generation;
- no operation converts a conditional write/delete into an unconditional one;
- ambiguous publication is resolved by the remote chain or reported as ambiguous;
- completed immutable orphans do not change repository meaning;
- restart either resumes safely or recomputes from HEAD; and
- cancellation terminates bounded work and leaves diagnostic context.

### 27.5 Maintenance and GC tests

| ID | Test | Required result |
| --- | --- | --- |
| ADM-01 | Four level-zero packs at fanout four | One verified level-one pack published. |
| ADM-02 | Compaction CAS loses to push | Logical push remains; new packset is orphan. |
| ADM-03 | Clone from compacted multilevel packset | Current refs pass full connectivity. |
| ADM-04 | GC without `--execute` | No writes or deletes. |
| ADM-05 | GC execute without explicit cutoff | Usage failure. |
| ADM-06 | Publisher read before barrier | Publisher CAS loses after barrier appears. |
| ADM-07 | Publisher read during barrier | Preserves barrier and cannot revive candidate. |
| ADM-08 | Candidate becomes live or ETag changes | Deletion aborts. |
| ADM-09 | GC crashes after partial deletes | Resume/abort preserves current live state. |
| ADM-10 | Normal clone/fetch/push/maintenance request trace | No `LIST` or delete API. |

### 27.6 Scale qualification

Release qualification MUST include generated metadata for 100,000 refs, page chains spanning at least
10,000 transactions, packs larger than the multipart threshold, byte-range interruption/resume, and
checked arithmetic at the 1 TiB boundary. A periodic non-PR benchmark SHOULD exercise a repository
large enough to expose pack, memory, and request-scaling regressions. CI need not transfer 1 TiB on
every change, but no code path may encode a smaller architectural limit.

### 27.7 Release and installer tests

The release pipeline MUST verify all four platform assets, checksum failure behavior, unsupported
platform failure, version pinning, installation into a path containing spaces, symlink fallback,
absence of shell-profile changes, `git3 version`, `git s3 version`, and automatic invocation of
`git-remote-s3` by a real Git command.

## 28. Definition of done

The complete target is conformant only when:

- every core command and remote-helper behavior in this specification is implemented;
- all version 1 formats have golden fixtures and corruption fixtures;
- the full validation matrix passes at the required level;
- adapter contract tests with AWS SDK mocks prove conditional requests, multipart transfer, range
  download, encryption request construction, pagination, error mapping, and conditional deletion;
- no normal Git operation issues `LIST` or delete;
- fault injection covers every publication boundary;
- documentation includes setup, least-privilege IAM, encryption, custom endpoint caveats, disaster
  recovery boundaries, maintenance scheduling, GC safety, troubleshooting, and cost shape;
- Linux and macOS binaries for both architectures install through the documented one-line command;
- release assets include verified checksums, signatures, and provenance;
- the repository contains security, contribution, conduct, license, notice, and release-policy files;
  and
- a cloned repository remains a valid ordinary Git repository after all git3 executables and local
  `.git/git3` metadata are removed.

## 29. Optional extensions

The following MAY be specified later but are not implied by format version 1:

- a coordinator that batches concurrent pushes while preserving repository-wide transaction order;
- precomputed blob-oriented pack groups for an explicit partial-clone mode;
- Windows release and installer support;
- independent Git LFS custom transfer support;
- a shared local pack cache using standard Git alternates;
- metrics exporters or an administrative web interface;
- cross-region read replicas and signed repository-manifest policy; and
- migration tools for other S3 Git layouts.

Extensions MUST NOT make pack identity part of the logical cursor or bypass HEAD publication
invariants.

## 30. References

- [Git remote-helper protocol](https://git-scm.com/docs/gitremote-helpers)
- [Git `pack-objects`](https://git-scm.com/docs/git-pack-objects)
- [Git `index-pack`](https://git-scm.com/docs/git-index-pack)
- [Git multi-pack-index design](https://git-scm.com/docs/multi-pack-index)
- [Git geometric repack](https://git-scm.com/docs/git-repack)
- [Amazon S3 conditional requests](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-requests.html)
- [Amazon S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
- [Amazon S3 conditional deletes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-deletes.html)
- [Amazon S3 consistency model](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel)
- [Amazon S3 multipart upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html)
- [Amazon S3 performance guidelines](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html)
- [AWS SDK for Go v2 configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html)
- [AWS SDK for Go v2 endpoints](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html)
- [Amazon S3 SSE-KMS](https://docs.aws.amazon.com/AmazonS3/latest/userguide/specifying-kms-encryption.html)
- [GitHub artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations)
- [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119)
- [RFC 8174](https://www.rfc-editor.org/rfc/rfc8174)
- [RFC 8785 JSON Canonicalization Scheme](https://www.rfc-editor.org/rfc/rfc8785)
