package cli

import (
	"fmt"

	skilldoc "github.com/huangzheng2016/pvm"
)

// CmdSecurity prints the embedded security guide (agent-facing posture
// instructions), like `pvm skill` does for the usage manual.
func CmdSecurity(args []string) int {
	fmt.Print(skilldoc.SECURITY)
	return 0
}
