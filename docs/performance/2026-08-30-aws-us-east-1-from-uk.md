# AWS S3 end-to-end performance baseline — UK to us-east-1

Date: 2026-08-30  
Run ID: `2026-08-30-uk-us-east-1` (public identifier)  
Result: **Passed**  
Conservative maximum estimated cost: **$1.78**, below the **$5.00** cap

## Executive summary

This benchmark exercised git3 end to end from a developer workstation in the UK against a real
S3 Standard bucket in `us-east-1`. It used a new synthetic repository rather than the git3 source
repository.

The main results were:

- ordinary metadata operations have an approximately one-second UK-to-N. Virginia latency floor;
- warm no-op fetch had a 0.931s median and 0.958s p95;
- small pushes had a 1.509s median, while tiny transaction pushes had a 1.623s median;
- an 80 MiB single-part push took 30.880s and a 512 MiB multipart push took 92.100s;
- incremental fetch throughput increased from approximately 14.3 MiB/s at 80 MiB to 19.0 MiB/s
  at 512 MiB;
- the approximately 753 MiB repository took 340.454s to cold-clone from its transaction packs;
- after maintenance, the same repository cold-cloned in 62.508s, a **5.45x improvement**;
- four concurrent ref advertisements all completed in 1.068–1.086s; and
- two synchronized concurrent writers produced one success and one expected CAS conflict. The
  losing writer refreshed and succeeded on retry in 9.641s total.

All completed clones passed `git fsck --full`. Remote metadata verification passed, all expected refs
were present, and the final repository reported healthy at logical generation 36.

## Environment

| Property | Value |
| --- | --- |
| Client location | United Kingdom |
| Bucket region | `us-east-1` (N. Virginia) |
| S3 locator | Dedicated synthetic repository under `s3://<benchmark-bucket>/git3-perf/<run-id>/` |
| AWS credentials | Development profile with access to the isolated benchmark prefix |
| S3 storage class | Standard |
| Bucket encryption | SSE-S3 (`AES256`) |
| Bucket versioning | Disabled |
| Transfer acceleration | Disabled |
| Client | Apple Silicon development workstation |
| Operating system | macOS, arm64 |
| Git | 2.50.1 (Apple Git-155) |
| git3 | v0.0.2, commit `3df0bb080e192ece645456c2d9fe2b4568a9f4fb` |
| Go runtime | go1.26.7 |
| Run window | Approximately 25 minutes |

No git3 transfer settings were overridden. Relevant defaults were a 100 MiB multipart threshold,
128 MiB part size, upload concurrency 2, 64 MiB download chunks, and download concurrency 4.

These are user-perceived timings from the UK. They include local Git work, pack generation and
verification, the local internet connection, transatlantic latency, AWS request processing, and
filesystem work. They should not be interpreted as isolated S3 service benchmarks.

## Workload

The synthetic repository was built incrementally with:

1. 250 generated source-like files, totaling 748,925 bytes;
2. five small source updates, each with an approximately 208 KiB marker;
3. 5,000 small text files totaling 20,538,890 bytes;
4. incompressible random payloads of 80 MiB, 160 MiB, and 512 MiB;
5. 22 tiny pushes to reach logical generation 32 and trigger the default maintenance boundary; and
6. four 1 MiB branch payloads across two two-writer contention tests.

Large payloads were intentionally incompressible so Git compression would not turn the transfer test
into a metadata-only test. Commands were timed with a monotonic wall clock and also emitted Git
Trace2 event logs. Every cold clone was followed by local full `fsck`.

## Results

### Metadata and small changes

| Operation | Samples | Median | p95 | Min–max |
| --- | ---: | ---: | ---: | ---: |
| `git ls-remote` | 7 | 1.101s | 1.205s | 0.965–1.205s |
| Warm no-op fetch | 7 | 0.931s | 0.958s | 0.822–0.958s |
| Small push (~208 KiB marker) | 5 | 1.509s | 1.557s | 1.433–1.557s |
| Matching incremental fetch | 5 | 1.234s | 1.293s | 1.206–1.293s |
| Tiny transaction push | 22 | 1.623s | 1.792s | 1.372–1.949s |
| Four simultaneous `ls-remote` calls | 4 | 1.083s | 1.086s | 1.068–1.086s |

