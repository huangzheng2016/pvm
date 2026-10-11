package cli

import (
	"fmt"

	skilldoc "github.com/huangzheng2016/pvm"
)

func CmdSkill(args []string) int {
	fmt.Print(skilldoc.SKILL)
	return 0
}
