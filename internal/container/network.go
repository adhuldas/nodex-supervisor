package container

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

var validNetworkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ContainerNetwork describes a container network managed by Nodexa Agent.
type ContainerNetwork struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Driver    string    `json:"driver"`
	Scope     string    `json:"scope"`
	Subnet    string    `json:"subnet,omitempty"`
	Gateway   string    `json:"gateway,omitempty"`
	Bridge    string    `json:"bridge,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Internal  bool      `json:"internal,omitempty"`
}

// CreateNetworkRequest is the payload accepted by POST /v1/networks.
type CreateNetworkRequest struct {
	Name    string `json:"name"`
	Driver  string `json:"driver,omitempty"`
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// ContainerEndpoint represents a container attached to a network.
type ContainerEndpoint struct {
	ContainerName string          `json:"container_name"`
	NetworkName   string          `json:"network_name"`
	IP            string          `json:"ip"`
	Aliases       []string        `json:"aliases,omitempty"`
	Ports         []ContainerPort `json:"ports,omitempty"`
}

// NetworkManager manages local container networks.
type NetworkManager struct {
	mu           sync.Mutex
	dir          string
	bus          *events.Bus
	cached       map[string]ContainerNetwork
	allocatedIPs map[string]string                       // containerName -> ip
	endpoints    map[string]map[string]ContainerEndpoint // netName -> containerName -> Endpoint
	dnsServers   map[string]*DNSServer                   // netName -> DNSServer
}

func deterministicID(name string) string {
	h := sha256.Sum256([]byte("builtin:" + name))
	return hex.EncodeToString(h[:6])
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewNetworkManager initializes the network manager and loads existing networks.
func NewNetworkManager(dir string, bus *events.Bus) (*NetworkManager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating network dir %s: %w", dir, err)
	}

	nm := &NetworkManager{
		dir:          dir,
		bus:          bus,
		cached:       make(map[string]ContainerNetwork),
		allocatedIPs: make(map[string]string),
		endpoints:    make(map[string]map[string]ContainerEndpoint),
		dnsServers:   make(map[string]*DNSServer),
	}

	now := time.Now().UTC()
	builtins := []ContainerNetwork{
		{
			ID:        deterministicID("bridge"),
			Name:      "bridge",
			Driver:    "bridge",
			Scope:     "local",
			Subnet:    "172.17.0.0/16",
			Gateway:   "172.17.0.1",
			Bridge:    "nodexa0",
			CreatedAt: now,
		},
		{
			ID:        deterministicID("host"),
			Name:      "host",
			Driver:    "host",
			Scope:     "local",
			CreatedAt: now,
		},
		{
			ID:        deterministicID("none"),
			Name:      "none",
			Driver:    "null",
			Scope:     "local",
			CreatedAt: now,
		},
	}

	for _, b := range builtins {
		nm.cached[b.Name] = b
	}

	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var net ContainerNetwork
			if err := json.Unmarshal(data, &net); err == nil && net.Name != "" {
				nm.cached[net.Name] = net
			}
		}
	}

	configureDnsmasqCoexistence()
	nm.ensureBridgesAndDNS()
	return nm, nil
}

func configureDnsmasqCoexistence() {
	if os.Geteuid() != 0 {
		return
	}
	confDir := "/etc/dnsmasq.d"
	if fi, err := os.Stat(confDir); err == nil && fi.IsDir() {
		confPath := filepath.Join(confDir, "nodexa.conf")
		content := "# Configured by nodexa-agent to allow embedded container DNS on bridge interfaces\nbind-interfaces\nexcept-interface=nodexa0,br-*\n"
		existing, _ := os.ReadFile(confPath)
		if string(existing) != content {
			_ = os.WriteFile(confPath, []byte(content), 0o644)
			if _, err := exec.LookPath("systemctl"); err == nil {
				_ = exec.Command("systemctl", "restart", "dnsmasq").Run()
			} else {
				_ = exec.Command("killall", "-HUP", "dnsmasq").Run()
			}
		}
	}
}

func (nm *NetworkManager) ensureBridgesAndDNS() {
	for _, n := range nm.cached {
		if n.Driver == "bridge" && n.Bridge != "" && n.Gateway != "" && n.Subnet != "" {
			_ = ensureLinuxBridge(n.Bridge, n.Gateway, n.Subnet)
			nm.ensureDNSServerLocked(n)
		}
	}
}

func (nm *NetworkManager) ensureDNSServerLocked(netObj ContainerNetwork) {
	if netObj.Driver != "bridge" || netObj.Gateway == "" {
		return
	}
	if _, exists := nm.dnsServers[netObj.Name]; exists {
		return
	}

	listenAddr := netObj.Gateway + ":53"
	server, err := NewDNSServer(listenAddr, netObj.Name, nm.Resolve, nil)
	if err != nil {
		// If running in restricted environment or port 53 in use, log and continue
		log.Printf("nodexa-agent: DNS server on %s not active: %v", listenAddr, err)
		return
	}
	nm.dnsServers[netObj.Name] = server
}

// Close cleanly terminates all running DNS servers.
func (nm *NetworkManager) Close() {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	for name, s := range nm.dnsServers {
		_ = s.Close()
		delete(nm.dnsServers, name)
	}
}

// Resolve looks up a hostname or alias for containers attached to netName.
func (nm *NetworkManager) Resolve(netName, hostname string) (net.IP, bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	eps, ok := nm.endpoints[netName]
	if !ok {
		return nil, false
	}

	hostname = strings.ToLower(strings.TrimSpace(hostname))
	for _, ep := range eps {
		if strings.ToLower(ep.ContainerName) == hostname {
			return net.ParseIP(ep.IP), true
		}
		for _, alias := range ep.Aliases {
			if strings.ToLower(alias) == hostname {
				return net.ParseIP(ep.IP), true
			}
		}
	}
	return nil, false
}

// RegisterContainer records an active container endpoint on a network and sets up port forwarding.
func (nm *NetworkManager) RegisterContainer(netName, containerName, ip string, aliases []string, ports []ContainerPort) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if nm.endpoints[netName] == nil {
		nm.endpoints[netName] = make(map[string]ContainerEndpoint)
	}
	nm.endpoints[netName][containerName] = ContainerEndpoint{
		ContainerName: containerName,
		NetworkName:   netName,
		IP:            ip,
		Aliases:       aliases,
		Ports:         ports,
	}

	nm.setupPortForwardingLocked(ip, ports)
}

// UnregisterContainer removes a container from service discovery and tears down port forwarding.
func (nm *NetworkManager) UnregisterContainer(containerName string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	for _, eps := range nm.endpoints {
		if ep, ok := eps[containerName]; ok {
			nm.teardownPortForwardingLocked(ep.IP, ep.Ports)
			delete(eps, containerName)
			break
		}
	}
}

// SyncNetworkHosts synchronizes /etc/hosts across all running container rootfs trees on netName.
func (nm *NetworkManager) SyncNetworkHosts(netName, containersDir string) {
	nm.mu.Lock()
	epsMap := nm.endpoints[netName]
	var endpoints []ContainerEndpoint
	for _, ep := range epsMap {
		endpoints = append(endpoints, ep)
	}
	nm.mu.Unlock()

	if len(endpoints) == 0 {
		return
	}

	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].ContainerName < endpoints[j].ContainerName
	})

	for _, self := range endpoints {
		hostsPath := filepath.Join(containersDir, self.ContainerName, "rootfs", "etc", "hosts")
		etcDir := filepath.Dir(hostsPath)
		if fi, err := os.Stat(etcDir); err != nil || !fi.IsDir() {
			continue
		}

		var b strings.Builder
		b.WriteString("127.0.0.1 localhost localhost.localdomain\n")
		b.WriteString("::1 localhost localhost.localdomain\n\n")

		// Container's own IP & aliases
		b.WriteString(fmt.Sprintf("%s %s", self.IP, self.ContainerName))
		for _, a := range self.Aliases {
			if a != self.ContainerName {
				b.WriteString(" " + a)
			}
		}
		b.WriteString("\n\n")

		// Other containers on the same network
		hasSiblings := false
		for _, other := range endpoints {
			if other.ContainerName == self.ContainerName {
				continue
			}
			if !hasSiblings {
				b.WriteString(fmt.Sprintf("# Containers on network %s\n", netName))
				hasSiblings = true
			}
			b.WriteString(fmt.Sprintf("%s %s", other.IP, other.ContainerName))
			for _, a := range other.Aliases {
				if a != other.ContainerName {
					b.WriteString(" " + a)
				}
			}
			b.WriteString("\n")
		}

		_ = os.WriteFile(hostsPath, []byte(b.String()), 0o644)
	}
}

func (nm *NetworkManager) setupPortForwardingLocked(ip string, ports []ContainerPort) {
	if _, err := exec.LookPath("iptables"); err != nil || os.Geteuid() != 0 {
		return
	}
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
	for _, p := range ports {
		if p.HostPort <= 0 || p.ContainerPort <= 0 {
			continue
		}
		proto := strings.ToLower(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		hPort := strconv.Itoa(p.HostPort)
		cPort := strconv.Itoa(p.ContainerPort)
		cDest := fmt.Sprintf("%s:%d", ip, p.ContainerPort)

		// PREROUTING (traffic entering external interfaces)
		if exec.Command("iptables", "-t", "nat", "-C", "PREROUTING", "-p", proto, "--dport", hPort, "-j", "DNAT", "--to-destination", cDest).Run() != nil {
			_ = exec.Command("iptables", "-t", "nat", "-A", "PREROUTING", "-p", proto, "--dport", hPort, "-j", "DNAT", "--to-destination", cDest).Run()
		}

		// OUTPUT (traffic generated on the local host machine)
		if exec.Command("iptables", "-t", "nat", "-C", "OUTPUT", "-p", proto, "--dport", hPort, "-j", "DNAT", "--to-destination", cDest).Run() != nil {
			_ = exec.Command("iptables", "-t", "nat", "-A", "OUTPUT", "-p", proto, "--dport", hPort, "-j", "DNAT", "--to-destination", cDest).Run()
		}

		// POSTROUTING masquerade for hairpin NAT
		if exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-p", proto, "-d", ip, "--dport", cPort, "-j", "MASQUERADE").Run() != nil {
			_ = exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-p", proto, "-d", ip, "--dport", cPort, "-j", "MASQUERADE").Run()
		}

		// FORWARD chain to allow forwarded traffic to the container
		if exec.Command("iptables", "-C", "FORWARD", "-p", proto, "-d", ip, "--dport", cPort, "-j", "ACCEPT").Run() != nil {
			_ = exec.Command("iptables", "-A", "FORWARD", "-p", proto, "-d", ip, "--dport", cPort, "-j", "ACCEPT").Run()
		}
	}
}

func (nm *NetworkManager) teardownPortForwardingLocked(ip string, ports []ContainerPort) {
	if _, err := exec.LookPath("iptables"); err != nil || os.Geteuid() != 0 {
		return
	}
	for _, p := range ports {
		if p.HostPort <= 0 || p.ContainerPort <= 0 {
			continue
		}
		proto := strings.ToLower(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		hPort := strconv.Itoa(p.HostPort)
		cPort := strconv.Itoa(p.ContainerPort)
		cDest := fmt.Sprintf("%s:%d", ip, p.ContainerPort)

		_ = exec.Command("iptables", "-t", "nat", "-D", "PREROUTING", "-p", proto, "--dport", hPort, "-j", "DNAT", "--to-destination", cDest).Run()
		_ = exec.Command("iptables", "-t", "nat", "-D", "OUTPUT", "-p", proto, "--dport", hPort, "-j", "DNAT", "--to-destination", cDest).Run()
		_ = exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "-p", proto, "-d", ip, "--dport", cPort, "-j", "MASQUERADE").Run()
		_ = exec.Command("iptables", "-D", "FORWARD", "-p", proto, "-d", ip, "--dport", cPort, "-j", "ACCEPT").Run()
	}
}

// List returns all container networks.
func (nm *NetworkManager) List() []ContainerNetwork {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	var list []ContainerNetwork
	for _, n := range nm.cached {
		list = append(list, n)
	}

	sort.Slice(list, func(i, j int) bool {
		builtinRank := func(name string) int {
			switch name {
			case "bridge":
				return 0
			case "host":
				return 1
			case "none":
				return 2
			default:
				return 3
			}
		}
		r1, r2 := builtinRank(list[i].Name), builtinRank(list[j].Name)
		if r1 != r2 {
			return r1 < r2
		}
		return list[i].Name < list[j].Name
	})

	return list
}

// Get finds a network by name or ID.
func (nm *NetworkManager) Get(nameOrID string) (*ContainerNetwork, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if n, ok := nm.cached[nameOrID]; ok {
		return &n, nil
	}

	for _, n := range nm.cached {
		if n.ID == nameOrID || (len(nameOrID) >= 4 && strings.HasPrefix(n.ID, nameOrID)) {
			return &n, nil
		}
	}

	return nil, fmt.Errorf("network %q not found", nameOrID)
}

// Create creates a new container network.
func (nm *NetworkManager) Create(req CreateNetworkRequest) (*ContainerNetwork, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("network name cannot be empty")
	}
	if !validNetworkName.MatchString(name) {
		return nil, fmt.Errorf("invalid network name %q: must start with alphanumeric character and contain only [a-zA-Z0-9_.-]", name)
	}

	if _, exists := nm.cached[name]; exists {
		return nil, fmt.Errorf("network %q already exists", name)
	}

	driver := strings.TrimSpace(req.Driver)
	if driver == "" {
		driver = "bridge"
	}
	if driver != "bridge" && driver != "host" && driver != "null" {
		return nil, fmt.Errorf("unsupported network driver %q (supported: bridge, host, null)", driver)
	}

	id := randomID()
	netObj := ContainerNetwork{
		ID:        id,
		Name:      name,
		Driver:    driver,
		Scope:     "local",
		CreatedAt: time.Now().UTC(),
	}

	if driver == "bridge" {
		subnet := strings.TrimSpace(req.Subnet)
		gateway := strings.TrimSpace(req.Gateway)

		if subnet == "" {
			var err error
			subnet, gateway, err = nm.allocateSubnetLocked()
			if err != nil {
				return nil, err
			}
		} else {
			ip, _, err := net.ParseCIDR(subnet)
			if err != nil {
				return nil, fmt.Errorf("invalid subnet CIDR %q: %w", subnet, err)
			}
			if gateway == "" {
				ip4 := ip.To4()
				if ip4 == nil {
					return nil, fmt.Errorf("only IPv4 subnets are currently supported")
				}
				ip4[3] = 1
				gateway = ip4.String()
			}
		}

		bridgeName := "br-" + id[:10]
		netObj.Subnet = subnet
		netObj.Gateway = gateway
		netObj.Bridge = bridgeName

		if err := ensureLinuxBridge(bridgeName, gateway, subnet); err != nil {
			log.Printf("warning: creating linux bridge %s: %v", bridgeName, err)
		}
		nm.ensureDNSServerLocked(netObj)
	}

	data, err := json.MarshalIndent(netObj, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling network: %w", err)
	}
	file := filepath.Join(nm.dir, id+".json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return nil, fmt.Errorf("writing network file %s: %w", file, err)
	}

	nm.cached[name] = netObj

	if nm.bus != nil {
		nm.bus.Emit("NODEXA_NETWORK_CREATED", fmt.Sprintf("container network %q created (%s)", name, id), map[string]string{
			"name":   name,
			"id":     id,
			"driver": driver,
		})
	}

	return &netObj, nil
}

func (nm *NetworkManager) allocateSubnetLocked() (subnet, gateway string, err error) {
	usedSubnets := make(map[string]bool)
	for _, n := range nm.cached {
		if n.Subnet != "" {
			usedSubnets[n.Subnet] = true
		}
	}

	for i := 18; i <= 31; i++ {
		candidate := fmt.Sprintf("172.%d.0.0/16", i)
		if !usedSubnets[candidate] {
			return candidate, fmt.Sprintf("172.%d.0.1", i), nil
		}
	}

	for i := 0; i <= 255; i++ {
		candidate := fmt.Sprintf("10.89.%d.0/24", i)
		if !usedSubnets[candidate] {
			return candidate, fmt.Sprintf("10.89.%d.1", i), nil
		}
	}

	return "", "", fmt.Errorf("no available subnets left for automatic allocation")
}

// Remove removes a container network.
func (nm *NetworkManager) Remove(nameOrID string) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	var target *ContainerNetwork
	for _, n := range nm.cached {
		if n.Name == nameOrID || n.ID == nameOrID || (len(nameOrID) >= 4 && strings.HasPrefix(n.ID, nameOrID)) {
			nCopy := n
			target = &nCopy
			break
		}
	}

	if target == nil {
		return fmt.Errorf("network %q not found", nameOrID)
	}

	if target.Name == "bridge" || target.Name == "host" || target.Name == "none" {
		return fmt.Errorf("predefined network %q cannot be removed", target.Name)
	}

	if s, ok := nm.dnsServers[target.Name]; ok {
		_ = s.Close()
		delete(nm.dnsServers, target.Name)
	}

	if target.Bridge != "" {
		removeLinuxBridge(target.Bridge, target.Subnet)
	}

	file := filepath.Join(nm.dir, target.ID+".json")
	_ = os.Remove(file)

	delete(nm.cached, target.Name)
	delete(nm.endpoints, target.Name)

	if nm.bus != nil {
		nm.bus.Emit("NODEXA_NETWORK_REMOVED", fmt.Sprintf("container network %q removed", target.Name), map[string]string{
			"name": target.Name,
			"id":   target.ID,
		})
	}

	return nil
}

func ensureLinuxBridge(brName, gateway, subnet string) error {
	if _, err := exec.LookPath("ip"); err != nil {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil
	}

	bridgeCreated := false
	if _, err := net.InterfaceByName(brName); err != nil {
		if out, err := exec.Command("ip", "link", "add", "name", brName, "type", "bridge").CombinedOutput(); err != nil {
			return fmt.Errorf("ip link add %s: %s: %w", brName, strings.TrimSpace(string(out)), err)
		}
		bridgeCreated = true
	}

	cidrBits := "16"
	if parts := strings.Split(subnet, "/"); len(parts) == 2 {
		cidrBits = parts[1]
	}
	if bridgeCreated {
		_ = exec.Command("ip", "addr", "add", gateway+"/"+cidrBits, "dev", brName).Run()
	}
	_ = exec.Command("ip", "link", "set", brName, "up").Run()

	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)

	if _, err := exec.LookPath("iptables"); err == nil {
		if exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", subnet, "!", "-o", brName, "-j", "MASQUERADE").Run() != nil {
			_ = exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "!", "-o", brName, "-j", "MASQUERADE").Run()
		}
		if exec.Command("iptables", "-C", "FORWARD", "-i", brName, "-j", "ACCEPT").Run() != nil {
			_ = exec.Command("iptables", "-A", "FORWARD", "-i", brName, "-j", "ACCEPT").Run()
		}
		if exec.Command("iptables", "-C", "FORWARD", "-o", brName, "-j", "ACCEPT").Run() != nil {
			_ = exec.Command("iptables", "-A", "FORWARD", "-o", brName, "-j", "ACCEPT").Run()
		}
	}

	return nil
}

func removeLinuxBridge(brName, subnet string) {
	if _, err := exec.LookPath("ip"); err != nil {
		return
	}
	if os.Geteuid() != 0 {
		return
	}

	_ = exec.Command("ip", "link", "set", brName, "down").Run()
	_ = exec.Command("ip", "link", "delete", brName, "type", "bridge").Run()

	if _, err := exec.LookPath("iptables"); err == nil && subnet != "" {
		_ = exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet, "!", "-o", brName, "-j", "MASQUERADE").Run()
	}
}

// EnsureNetwork finds an existing network by name or ID, or creates it automatically
// if it does not already exist.
func (nm *NetworkManager) EnsureNetwork(name string) (*ContainerNetwork, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("network name cannot be empty")
	}

	netObj, err := nm.Get(name)
	if err == nil {
		if netObj.Driver == "bridge" && netObj.Bridge != "" && netObj.Gateway != "" && netObj.Subnet != "" {
			_ = ensureLinuxBridge(netObj.Bridge, netObj.Gateway, netObj.Subnet)
			nm.mu.Lock()
			nm.ensureDNSServerLocked(*netObj)
			nm.mu.Unlock()
		}
		return netObj, nil
	}

	// Not found - automatically create it
	return nm.Create(CreateNetworkRequest{
		Name:   name,
		Driver: "bridge",
	})
}

// AllocateContainerIP assigns an available IPv4 address from netObj's subnet to containerName.
func (nm *NetworkManager) AllocateContainerIP(netObj *ContainerNetwork, containerName string) (string, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if ip, ok := nm.allocatedIPs[containerName]; ok {
		return ip, nil
	}

	ip, ipNet, err := net.ParseCIDR(netObj.Subnet)
	if err != nil {
		return "", fmt.Errorf("invalid subnet %q: %w", netObj.Subnet, err)
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return "", fmt.Errorf("only IPv4 subnets supported")
	}

	used := make(map[string]bool)
	used[netObj.Gateway] = true
	for _, assigned := range nm.allocatedIPs {
		used[assigned] = true
	}

	curIP := make(net.IP, len(ip4))
	copy(curIP, ip4)
	curIP[3] = 2 // Start scanning from .2

	for ipNet.Contains(curIP) {
		ipStr := curIP.String()
		if !used[ipStr] {
			nm.allocatedIPs[containerName] = ipStr
			return ipStr, nil
		}
		for j := len(curIP) - 1; j >= 0; j-- {
			curIP[j]++
			if curIP[j] > 0 {
				break
			}
		}
	}

	return "", fmt.Errorf("no available IP addresses in subnet %s", netObj.Subnet)
}

// ReserveContainerIP claims ip for containerName unless another container
// holds it, so a restarted container gets back the address it had (and
// that its siblings' /etc/hosts may still point at) rather than the next
// free one. It reports whether containerName now holds ip.
func (nm *NetworkManager) ReserveContainerIP(containerName, ip string) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if cur, ok := nm.allocatedIPs[containerName]; ok {
		return cur == ip
	}
	for _, assigned := range nm.allocatedIPs {
		if assigned == ip {
			return false
		}
	}
	nm.allocatedIPs[containerName] = ip
	return true
}

// ReleaseContainerIP frees the allocated IP for containerName.
func (nm *NetworkManager) ReleaseContainerIP(containerName string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	delete(nm.allocatedIPs, containerName)
}

// nsfsMagic is NSFS_MAGIC from linux/magic.h: the filesystem type of a
// bind-mounted namespace file.
const nsfsMagic = 0x6e736673


func cleanNetns(netnsName string) {
	netnsPath := "/run/netns/" + netnsName
	_ = exec.Command("ip", "netns", "del", netnsName).Run()
	for i := 0; i < 5; i++ {
		if err := exec.Command("umount", "-l", netnsPath).Run(); err != nil {
			break
		}
	}
	_ = os.Remove(netnsPath)
}

// SetupContainerNetwork creates a network namespace, veth pair, and default routing
// for containerName within netObj. It returns the path to the network namespace
// (/run/netns/nodexa-<containerName>) and the container's IP address.
func (nm *NetworkManager) SetupContainerNetwork(containerName string, netObj *ContainerNetwork) (string, string, error) {
	log.Printf("SetupContainerNetwork: container=%s bridge=%q netObj=%+v", containerName, netObj.Bridge, netObj)
	netnsName := "nodexa-" + containerName
	netnsPath := "/run/netns/" + netnsName

	ipStr, err := nm.AllocateContainerIP(netObj, containerName)
	if err != nil {
		return "", "", fmt.Errorf("allocating IP for container %q: %w", containerName, err)
	}

	if _, err := exec.LookPath("ip"); err != nil || os.Geteuid() != 0 {
		// In dev/test environments without Linux ip/root capabilities, return the netns path & IP
		return netnsPath, ipStr, nil
	}

	_ = os.MkdirAll("/run/netns", 0o755)

	// Clean up any stale netns
	cleanNetns(netnsName)
	if _, err := os.Stat(netnsPath); err == nil {
		// Still mounted from another mount namespace (e.g. a second agent
		// started by hand), so the unlink got EBUSY.
		return "", "", fmt.Errorf("netns %s is still held by another mount namespace; is a second nodexa-agent running?", netnsPath)
	}

	h := sha256.Sum256([]byte(containerName))
	cHash := hex.EncodeToString(h[:4])
	vethHost := "vh" + cHash
	vethPeer := "vp" + cHash

	_ = exec.Command("ip", "link", "delete", vethHost).Run()

	if out, err := exec.Command("ip", "netns", "add", netnsName).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("creating netns %s: %s: %w", netnsName, strings.TrimSpace(string(out)), err)
	}
	if !isNetnsMount(netnsPath) {
		_ = exec.Command("ip", "netns", "del", netnsName).Run()
		return "", "", fmt.Errorf("creating netns %s: %s is not a namespace mount after ip netns add", netnsName, netnsPath)
	}

	if netObj.Bridge != "" {
		_ = ensureLinuxBridge(netObj.Bridge, netObj.Gateway, netObj.Subnet)
	}

	if out, err := exec.Command("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethPeer).CombinedOutput(); err != nil {
		_ = exec.Command("ip", "netns", "del", netnsName).Run()
		return "", "", fmt.Errorf("creating veth pair %s/%s: %s: %w", vethHost, vethPeer, strings.TrimSpace(string(out)), err)
	}

	if out, err := exec.Command("ip", "link", "set", vethPeer, "netns", netnsName).CombinedOutput(); err != nil {
		_ = exec.Command("ip", "link", "delete", vethHost).Run()
		_ = exec.Command("ip", "netns", "del", netnsName).Run()
		return "", "", fmt.Errorf("moving veth to netns %s: %s: %w", netnsName, strings.TrimSpace(string(out)), err)
	}

	if netObj.Bridge != "" {
		out, err := exec.Command("ip", "link", "set", vethHost, "master", netObj.Bridge).CombinedOutput()
		log.Printf("ip link set %s master %s: out=%q err=%v", vethHost, netObj.Bridge, strings.TrimSpace(string(out)), err)
		_ = exec.Command("ip", "link", "set", netObj.Bridge, "up").Run()
	}
	_ = exec.Command("ip", "link", "set", vethHost, "up").Run()

	// Inside container netns: bring up loopback
	_ = exec.Command("ip", "-n", netnsName, "link", "set", "lo", "up").Run()

	// Rename peer to eth0
	_ = exec.Command("ip", "-n", netnsName, "link", "set", vethPeer, "name", "eth0").Run()

	cidrBits := "16"
	if parts := strings.Split(netObj.Subnet, "/"); len(parts) == 2 {
		cidrBits = parts[1]
	}

	// Assign IP
	if out, err := exec.Command("ip", "-n", netnsName, "addr", "add", ipStr+"/"+cidrBits, "dev", "eth0").CombinedOutput(); err != nil {
		_ = exec.Command("ip", "link", "delete", vethHost).Run()
		_ = exec.Command("ip", "netns", "del", netnsName).Run()
		return "", "", fmt.Errorf("assigning IP %s to eth0 in netns %s: %s: %w", ipStr, netnsName, strings.TrimSpace(string(out)), err)
	}

	_ = exec.Command("ip", "-n", netnsName, "link", "set", "eth0", "up").Run()

	if netObj.Gateway != "" {
		_ = exec.Command("ip", "-n", netnsName, "route", "add", "default", "via", netObj.Gateway).Run()
	}

	return netnsPath, ipStr, nil
}

// EnsureContainerBridgeAttached ensures that the host side of a container's veth pair
// is attached to the given bridge and set UP. This is called on startLocked/recovery.
func (nm *NetworkManager) EnsureContainerBridgeAttached(containerName, bridgeName string) {
	if bridgeName == "" {
		return
	}
	h := sha256.Sum256([]byte(containerName))
	vethHost := "vh" + hex.EncodeToString(h[:4])
	if _, err := net.InterfaceByName(vethHost); err == nil {
		_ = exec.Command("ip", "link", "set", vethHost, "master", bridgeName).Run()
		_ = exec.Command("ip", "link", "set", vethHost, "up").Run()
		_ = exec.Command("ip", "link", "set", bridgeName, "up").Run()
	}
}

// TeardownContainerNetwork removes the container's netns and host veth, frees its IP,
// unregisters it from service discovery, and cleans up port forwarding.
func (nm *NetworkManager) TeardownContainerNetwork(containerName string) error {
	nm.ReleaseContainerIP(containerName)
	nm.UnregisterContainer(containerName)

	if _, err := exec.LookPath("ip"); err != nil || os.Geteuid() != 0 {
		return nil
	}

	netnsName := "nodexa-" + containerName
	cleanNetns(netnsName)

	h := sha256.Sum256([]byte(containerName))
	cHash := hex.EncodeToString(h[:4])
	vethHost := "vh" + cHash
	_ = exec.Command("ip", "link", "delete", vethHost).Run()

	return nil
}
