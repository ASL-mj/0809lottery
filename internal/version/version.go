// Package version carries the build-time release version.
//
// The value is injected at build time via
//
//	-ldflags "-X skyeapi/lottery-bot/internal/version.Version=<version>"
//
// by deploy/release.sh; builds without the flag report "dev".
package version

var Version = "dev"