Initial repository creation and push took 2.688s. The initial cold clone took 1.598s. Pushing the
5,000-file, 20.5 MB source-like addition took 3.849s and its incremental fetch took 1.768s. That
dataset is highly compressible, so its source size must not be treated as transferred bytes.

The parallel advertisement result showed no material latency degradation at four clients. This was
a metadata concurrency test, not a parallel full-clone throughput test.

### Binary transfer

| Stage | Payload represented | Time | Approximate payload rate |
| --- | ---: | ---: | ---: |
| Single-part push | 80 MiB | 30.880s | 2.59 MiB/s |
| Incremental fetch | 80 MiB | 5.595s | 14.30 MiB/s |
| Cold clone at this point | ~80 MiB plus source data | 7.290s | — |
| Multipart push | 160 MiB | 41.256s | 3.88 MiB/s |
| Incremental fetch | 160 MiB | 9.553s | 16.75 MiB/s |
| Cold clone at this point | ~240 MiB plus source data | 14.868s | — |
| Multipart push | 512 MiB | 92.100s | 5.56 MiB/s |
| Incremental fetch | 512 MiB | 26.892s | 19.04 MiB/s |

Payload rates divide generated payload size by whole-command wall time. Push timings therefore also
include Git pack generation, checksum calculation, metadata publication, and fixed request latency.

Download throughput was substantially higher than upload throughput from this UK workstation. A
same-region EC2 run is needed to distinguish git3/S3 throughput from the local upstream connection
and transatlantic path.

### Maintenance and cold clone

Immediately before maintenance, `doctor` reported:

- logical generation 32;
- manifest revision 32;
- maintenance due; and
- 789,338,887 bytes (752.77 MiB) of live WAL data.

| Operation | Time |
| --- | ---: |
| Pre-maintenance cold clone | 340.454s |
| Maintenance | 185.532s |
| Post-maintenance cold clone | 62.508s |
| Second post-maintenance cold clone | 64.897s |

Maintenance reduced measured cold-clone time by **81.64%**, or **5.45x**. The pre-maintenance clone
was measured at generation 10 after all large payloads had been published. The additional commits
up to generation 32 contained only a few bytes each, but would add transaction request overhead;
this asymmetry makes the improvement estimate conservative rather than artificially favorable.

Before maintenance the helper replayed the large transaction packs sequentially through
`index-pack`. After maintenance it downloaded one compacted pack with concurrent range reads. On
this workload, the 277.946s saved by one subsequent cold clone exceeded the 185.532s maintenance
cost, although those times are spent by different operations and should not be treated as a formal
amortized cost model.

After maintenance, `doctor` reported manifest revision 33, maintenance not due, and zero live WAL
bytes. The four contention branches subsequently added 4,196,762 bytes of live WAL.

### Concurrent writers

Two contention tests were performed. In each, both clients attempted to create a different branch
from the same published HEAD. One writer succeeded and one received:

```text
CAS_CONFLICT: publish: another writer replaced HEAD
```

The corrected test used two fully synchronized clients:

| Losing-writer stage | Time |
| --- | ---: |
| Initial conflicting push | 2.192s |
| Fetch/refresh | 2.215s |
| Successful retry | 5.234s |
| Total to success | 9.641s |

This confirms the expected single-HEAD compare-and-swap behavior. The first contention test used the
original authoring repository as the loser; because it lacked a current git3 cursor, refresh took
24.286s. That result demonstrates the cursor-bootstrap penalty but is not the representative
active-client retry number.

## Cost and budget

