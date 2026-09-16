# Contributing

Use Go 1.24 or newer, Python 3.10 or newer, and Make. The Go coordinator and embedded Python worker use their standard libraries.

Run these checks before opening a pull request:

```sh
make check
```

This runs Go tests with the race detector, Python worker tests, a build, Go vet, and a formatting check. `make build` writes `dist/t3-update-preview`. `make test` runs only the tests.

Keep tests isolated. Use temporary directories, fake process results, and local HTTP fixtures. Tests must not contact enrolled devices, invoke real SSH sessions, update a T3 installation, or manage a live service. Do not run the updater against a personal installation to validate a pull request.

Keep changes focused. Add a regression test for a bug fix. Explain what changed, why, and which checks passed. State any platform behavior that has not been tested. Remove tokens, host names, user paths, and database contents from reports.

CI checks macOS and Linux. The tag release workflow cross-compiles archives for both systems on amd64 and arm64, adds SHA256 checksums, and uploads them to a GitHub release. Cross-compilation does not prove that a live update works on each target. Release workflows do not install the updater or update devices.

Report security issues through the process in [SECURITY.md](SECURITY.md).
