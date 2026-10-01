package catalog

import "embed"

// The catalog that ships in the binary. It is embedded rather than read from a
// path so a sandbox environment works the moment it starts, with no volume and
// nothing to mount.
//
//go:embed seed/*.yaml
var seedFS embed.FS
