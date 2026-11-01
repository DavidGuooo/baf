package main

import "runtime/debug"

// version is the release tag, set by deploy/release.sh. A build from source
// leaves it empty.
var version string

// versionString is the release tag, or "dev" with the commit it was built
// from when Go recorded one.
func versionString() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = ", modified"
			}
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev == "" {
		return "dev"
	}
	return "dev (" + rev + dirty + ")"
}
