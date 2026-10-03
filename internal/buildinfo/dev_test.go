//go:build dev

package buildinfo

import "testing"

func TestDevBuild(t *testing.T) {
	if !Dev {
		t.Fatal("Dev is false in a build with the dev tag")
	}
	if !binaryContainsMarker(t) {
		t.Fatal("a build with the dev tag does not contain the dev marker")
	}
}
