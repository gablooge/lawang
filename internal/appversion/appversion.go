// Package appversion reports what build is running.
package appversion

import "runtime/debug"

// Version is set at release time with -ldflags "-X .../internal/appversion.Version=v0.1.0".
var Version = ""

// revisionLen is how much of the VCS revision a development build shows.
const revisionLen = 12

// String returns the release version, or the VCS revision for a development build.
func String() string {
	return describe(Version, buildSettings())
}

// buildSettings returns what the toolchain recorded about this build, or nil when it recorded
// nothing.
func buildSettings() []debug.BuildSetting {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return info.Settings
}

// describe is String without its inputs, so that every case can be tested: a binary under test
// carries no VCS settings and would only ever show "dev".
//
//   - a release version wins over everything else
//   - otherwise "dev-" and the first 12 characters of the revision, with "-dirty" when the tree had
//     uncommitted changes
//   - "dev" alone when there is no revision to show
func describe(version string, settings []debug.BuildSetting) string {
	if version != "" {
		return version
	}
	rev, dirty := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > revisionLen {
		rev = rev[:revisionLen]
	}
	if dirty {
		rev += "-dirty"
	}
	return "dev-" + rev
}
