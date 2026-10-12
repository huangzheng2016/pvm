//go:build !embedassets

package assets

import "embed"

// Guest builds must leave packed empty: embedding it would recursively
// nest the previous generation's guest binaries.
var packed embed.FS
