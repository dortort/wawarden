//go:build dev

package buildinfo

const Dev = true

const devMarker = "wawarden-dev-build-not-for-release"

var marker string

// Assigned in init because the linker drops unreferenced constants and statically initialised variables.
func init() { marker = devMarker }
