package cli

import (
	"fmt"

	skilldoc "github.com/huangzheng2016/pvm"
)

func CmdSecurity(args []string) int {
	fmt.Print(skilldoc.SECURITY)
	return 0
}
