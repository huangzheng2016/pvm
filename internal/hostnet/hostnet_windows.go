//go:build windows

package hostnet

import (
	"fmt"
	"math/bits"
	"net"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi   = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIf2 = iphlpapi.NewProc("GetIfEntry2")
)

type ifRow2 struct {
	InterfaceLuid        uint64
	InterfaceIndex       uint32
	InterfaceGuid        windows.GUID
	Alias                [257]uint16
	Description          [257]uint16
	PhysicalAddressLen   uint32
	PhysicalAddress      [32]byte
	PermanentAddress     [32]byte
	Mtu                  uint32
	Type                 uint32
	TunnelType           uint32
	MediaType            uint32
	PhysicalMediumType   uint32
	AccessType           uint32
	DirectionType        uint32
	InterfaceAndOperFlag uint8
	_                    [495]byte
}

func isHardwareAdapter(index uint32) bool {
	if procGetIf2.Find() != nil {
		return false
	}
	var row ifRow2
	row.InterfaceIndex = index
	hr, _, _ := procGetIf2.Call(uintptr(unsafe.Pointer(&row)))
	if hr != 0 {
		return false
	}
	// HardwareInterface is bit 2 of InterfaceAndOperFlag (0x04).
	return row.InterfaceAndOperFlag&0x04 != 0
}

func selectPhysical() (*Interface, error) {
	ifcs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	macToName := map[string]string{}
	var hwMACs []string
	for _, ifc := range ifcs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		mac := strings.ToLower(ifc.HardwareAddr.String())
		macToName[mac] = ifc.Name
		if isHardwareAdapter(uint32(ifc.Index)) {
			hwMACs = append(hwMACs, mac)
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
					if !IsVirtual(ifc.Name) {
						return &Interface{Name: ifc.Name, Index: ifc.Index, IPv4: ipn.IP.String()}, nil
					}
				}
			}
		}
	}
	// Hyper-V external switch: the physical NIC loses its IPv4 to a same-MAC
	// management vEthernet adapter. Accept only that exact MAC twin.
	for _, mac := range hwMACs {
		name := macToName[mac]
		ifc, err := net.InterfaceByName(name)
		if err != nil || !strings.HasPrefix(strings.ToLower(ifc.Name), "vethernet") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				return &Interface{Name: ifc.Name, Index: ifc.Index, IPv4: ipn.IP.String()}, nil
			}
		}
	}
	return nil, fmt.Errorf("no physical IPv4 adapter found")
}

func bindControl(_ string, index int) func(string, string, syscall.RawConn) error {
	const ipUnicastIF = 31
	return func(network, address string, c syscall.RawConn) error {
		if skipBindForAddress(address) {
			return nil
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			// IP_UNICAST_IF expects the 4-byte index in network byte order.
			serr = windows.SetsockoptInt(windows.Handle(fd), syscall.IPPROTO_IP, ipUnicastIF, int(bits.ReverseBytes32(uint32(index))))
		}); err != nil {
			return err
		}
		return serr
	}
}
