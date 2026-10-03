// Package buildinfo reports the version, VCS state and flavour of the running binary.
package buildinfo

import "runtime/debug"

var Version = "dev"

type Info struct {
	Version  string
	Revision string
	Modified bool
	Dev      bool
}

func Read() Info {
	bi, _ := debug.ReadBuildInfo()
	return fromBuildInfo(bi)
}

func fromBuildInfo(bi *debug.BuildInfo) Info {
	info := Info{Version: Version, Dev: Dev}
	if bi == nil {
		return info
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
