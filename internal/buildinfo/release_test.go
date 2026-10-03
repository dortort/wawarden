//go:build !dev

package buildinfo

import "testing"

func TestReleaseBuild(t *testing.T) {
	if Dev {
		t.Fatal("Dev is true in a build without the dev tag")
	}
	if binaryContainsMarker(t) {
		t.Fatal("a build without the dev tag contains the dev marker")
	}
}
