// Package themes holds Actlet's built-in viewer themes as data, in the
// theme format every viewer shares: dark, light and plain.
package themes

import "embed"

// FS holds the built-in themes, as <name>.yaml.
//
//go:embed *.yaml
var FS embed.FS

// Names are the built-in themes.
var Names = []string{"dark", "light", "plain"}
