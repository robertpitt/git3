# AWS S3 end-to-end performance baseline — UK to eu-west-2

- Date: 2026-08-30
- Run ID: `2026-08-30-uk-eu-west-2` (public identifier)
- Result: **Passed**
- Conservative maximum estimated cost: **$1.46**, below the **$5.00** cap

## Executive summary

This benchmark exercised git3 end to end from a UK development workstation against a dedicated
S3 Standard bucket in the London AWS region (`eu-west-2`). It used a new synthetic repository rather
than the git3 source repository.

The main results were:

- ref advertisement had a 0.248s median and 0.286s p95;
- warm no-op fetch had a 0.215s median and 0.259s p95;
- small pushes had a 0.477s median, while tiny transaction pushes had a 0.452s median;
- an 80 MiB single-part push took 8.658s and a 512 MiB multipart push took 57.855s;
- incremental fetch throughput increased from approximately 33.9 MiB/s at 80 MiB to 40.6 MiB/s
  at 512 MiB;
- the approximately 753 MiB transaction-based repository cold-cloned in 19.654s;
- after maintenance, two cold clones took 21.164s and 15.832s, showing no clear material change at
  this sample size;
- four concurrent ref advertisements all completed in 0.282–0.286s; and
- two synchronized concurrent writers produced one success and one expected CAS conflict. The
  losing writer refreshed and succeeded on retry in 1.995s total.

All completed clones passed `git fsck --full`. Remote metadata verification passed, all expected refs
were present, and the final repository reported healthy at logical generation 34.

## Environment

| Property | Value |
| --- | --- |
| Client location | United Kingdom |
| Bucket region | `eu-west-2` (London) |
| S3 locator | Dedicated synthetic repository under `s3://<benchmark-bucket>/git3-perf/<run-id>/` |
| AWS credentials | Development profile with access to the isolated benchmark bucket |
| S3 storage class | Standard |
| Bucket encryption | SSE-S3 (`AES256`) |
| Bucket versioning | Disabled |
| Public access | Fully blocked |
| Object ownership | Bucket owner enforced |
| Transfer acceleration | Disabled |
| Lifecycle | `git3-perf/` objects expire after 14 days; incomplete multipart uploads after 1 day |
| Client | Apple Silicon development workstation |
| Operating system | macOS, arm64 |
| Git | 2.50.1 (Apple Git-155) |
| git3 | v0.0.2, commit `3df0bb080e192ece645456c2d9fe2b4568a9f4fb` |
| Go runtime | go1.26.7 |
| Run duration | Approximately 8 minutes |

No git3 transfer settings were overridden. Relevant defaults were a 100 MiB multipart threshold,
128 MiB part size, upload concurrency 2, 64 MiB download chunks, and download concurrency 4.

These are user-perceived timings. They include local Git work, pack generation and verification, the
client's internet connection, AWS request processing, and local filesystem work. Although the client
and bucket were both in the UK, traffic still used the normal public S3 endpoint; this was not an
in-VPC or same-AZ service benchmark.

## Workload

The synthetic repository was built incrementally with:

1. 250 generated source-like files, totaling 748,925 bytes;
2. five small source updates, each with an approximately 208 KiB marker;
3. 5,000 small text files totaling 20,538,890 bytes;
4. incompressible random payloads of 80 MiB, 160 MiB, and 512 MiB;
5. 22 tiny pushes to reach logical generation 32 and trigger the default maintenance boundary; and
6. two 1 MiB branch payloads in a synchronized two-writer contention test.

Large payloads were intentionally incompressible so Git compression would not turn the transfer test
into a metadata-only test. Commands were timed with a monotonic wall clock and emitted Git Trace2
events. Every cold clone was followed by local full `fsck`.

## Results

### Metadata and small changes

