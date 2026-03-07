// Package version holds build-time version information injected via -ldflags.
package version

// Version is the semver release tag (e.g. "v1.2.3"). Defaults to "dev" when
// not injected at build time.
var Version = "dev"

// Commit is the short git commit hash of the build. Defaults to "none".
var Commit = "none"

// Date is the ISO-8601 UTC build timestamp. Defaults to "unknown".
var Date = "unknown"
