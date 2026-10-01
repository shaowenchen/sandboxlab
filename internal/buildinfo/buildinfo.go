// Package buildinfo carries the version of the running binary.
//
// The values are injected at link time (`-X .../buildinfo.Version=...`) by the
// Dockerfile and the Makefile, so a binary built with neither still reports
// something honest rather than an empty string.
package buildinfo

// Version is the release version, or "dev" for a build that was not tagged.
var Version = "dev"

// Commit is the git revision the binary was built from, or "unknown".
var Commit = "unknown"

// String renders the version and commit as one line, for logs and /api/v1/config.
//
// The two are usually the same string on a build with no tag, because
// `git describe --tags --always` falls back to the commit — so the duplicate is
// dropped rather than printed as "dfdb626 (dfdb626)".
func String() string {
	switch {
	case Version == "":
		return Commit
	case Commit == "" || Commit == "unknown" || Commit == Version:
		return Version
	default:
		return Version + " (" + Commit + ")"
	}
}
