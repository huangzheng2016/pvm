//go:build embedassets

package assets

import "embed"

//go:embed packed/manifest.json packed/*.zst
var packed embed.FS