The run enforced a 50 GiB conservative outbound guard. At $0.09/GiB-equivalent, that leaves $0.50
of the $5 cap for storage and requests. The guard reserved the entire physical test prefix for some
metadata and incremental operations, so it deliberately overstates likely transfer.

| Cost component | Conservative usage | Estimated cost |
| --- | ---: | ---: |
| Internet data transfer out | 18.339 GiB | $1.651 |
| One month of retained S3 Standard storage | 1.476 GiB | $0.034 |
| Request-cost allowance | — | $0.100 |
| **Conservative total** | — | **$1.785** |

If the AWS account still has its shared first 100 GB/month internet-transfer allowance, the transfer
line should be $0 and the corresponding estimate is at most $0.134. Actual request charges should be
well below the $0.10 reserve. Taxes and optional logging/metrics services are excluded.

Rates used were $0.023/GB-month for the first 50 TB of S3 Standard storage, $0.005 per 1,000
PUT/COPY/POST/LIST requests, $0.0004 per 1,000 GET and similar requests, and $0.09/GB for the first
10 TB of internet transfer beyond the global free tier. See [AWS S3 pricing](https://aws.amazon.com/s3/pricing/)
and [AWS data-transfer pricing](https://aws.amazon.com/ec2/pricing/on-demand/).

The final prefix contains 83 objects and 1,585,138,291 physical bytes. About half is the compacted
live pack and half is old WAL retained after maintenance. Normal git3 operation does not immediately
delete superseded objects; GC or removal of the isolated test prefix is required to stop retaining
that historical copy.

## Final validation

Final remote state:

| Property | Value |
| --- | --- |
| Logical generation | 36 |
| Manifest revision | 37 |
| Object format | SHA-1 |
| Maintenance due | No |
| Live WAL | 4,196,762 bytes |
| Remote refs | `main`, `perf/client-1`, `perf/client-2`, `perf/fair-1`, `perf/fair-2` |

All completed clones passed `git fsck --full`. `git3 fsck` passed against the final remote metadata.
The benchmark intentionally retained the remote repository for inspection.

To preview eventual cleanup, use the exact isolated run prefix:

```sh
BENCHMARK_BUCKET="your-benchmark-bucket"
RUN_ID="your-run-id"

aws s3 rm \
  "s3://${BENCHMARK_BUCKET}/git3-perf/${RUN_ID}/synthetic" \
  --recursive --dryrun
```

Only remove `--dryrun` after confirming that exact target.

## Findings and follow-up work

1. **Maintenance is essential for cold-clone performance.** At this data shape, waiting until the
   32-transaction threshold resulted in a 340s clone. Compaction reduced it to about 63–65s.
2. **UK metadata latency is dominated by fixed round trips.** Small pushes and fetches cluster around
   1–2s even when almost no bytes change.
3. **Multipart upload is currently sequential.** The current
   [`putMultipart`](../../internal/store/s3.go) loop calls `UploadPart` synchronously for each part.
   The unused upload-concurrency setting has been removed; implementing bounded parallel part
   uploads remains the clearest likely improvement for large UK-origin pushes.
4. **The CAS conflict is safe but visible to users.** A synchronized loser needed 9.641s to succeed
   after explicit refresh and retry. A carefully bounded automatic retry for safe, non-forced
   updates would improve this path if it preserves the specification's revalidation rules.
5. **Add S3-operation telemetry before deeper tuning.** Per-operation request counts, bytes, retries,
   and durations would separate Git CPU time, network transfer, and S3 latency without relying on a
   conservative inventory-based transfer model.
6. **Run a paired regional benchmark.** Repeating the same synthetic repository from an appropriately
   sized EC2 instance in `us-east-1` would establish backend throughput; comparing it with this UK
   baseline would quantify geographic/network cost.

Large operations have one or two samples because the goal was a cost-bounded baseline, not a
statistically complete capacity test. Future comparison runs should reuse this workload shape,
record at least five large-operation samples, and retain the same client region and network class.
