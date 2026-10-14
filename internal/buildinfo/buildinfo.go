// Package buildinfo reports the version, VCS state and flavour of the running binary.
package buildinfo

import (
	"regexp"
	"runtime/debug"
)

const unreleased = "dev"

var Version = unreleased

var releaseVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Info struct {
	Version  string
	Revision string
	Modified bool
	Dev      bool
}

func Read() Info {
	bi, _ := debug.ReadBuildInfo()
	return fromBuildInfo(Version, bi)
}

func fromBuildInfo(version string, bi *debug.BuildInfo) Info {
	info := Info{Version: version, Dev: Dev}
	if bi == nil {
		return info
	}
	if version == unreleased && releaseVersion.MatchString(bi.Main.Version) {
		info.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}
