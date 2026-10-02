// Package version holds the build version shared by this repository's binaries.
package version

// Version is the build version. Release builds override it with:
//
//	-ldflags "-X github.com/ipekutku/ai-sre-agent/internal/version.Version=v0.1.0"
var Version = "dev"
