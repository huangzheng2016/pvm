package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"syscall"
)

// APIError is the structured daemon error envelope. Error keeps the legacy
// flat message so older clients continue to decode it unchanged.
type APIError struct {
	Message    string `json:"error"`
	Code       string `json:"code"`
	NextAction string `json:"next_action,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"-"`
}

func (e *APIError) Error() string { return e.Message }

func classifyError(httpCode int, err error) *APIError {
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		out := *ae
		if out.Message == "" {
			out.Message = err.Error()
		}
		if out.HTTPStatus == 0 {
			out.HTTPStatus = httpCode
		}
		return &out
	}
	out := &APIError{Message: err.Error(), Code: "internal_error", HTTPStatus: httpCode}
	msg := out.Message
	switch {
	case errors.Is(err, errVMNotFound):
		out.Code = "vm_not_found"
		out.NextAction = "pvm list"
	case errors.Is(err, context.DeadlineExceeded):
		out.Code = "timeout"
		out.Retryable = true
	case errors.Is(err, context.Canceled):
		out.Code = "canceled"
		out.Retryable = true
	case errors.Is(err, os.ErrPermission):
		out.Code = "permission_denied"
		out.NextAction = "pvm doctor"
	case errors.Is(err, syscall.ENOSPC):
		out.Code = "disk_full"
		out.NextAction = "free host disk space, or stop a VM and run `pvm compact <name>`"
	case errors.Is(err, syscall.EADDRINUSE):
		out.Code = "port_in_use"
		out.NextAction = "pvm map ls"
	case errors.Is(err, os.ErrNotExist):
		out.Code = "not_found"
	case httpCode == http.StatusConflict:
		out.Code = "state_conflict"
	case httpCode == http.StatusBadRequest:
		out.Code = "invalid_input"
	case httpCode == http.StatusNotFound:
		out.Code = "not_found"
	case httpCode == http.StatusGatewayTimeout:
		out.Code = "timeout"
		out.Retryable = true
	}
	switch {
	case strings.Contains(msg, "is not running"):
		out.Code = "vm_not_running"
		out.NextAction = "pvm start <name>"
	case strings.Contains(msg, "cloud-init"):
		out.Code = "cloud_init_failed"
		out.NextAction = "inspect `pvm ssh <name> 'sudo tail -n 50 /var/log/cloud-init-output.log'` or recreate the VM"
	case strings.Contains(msg, "ssh not reachable") || strings.Contains(msg, "agent not reachable"):
		out.Code = "guest_unreachable"
		out.Retryable = true
		out.NextAction = "pvm logs <name> --follow"
	case strings.Contains(msg, "already exists"):
		out.Code = "already_exists"
		out.NextAction = "pvm list"
	}
	return out
}
