// Package version says which Solo this is: the desktop app, or the web client built with it.
package version

// Version is the release ("0.6.0") or, for a build between releases, git's description of its commit
// ("0.6.0-3-gcb015ff": three commits after 0.6.0). The build scripts set it (scripts/version.sh says
// what to); a plain go build leaves it empty.
var Version string

// String is the version to show: Version, or else what the build itself records.
func String() string {
	if Version != "" {
		return Version
	}
	return unreleased()
}
