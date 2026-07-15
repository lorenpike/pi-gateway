// Package version exposes wall-e's release version.
package version

import (
	_ "embed"
	"strings"
)

// raw is embedded from the repository's single version source.
//
//go:embed VERSION
var raw string

// String returns the normalized semantic version.
func String() string {
	return strings.TrimSpace(raw)
}
