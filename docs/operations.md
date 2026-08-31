# Operations and recovery

## Maintenance

Run `git3 maintenance origin` from a complete, non-shallow clone when `doctor` reports maintenance
due. Compaction publishes immutable native packs, a packset, and a ref snapshot, then conditionally
replaces HEAD. A conflict leaves harmless immutable orphans and must be retried from a fresh state.
Schedule maintenance around the default 32-transaction or 128 MiB WAL thresholds. Large compactions
need temporary local space comparable to the selected merge.

## Garbage collection

Bucket lifecycle and retention are operator policy. Start with `git3 gc origin --json`, review every
candidate, then supply an explicit boundary such as `--execute --older-than 720h`. Execution writes
a plan, publishes a barrier, revalidates every candidate, conditionally deletes it, and clears the
barrier. Resume an interrupted plan with `--resume`; use `--abort` to clear its matching barrier.
Object Lock, legal holds, versioning, and replication may prevent or preserve deletions.

Git LFS payloads below `.git/git3/lfs/` are excluded from git3's manifest-based garbage collector.
git3 does not currently determine which LFS OIDs remain reachable or delete LFS payloads. Apply a
separate, explicitly reviewed bucket lifecycle or future LFS-aware collection process if retention
is required. Do not resume a GC plan produced by an older client if it names an LFS object; current
clients reject such a plan and allow it to be aborted safely.

## Disaster recovery

Enable bucket versioning if rollback is required. Restoring an older HEAD can lower the generation
or create a divergent transaction ID; clients retain local objects and bootstrap/seed against the
restored manifest. Restore all immutable objects referenced by the selected HEAD before restoring
HEAD itself. `git3 fsck --full` validates the result. CloudTrail is the identity-level audit log.

## Custom endpoints

Custom endpoints are optional. They must implement strong read-after-write behavior, conditional
single-key writes and deletes, ranges, multipart upload, and S3-compatible error semantics. HTTP is
rejected unless explicitly enabled and should only be used on loopback test systems.

## Troubleshooting and cost shape

- `AUTH_FAILED`: refresh the standard AWS credential source or IAM/KMS policy.
- `CAS_CONFLICT`: another publisher won; fetch and revalidate before retrying.
- `INTEGRITY_FAILED`: stop writes, retain evidence, and run `fsck`.
- `GC_BARRIER_ACTIVE`: resume or abort the named plan.
- Missing thin-pack bases cause a safe packset bootstrap.

Push cost is proportional to changed refs and new reachable data. Active fetch consumes logical
transactions after the cursor. A cold clone downloads the current packset plus the bounded WAL tail.
Regular geometric maintenance keeps pack request count and remote rewrite amplification logarithmic.
