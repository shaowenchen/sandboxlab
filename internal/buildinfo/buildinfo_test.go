package buildinfo

import "testing"

func TestString(t *testing.T) {
	// The values are package-level and stamped at link time, so a test saves
	// and restores them rather than running in parallel.
	savedVersion, savedCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = savedVersion, savedCommit })

	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{"unbuilt", "dev", "unknown", "dev"},
		{"a release tag", "v0.1.0", "dfdb626", "v0.1.0 (dfdb626)"},
		// No tags: `git describe --tags --always` falls back to the commit, so
		// both arrive as the same string and must not be printed twice.
		{"no tags at all", "dfdb626", "dfdb626", "dfdb626"},
		{"a version with no commit", "v0.1.0", "", "v0.1.0"},
		{"a commit with no version", "", "dfdb626", "dfdb626"},
		{"neither", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit = tc.version, tc.commit
			if got := String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
