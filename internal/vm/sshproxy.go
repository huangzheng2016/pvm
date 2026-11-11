package vm

import (
	"runtime"
	"strings"
)

// ProxyCommandQuote quotes a host path for an ssh ProxyCommand. Windows
// OpenSSH starts ProxyCommand without a POSIX shell, so single quotes
// would stay part of the path; Windows paths cannot contain '"'.
func ProxyCommandQuote(s string) string {
	if runtime.GOOS == "windows" {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
