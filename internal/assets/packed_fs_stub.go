//go:build !embedassets

package assets

import "embed"

// packed stays empty unless the host binary is built with
// `-tags embedassets` (script/build.sh). Guest agent binaries are
// built WITHOUT the tag: embedding packed/ here would recursively
// nest the previous generation's guest binaries and explode in size.
var packed embed.FS
