# Changelog

## Unreleased

- Implement remote format version 1, S3 conditional publication, native Git pack transfer, logical
  cursors, helper protocol, maintenance, barriered GC, administrative commands, and release assets.
- Verify cache-derived ref state before mutation, enforce atomic first-push batches, recover stale
  local locks and interrupted pack-pair installations, and expand mock-based storage coverage.
- Add `AWS_PROFILE`, `git3.profile`, and per-remote `git3Profile` selection for multi-account and
  role-based AWS configurations.