| Operation | Samples | Median | p95 | Min–max |
| --- | ---: | ---: | ---: | ---: |
| `git ls-remote` | 7 | 0.248s | 0.286s | 0.225–0.286s |
| Warm no-op fetch | 7 | 0.215s | 0.259s | 0.185–0.259s |
| Small push (~208 KiB marker) | 5 | 0.477s | 0.532s | 0.424–0.532s |
| Matching incremental fetch | 5 | 0.434s | 0.551s | 0.409–0.551s |
| Tiny transaction push | 22 | 0.452s | 0.514s | 0.408–0.518s |
| Four simultaneous `ls-remote` calls | 4 | 0.284s | 0.286s | 0.282–0.286s |

Initial repository creation and push took 0.926s. The initial cold clone took 0.532s. Pushing the
5,000-file, 20.5 MB source-like addition took 0.986s and its incremental fetch took 0.530s. That
dataset is highly compressible, so its source size must not be treated as transferred bytes.

The parallel advertisement result showed no material latency degradation at four clients. This was
a metadata concurrency test, not a parallel full-clone throughput test.

### Binary transfer

| Stage | Payload represented | Time | Approximate payload rate |
| --- | ---: | ---: | ---: |
| Single-part push | 80 MiB | 8.658s | 9.24 MiB/s |
| Incremental fetch | 80 MiB | 2.361s | 33.88 MiB/s |
| Cold clone at this point | ~80 MiB plus source data | 3.353s | — |
| Multipart push | 160 MiB | 19.138s | 8.36 MiB/s |
| Incremental fetch | 160 MiB | 4.281s | 37.37 MiB/s |
| Cold clone at this point | ~240 MiB plus source data | 7.454s | — |
| Multipart push | 512 MiB | 57.855s | 8.85 MiB/s |
| Incremental fetch | 512 MiB | 12.626s | 40.55 MiB/s |

Payload rates divide generated payload size by whole-command wall time. Push timings therefore also
include Git pack generation, checksum calculation, metadata publication, and fixed request latency.
The results indicate a roughly 8–9 MiB/s large-push ceiling from this workstation, while incremental
downloads reached roughly 34–41 MiB/s.

### Maintenance and cold clone

Immediately before maintenance, `doctor` reported:

- logical generation 32;
- manifest revision 32;
- maintenance due; and
- 789,338,789 bytes (752.77 MiB) of live WAL data.

| Operation | Time |
| --- | ---: |
| Transaction-layout cold clone after large payloads | 19.654s |
| Maintenance | 108.039s |
| First post-maintenance cold clone | 21.164s |
| Second post-maintenance cold clone | 15.832s |
| Post-maintenance median | 18.498s |

The post-maintenance median was 5.9% faster than the transaction-layout sample, but the difference is
smaller than the 5.332s spread between the two post-maintenance observations. This run therefore
does **not** establish a material cold-clone improvement from maintenance within the UK region.

The transaction-layout clone was measured at generation 10 after all large payloads had been
published. The additional commits up to generation 32 contained only a few bytes each but would add
request overhead. A statistically stronger maintenance comparison would clone at generation 32
immediately before compaction and take at least five samples on each side.

Maintenance still reduced live WAL from 789,338,789 bytes to zero and reset the maintenance-due
state. Its value includes bounding transaction history and future request count, even though this
single regional run did not show a clear immediate clone-time benefit.

### Concurrent writers

Two synchronized clients attempted to create different branches from the same published HEAD. One
writer succeeded and one received the expected:

```text
CAS_CONFLICT: publish: another writer replaced HEAD
```

| Losing-writer stage | Time |
| --- | ---: |
| Initial conflicting push | 0.653s |
| Fetch/refresh | 0.677s |
| Successful retry | 0.665s |
| Total to success | 1.995s |

This confirms the expected single-HEAD compare-and-swap behavior. Both branch refs were present at
the end of the run.

## Cost and budget

The run enforced a 50 GiB conservative outbound guard. At $0.09/GB-equivalent, that leaves at least
$0.50 of the $5 cap for storage and requests. The guard reserved the entire physical test prefix for
some metadata and incremental operations, so it deliberately overstates likely transfer.

