// Package cluster manages cluster state: subnet allocation, per-VM
// IP/MAC assignment, gateway port pools and port maps. State persists
// to ~/.pvm/clusters/clusters.json (atomic writes).
package cluster

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

const (
	GatewayVMNamePrefix = "pvm-gw"
	DefaultClusterID    = 1
	poolBasePerCluster  = 30000
	poolStride          = 1000
	PoolSize            = 100
	vmIPBase            = 10 // first VM is .10
	vmIPMax             = 73 // 64 slots, matching gateway hotplug root ports
)

type VMNet struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	IP   string `json:"ip"`
}

type PortMap struct {
	VM        string `json:"vm"`
	VMIP      string `json:"vm_ip"`
	HostPort  int    `json:"host_port"` // user-facing, daemon host relay
	PoolPort  int    `json:"pool_port"` // gateway slirp hostfwd + DNAT
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

type Cluster struct {
	ID       int               `json:"id"`
	CIDR     string            `json:"cidr"`
	GwIP     string            `json:"gw_ip"`
	Gateway  string            `json:"gateway"` // gateway VM name
	PoolBase int               `json:"pool_base"`
	PoolSize int               `json:"pool_size"`
	VMs      map[string]*VMNet `json:"vms"`
	Maps     []*PortMap        `json:"maps"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data *storeData
}

type storeData struct {
	Clusters map[int]*Cluster `json:"clusters"`
}

func LoadStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "clusters.json")}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.data = &storeData{Clusters: map[int]*Cluster{}}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var d storeData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	if d.Clusters == nil {
		d.Clusters = map[int]*Cluster{}
	}
	s.data = &d
	return s, nil
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, data, 0o600)
}

// Ensure returns the cluster, creating it (with subnet) if missing.
func (s *Store) Ensure(id int) (*Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.data.Clusters[id]; ok {
		return c, nil
	}
	if id < 1 || id > 254 {
		return nil, fmt.Errorf("cluster id %d out of range", id)
	}
	c := &Cluster{
		ID:       id,
		CIDR:     fmt.Sprintf("192.168.%d.0/24", id),
		GwIP:     fmt.Sprintf("192.168.%d.1", id),
		Gateway:  fmt.Sprintf("%s%d", GatewayVMNamePrefix, id),
		PoolBase: poolBasePerCluster + (id-1)*poolStride,
		PoolSize: PoolSize,
		VMs:      map[string]*VMNet{},
	}
	s.data.Clusters[id] = c
	return c, s.save()
}

func (s *Store) Get(id int) *Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Clusters[id]
}

func (s *Store) List() []*Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Cluster
	for _, c := range s.data.Clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) LinksDir(id int) string {
	return filepath.Join(filepath.Dir(s.path), fmt.Sprint(id), "links")
}

func (s *Store) LinkSock(id int, vmName string) string {
	return filepath.Join(s.LinksDir(id), vmName+".sock")
}

// AddVM assigns the lowest free IP (from .10) and a derived MAC.
func (s *Store) AddVM(id int, name string) (*VMNet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return nil, fmt.Errorf("cluster %d does not exist", id)
	}
	if v, ok := c.VMs[name]; ok {
		return v, nil
	}
	used := map[int]bool{}
	for _, v := range c.VMs {
		ip := net.ParseIP(v.IP).To4()
		if ip != nil {
			used[int(ip[3])] = true
		}
	}
	var last int
	for last = vmIPBase; last <= vmIPMax; last++ {
		if !used[last] {
			break
		}
	}
	if last > vmIPMax {
		return nil, fmt.Errorf("cluster %d out of IPs", id)
	}
	vn := &VMNet{
		Name: name,
		IP:   fmt.Sprintf("192.168.%d.%d", c.ID, last),
		MAC:  fmt.Sprintf("52:54:00:%02x:%02x:%02x", c.ID, last>>8, last&0xff),
	}
	c.VMs[name] = vn
	return vn, s.save()
}

func (s *Store) RemoveVM(id int, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return nil
	}
	delete(c.VMs, name)
	var maps []*PortMap
	for _, m := range c.Maps {
		if m.VM != name {
			maps = append(maps, m)
		}
	}
	c.Maps = maps
	return s.save()
}

// AddMap allocates a pool port and records the map. Requested host port
// 0 means auto-assign from 127.0.0.1-bindable ports outside the pool.
func (s *Store) AddMap(id int, vmName, vmIP string, hostPort, guestPort int, proto string) (*PortMap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return nil, fmt.Errorf("cluster %d does not exist", id)
	}
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return nil, fmt.Errorf("proto must be tcp|udp")
	}
	poolUsed := map[int]bool{}
	for _, m := range c.Maps {
		poolUsed[m.PoolPort] = true
		if m.HostPort == hostPort && m.Proto == proto {
			return nil, fmt.Errorf("host port %d/%s already mapped", hostPort, proto)
		}
	}
	var pool int
	for pool = c.PoolBase; pool < c.PoolBase+c.PoolSize; pool++ {
		if !poolUsed[pool] {
			break
		}
	}
	if pool >= c.PoolBase+c.PoolSize {
		return nil, fmt.Errorf("cluster %d port pool exhausted", id)
	}
	m := &PortMap{VM: vmName, VMIP: vmIP, HostPort: hostPort, PoolPort: pool, GuestPort: guestPort, Proto: proto}
	c.Maps = append(c.Maps, m)
	return m, s.save()
}

func (s *Store) RemoveMap(id int, hostPort int, proto string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return nil
	}
	var maps []*PortMap
	for _, m := range c.Maps {
		if !(m.HostPort == hostPort && m.Proto == proto) {
			maps = append(maps, m)
		}
	}
	c.Maps = maps
	return s.save()
}
