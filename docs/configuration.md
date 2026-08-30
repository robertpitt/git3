# Configuration reference

Precedence is command flags, environment, named-remote configuration, ordinary `git3.*` Git
configuration, then defaults. AWS credential and shared configuration precedence remains owned by
the AWS SDK.

| Git key | Named-remote suffix | Environment | Default |
| --- | --- | --- | --- |
| `git3.profile` | `git3Profile` | `AWS_PROFILE` | AWS resolution |
| `git3.region` | `git3Region` | `GIT3_REGION` | AWS resolution |
| `git3.endpoint` | `git3Endpoint` | `GIT3_ENDPOINT` | AWS endpoint |
| `git3.pathStyle` | `git3PathStyle` | `GIT3_PATH_STYLE` | `false` |
| `git3.allowInsecureEndpoint` | `git3AllowInsecureEndpoint` | `GIT3_ALLOW_INSECURE_ENDPOINT` | `false` |
| `git3.sse` | `git3Sse` | `GIT3_SSE` | `inherit` |
| `git3.kmsKeyId` | `git3KmsKeyId` | `GIT3_KMS_KEY_ID` | unset |
| `git3.bucketKeyEnabled` | `git3BucketKeyEnabled` | `GIT3_BUCKET_KEY_ENABLED` | unset |
| `git3.multipartThreshold` | `git3MultipartThreshold` | `GIT3_MULTIPART_THRESHOLD` | `100MiB` |
| `git3.partSize` | `git3PartSize` | `GIT3_PART_SIZE` | `128MiB` |
| `git3.uploadConcurrency` | `git3UploadConcurrency` | `GIT3_UPLOAD_CONCURRENCY` | `2` |
| `git3.downloadChunkSize` | `git3DownloadChunkSize` | `GIT3_DOWNLOAD_CHUNK_SIZE` | `64MiB` |
| `git3.downloadConcurrency` | `git3DownloadConcurrency` | `GIT3_DOWNLOAD_CONCURRENCY` | `4` |
| `git3.maxAttempts` | `git3MaxAttempts` | `GIT3_MAX_ATTEMPTS` | `5` |
| `git3.logFormat` | `git3LogFormat` | `GIT3_LOG_FORMAT` | `human` |
| `git3.compactionFanout` | `git3CompactionFanout` | `GIT3_COMPACTION_FANOUT` | `4` |
| `git3.compactAfterTransactions` | `git3CompactAfterTransactions` | `GIT3_COMPACT_AFTER_TRANSACTIONS` | `32` |
| `git3.compactAfterBytes` | `git3CompactAfterBytes` | `GIT3_COMPACT_AFTER_BYTES` | `128MiB` |

For a remote named `origin`, combine `remote.origin.` with the named suffix, for example
`remote.origin.git3DownloadConcurrency`. Boolean values use Git boolean spellings. Byte values are
unsigned bytes or use `KiB`, `MiB`, `GiB`, or `TiB`.

`uploadConcurrency` bounds in-flight multipart parts from 1 to 16. Multipart upload memory is
approximately `partSize × uploadConcurrency`, so increase it only when the available memory and
network bandwidth justify the additional parallelism.

`AWS_PROFILE` selects a profile from the standard AWS shared configuration and credentials files.
The equivalent Git settings let different remotes use different AWS accounts or roles:

```sh
git config remote.origin.git3Profile production
git config remote.backup.git3Profile disaster-recovery
```
