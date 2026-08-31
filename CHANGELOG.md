# Changelog

## Unreleased

- Implement remote format version 1, S3 conditional publication, native Git pack transfer, logical
  cursors, helper protocol, maintenance, barriered GC, administrative commands, and release assets.
- Verify cache-derived ref state before mutation, enforce atomic first-push batches, recover stale
  local locks and interrupted pack-pair installations, and expand mock-based storage coverage.
- Add `AWS_PROFILE`, `git3.profile`, and per-remote `git3Profile` selection for multi-account and
  role-based AWS configurations.
- Add an optional Git LFS standalone transfer agent backed by immutable S3 objects, verified ranged
  downloads, conditional multipart uploads, URL-scoped setup, and GC protection for LFS payloads.
