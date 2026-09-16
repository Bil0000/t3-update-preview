# Changelog

## 0.1.0-alpha.1 - 2026-09-16

- Initial Go command and embedded Python host worker for coordinated T3 preview updates.
- Tests for release selection, download checks, configuration, discovery, scheduling, and host update recovery using isolated fixtures.
- macOS and Linux CI checks, plus cross-compiled release archives for amd64 and arm64 with SHA256 checksums.

Live installation behavior and platform limits are described in the [README](README.md). Fixture tests and successful builds do not certify live updates on every platform.
