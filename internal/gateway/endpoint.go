package gateway

import "github.com/huangzheng2016/pvm/internal/exit"

func resolveEndpoint(endpoint string) (string, string, error) {
	return exit.ResolveEndpoint(endpoint)
}
