package cluster

import (
	"fmt"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func (s *Store) AddRule(id int, r rules.Rule) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return 0, fmt.Errorf("cluster %d does not exist", id)
	}
	previous := c.deepCopy()
	for i, e := range c.Rules {
		if e.Type == r.Type && e.Pattern == r.Pattern {
			c.Rules[i] = r
			return i, s.saveCluster(id, previous)
		}
	}
	c.Rules = append(c.Rules, r)
	return len(c.Rules) - 1, s.saveCluster(id, previous)
}

func (s *Store) SetRules(id int, rs []rules.Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return fmt.Errorf("cluster %d does not exist", id)
	}
	previous := c.deepCopy()
	c.Rules = append([]rules.Rule(nil), rs...)
	return s.saveCluster(id, previous)
}

// RemoveRule drops the rule at index, reporting whether it existed.
func (s *Store) RemoveRule(id, index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return fmt.Errorf("cluster %d does not exist", id)
	}
	previous := c.deepCopy()
	if index < 0 || index >= len(c.Rules) {
		return fmt.Errorf("rule index %d out of range", index)
	}
	c.Rules = append(c.Rules[:index], c.Rules[index+1:]...)
	return s.saveCluster(id, previous)
}

// SetVMExit pins a VM's default route. An explicit "direct" is kept so
// the VM does not inherit a later cluster default; "" removes the override.
func (s *Store) SetVMExit(id int, vmName, exit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return fmt.Errorf("cluster %d does not exist", id)
	}
	previous := c.deepCopy()
	if c.VMExits == nil {
		c.VMExits = map[string]string{}
	}
	if exit == "" {
		delete(c.VMExits, vmName)
	} else {
		c.VMExits[vmName] = exit
	}
	return s.saveCluster(id, previous)
}

// SetDefaultExit sets the cluster-wide fallback route; "" or "direct"
// restores direct egress for unmatched traffic.
func (s *Store) SetDefaultExit(id int, exit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return fmt.Errorf("cluster %d does not exist", id)
	}
	previous := c.deepCopy()
	if exit == "direct" {
		exit = ""
	}
	c.DefaultExit = exit
	return s.saveCluster(id, previous)
}
