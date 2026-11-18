//go:build windows && !production

package main

import "embed"

// Without the production tag the package only has to compile: the built
// frontend is not in the repository.
var assets embed.FS
