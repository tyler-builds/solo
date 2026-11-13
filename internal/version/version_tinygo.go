//go:build tinygo

package version

// unreleased names a build made without the scripts. TinyGo records nothing about the build.
func unreleased() string { return "dev" }
