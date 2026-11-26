// Package keepawake prevents idle system sleep while VMs or interactive
// sessions are active. It never blocks screen locking or explicit user sleep
// (such as closing a laptop lid).
package keepawake

import (
	"sync"
)

// Manager holds at most one platform sleep assertion, keyed by active state.
type Manager struct {
	mu      sync.Mutex
	active  bool
	release func()
}

func NewManager() *Manager { return &Manager{} }

// SetActive acquires the sleep assertion when active becomes true and
// releases it when active becomes false. Acquisition is best-effort.
func (m *Manager) SetActive(active bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if active == m.active {
		return
	}
	if !active {
		if m.release != nil {
			m.release()
			m.release = nil
		}
		m.active = false
		return
	}
	rel, err := acquire()
	if err != nil {
		return
	}
	m.release = rel
	m.active = true
}

func (m *Manager) Close() { m.SetActive(false) }
