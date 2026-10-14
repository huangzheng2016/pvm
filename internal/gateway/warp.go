package gateway

import "net"

func (m *exitManager) TunnelDNSAddr(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tuns[name]
	if !ok || t.dev == nil || t.def.DNS == "" {
		return "", false
	}
	return net.JoinHostPort(t.def.DNS, "53"), true
}
