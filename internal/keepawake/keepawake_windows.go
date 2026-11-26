//go:build windows

package keepawake

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	powrprof       = windows.NewLazySystemDLL("PowrProf.dll")
	powerCreateReq = powrprof.NewProc("PowerCreateRequest")
	powerSetReq    = powrprof.NewProc("PowerSetRequest")
	powerClearReq  = powrprof.NewProc("PowerClearRequest")
)

type powerRequestReason struct {
	Version     uint32
	Flags       uint32
	ReasonCount uint32
	Reasons     uintptr
}

func acquire() (func(), error) {
	for _, p := range []*windows.LazyProc{powerCreateReq, powerSetReq, powerClearReq} {
		if err := p.Find(); err != nil {
			return nil, err
		}
	}
	// REASON_CONTEXT version 0, POWER_REQUEST_CONTEXT_SIMPLE_STRING flag 0x1.
	why, err := windows.UTF16PtrFromString("pvm virtual machines running")
	if err != nil {
		return nil, err
	}
	ctx := struct {
		Version    uint32
		Flags      uint32
		SimpleText *uint16
	}{Version: 0, Flags: 0x1, SimpleText: why}
	h, _, callErr := powerCreateReq.Call(uintptr(unsafe.Pointer(&ctx)))
	if h == 0 || h == uintptr(windows.InvalidHandle) {
		return nil, fmt.Errorf("PowerCreateRequest: %v", callErr)
	}
	// PowerRequestSystemRequired = 1 (allows display sleep and explicit sleep).
	const powerRequestSystemRequired = 1
	ok, _, callErr := powerSetReq.Call(h, powerRequestSystemRequired)
	if ok == 0 {
		windows.CloseHandle(windows.Handle(h))
		return nil, fmt.Errorf("PowerSetRequest: %v", callErr)
	}
	return func() {
		_, _, _ = powerClearReq.Call(h, powerRequestSystemRequired)
		_ = windows.CloseHandle(windows.Handle(h))
	}, nil
}
