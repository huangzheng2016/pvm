package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/huangzheng2016/pvm/internal/keys"
)

func loadToken(value, file string) (string, error) {
	if file == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("--token and --token-file are mutually exclusive")
	}
	if err := keys.CheckPermissions(file); err != nil {
		return "", fmt.Errorf("token file must be a private regular file owned by this user (mode 0600)")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("cannot read token file")
	}
	value = strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n\t ") {
		return "", fmt.Errorf("token file must contain one nonempty token")
	}
	return value, nil
}
