//go:build embedassets

package assets

import "embed"

//go:embed all:packed
var packed embed.FS
