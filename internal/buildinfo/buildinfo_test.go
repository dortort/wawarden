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
			got := fromBuildInfo(tt.bi)
			want := Info{Version: Version, Revision: tt.wantRevision, Modified: tt.wantModified, Dev: Dev}
			if got != want {
				t.Fatalf("fromBuildInfo() = %+v, want %+v", got, want)
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	return strings.Contains(string(data), scanMarker())
}
