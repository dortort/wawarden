package buildinfo

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	tests := []struct {
		name         string
		bi           *debug.BuildInfo
		wantRevision string
		wantModified bool
	}{
		{name: "no build info", bi: nil},
		{name: "no vcs settings", bi: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "-trimpath", Value: "true"}}}},
		{
			name: "clean checkout",
			bi: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs", Value: "git"},
				{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
				{Key: "vcs.modified", Value: "false"},
			}},
			wantRevision: "0123456789abcdef0123456789abcdef01234567",
		},
		{
			name: "modified checkout",
			bi: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "fedcba9876543210fedcba9876543210fedcba98"},
				{Key: "vcs.modified", Value: "true"},
			}},
			wantRevision: "fedcba9876543210fedcba9876543210fedcba98",
			wantModified: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fromBuildInfo(Version, tt.bi)
			want := Info{Version: Version, Revision: tt.wantRevision, Modified: tt.wantModified, Dev: Dev}
			if got != want {
				t.Fatalf("fromBuildInfo() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestVersionFromTheModule(t *testing.T) {
	tests := []struct {
		name, linked, module, want string
	}{
		{name: "go install of a release", linked: "dev", module: "v1.2.3", want: "v1.2.3"},
		{name: "a release with a zero minor and patch", linked: "dev", module: "v1.0.0", want: "v1.0.0"},
		{name: "a build outside version control", linked: "dev", module: "(devel)", want: "dev"},
		{name: "no module version", linked: "dev", module: "", want: "dev"},
		{name: "a commit after a tag", linked: "dev", module: "v1.2.4-0.20261014010000-f7d12a47976a", want: "dev"},
		{name: "a modified tree at a tag", linked: "dev", module: "v1.2.3+dirty", want: "dev"},
		{name: "a pre-release", linked: "dev", module: "v1.2.3-rc.1", want: "dev"},
		{name: "a leading zero", linked: "dev", module: "v1.02.3", want: "dev"},
		{name: "no v prefix", linked: "dev", module: "1.2.3", want: "dev"},
		{name: "a version set at link time", linked: "v1.3.0", module: "v1.2.3", want: "v1.3.0"},
		{name: "the CI version set at link time", linked: "ci", module: "v1.2.3", want: "ci"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/dortort/wawarden", Version: tt.module}}
			if got := fromBuildInfo(tt.linked, bi).Version; got != tt.want {
				t.Fatalf("version linked %q, module %q: got %q, want %q", tt.linked, tt.module, got, tt.want)
			}
		})
	}
}

func TestReadDefaults(t *testing.T) {
	got := Read()
	if got.Version != "dev" {
		t.Fatalf("Read().Version = %q, want the default %q", got.Version, "dev")
	}
	if got.Dev != Dev {
		t.Fatalf("Read().Dev = %v, want %v", got.Dev, Dev)
	}
}

func scanMarker() string {
	return strings.Join([]string{"wawarden", "dev", "build", "not", "for", "release"}, "-")
}

func binaryContainsMarker(t *testing.T) bool {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the running test binary, from os.Executable
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	return strings.Contains(string(data), scanMarker())
}