| Cost component | Conservative usage | Estimated cost |
| --- | ---: | ---: |
| Internet data transfer out | 14.652 GiB | $1.319 |
| One month of retained S3 Standard storage | 1.473 GiB | $0.035 |
| Request-cost allowance | — | $0.100 |
| **Conservative total** | — | **$1.454** |

The bucket's lifecycle should expire the benchmark objects after 14 days, making expected storage
cost approximately $0.017 rather than the full-month $0.035 shown above. If the AWS account still
has its shared first 100 GB/month internet-transfer allowance, the transfer line should be $0.
Actual request charges should be well below the $0.10 reserve. Taxes and optional logging/metrics
services are excluded.

London-region rates used were $0.024/GB-month for the first 50 TB of S3 Standard storage, $0.0053 per
1,000 PUT/COPY/POST/LIST requests, $0.00042 per 1,000 GET and similar requests, and $0.09/GB for the
first 10 TB of internet transfer beyond the global free tier. See
[AWS S3 pricing](https://aws.amazon.com/s3/pricing/) and
[AWS data-transfer pricing](https://aws.amazon.com/ec2/pricing/on-demand/).

The final prefix contains 77 objects and 1,581,985,978 physical bytes. Approximately half is the
compacted live pack and half is historical WAL retained after maintenance. The configured lifecycle
will eventually remove both; explicit GC or exact-prefix removal can reclaim them earlier.

## Final validation

Final remote state:

| Property | Value |
| --- | --- |
| Logical generation | 34 |
| Manifest revision | 35 |
| Object format | SHA-1 |
| Maintenance due | No |
| Live WAL | 2,098,387 bytes |
| Remote refs | `main`, `perf/client-1`, `perf/client-2` |

All completed clones passed `git fsck --full`. `git3 fsck` passed against the final remote metadata.
The benchmark repository remains available temporarily for inspection and is covered by the
14-day lifecycle rule.

To preview earlier cleanup, substitute the private bucket and run identifiers locally:

```sh
BENCHMARK_BUCKET="your-benchmark-bucket"
RUN_ID="your-run-id"

aws s3 rm \
  "s3://${BENCHMARK_BUCKET}/git3-perf/${RUN_ID}/synthetic" \
  --recursive --dryrun
```

Only remove `--dryrun` after confirming that exact target.

## Findings and follow-up work

1. **Regional placement dominates small-operation latency.** Keeping the client and bucket in the UK
   produced sub-300ms advertisement/no-op timings and sub-550ms ordinary push/fetch timings.
2. **Large downloads substantially outpaced uploads.** Incremental reads reached about 41 MiB/s,
   while large pushes remained around 8–9 MiB/s, suggesting the client upstream path and sequential
   upload implementation are the leading constraints.
3. **Multipart upload is currently sequential.** `git3.uploadConcurrency` is parsed and documented,
   but the current [`putMultipart`](../../internal/store/s3.go) loop calls `UploadPart` synchronously
   for each part and does not consume the configured upload-concurrency value. Implementing bounded
   parallel part uploads is the clearest likely improvement for large pushes.
4. **Maintenance needs a stronger regional experiment.** The two post-maintenance samples straddled
   the pre-maintenance observation, so this run does not support a confident clone-time claim.
5. **CAS contention is inexpensive but visible.** A synchronized losing writer recovered in 1.995s
   after explicit refresh and retry. A bounded automatic retry for safe non-forced changes could
   improve this path if it preserves the specification's full revalidation rules.
6. **Add S3-operation telemetry before deeper tuning.** Per-operation request counts, bytes, retries,
   and durations would separate Git CPU time, public-network transfer, and S3 latency without relying
   on a conservative inventory-based transfer model.

Large operations have one or two samples because the goal was a cost-bounded baseline, not a
statistically complete capacity test. Future comparison runs should retain the same workload shape,
record at least five large-operation samples, and keep the client network class constant.
