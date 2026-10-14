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
	"github.com/huangzheng2016/pvm/internal/gw/rules"
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
	// Rules is the routing rule set (fake-ip engine input); VMExits pins
	// a VM's default route to an exit-node name.
	Rules   []rules.Rule      `json:"rules,omitempty"`
	VMExits map[string]string `json:"vm_exits,omitempty"`
	// DefaultExit is the cluster-wide fallback route used when neither a
	// rule nor a per-VM exit matches ("" = direct).
	DefaultExit string `json:"default_exit,omitempty"`
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

func (s *Store) Ensure(id int) (*Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.data.Clusters[id]; ok {
		return c.deepCopy(), nil
	}
	if id < 1 || id > 254 {
		return nil, fmt.Errorf("cluster id %d out of range", id)
	}
	poolBase, err := freePoolWindow(poolBasePerCluster+(id-1)*poolStride, PoolSize)
	if err != nil {
		return nil, err
	}
	c := &Cluster{
		ID:       id,
		CIDR:     fmt.Sprintf("100.99.%d.0/24", id),
		GwIP:     fmt.Sprintf("100.99.%d.1", id),
		Gateway:  fmt.Sprintf("%s%d", GatewayVMNamePrefix, id),
		PoolBase: poolBase,
		PoolSize: PoolSize,
		VMs:      map[string]*VMNet{},
	}
	s.data.Clusters[id] = c
	return c.deepCopy(), s.save()
}

// freePoolWindow finds a PoolSize-wide window of loopback ports that is
// currently bindable (tcp+udp), starting at preferred and striding by
// PoolSize. The gateway's slirp hostfwd binds the whole window, so
// colliding with another pvm environment's gateway must be detected
// here rather than at qemu start.
func freePoolWindow(preferred, size int) (int, error) {
	for base := preferred; base+size <= 65535; base += size {
		if poolWindowFree(base, size) {
			return base, nil
		}
	}
	return 0, fmt.Errorf("no free %d-wide host port window from %d", size, preferred)
}

func poolWindowFree(base, size int) bool {
	for p := base; p < base+size; p++ {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		tc, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		tc.Close()
		uc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		uc.Close()
	}
	return true
}

// deepCopy returns an independent copy safe to read outside the store
// lock; mutations must go through Store methods.
func (c *Cluster) deepCopy() *Cluster {
	if c == nil {
		return nil
	}
	out := *c
	out.VMs = make(map[string]*VMNet, len(c.VMs))
	for k, v := range c.VMs {
		vv := *v
		out.VMs[k] = &vv
	}
	out.Maps = make([]*PortMap, len(c.Maps))
	for i, m := range c.Maps {
		mm := *m
		out.Maps[i] = &mm
	}
	out.Rules = append([]rules.Rule(nil), c.Rules...)
	out.VMExits = make(map[string]string, len(c.VMExits))
	for k, v := range c.VMExits {
		out.VMExits[k] = v
	}
	return &out
}

func (s *Store) Get(id int) *Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Clusters[id].deepCopy()
}

func (s *Store) List() []*Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Cluster
	for _, c := range s.data.Clusters {
		out = append(out, c.deepCopy())
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

// Dell's OUI hides QEMU's hypervisor vendor tell from guests reading
// their own NIC address.
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
		IP:   fmt.Sprintf("100.99.%d.%d", c.ID, last),
		MAC:  fmt.Sprintf("10:65:30:%02x:%02x:%02x", c.ID, last>>8, last&0xff),
	}
	c.VMs[name] = vn
	out := *vn
	return &out, s.save()
}

func (s *Store) RemoveVM(id int, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return nil
	}
	delete(c.VMs, name)
	delete(c.VMExits, name)
	var maps []*PortMap
	for _, m := range c.Maps {
		if m.VM != name {
			maps = append(maps, m)
		}
	}
	c.Maps = maps
	return s.save()
}

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
	out := *m
	return &out, s.save()
}

func (s *Store) SetMaps(id int, maps []*PortMap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return fmt.Errorf("cluster %d does not exist", id)
	}
	c.Maps = make([]*PortMap, len(maps))
	for i, m := range maps {
		mm := *m
		c.Maps[i] = &mm
	}
	return s.save()
}

func (s *Store) RemoveMap(id int, hostPort int, proto string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data.Clusters[id]
	if c == nil {
		return false, nil
	}
	removed := false
	var maps []*PortMap
	for _, m := range c.Maps {
		if m.HostPort == hostPort && m.Proto == proto {
			removed = true
			continue
		}
		maps = append(maps, m)
	}
	c.Maps = maps
	return removed, s.save()
}
