package containuum

import (
	"encoding/binary"
	"hash/fnv"
	"sort"
)

// Container represents a Docker container's relevant state.
type Container struct {
	ID       string            // Full container ID
	Name     string            // Container name (without leading slash)
	Image    string            // Image name (e.g., "nginx:latest")
	State    string            // Container state (e.g., "running", "exited", "paused")
	Labels   map[string]string // Container labels
	Networks []Network         // All connected networks
	Ports    []Port            // Published port mappings
}

// hash computes a hash of the Container.
func (c *Container) hash() uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(c.ID))
	_, _ = h.Write([]byte(c.Name))
	_, _ = h.Write([]byte(c.Image))
	_, _ = h.Write([]byte(c.State))

	if len(c.Labels) > 0 {
		keys := make([]string, 0, len(c.Labels))
		for k := range c.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = h.Write([]byte(k))
			_, _ = h.Write([]byte(c.Labels[k]))
		}
	}

	var networksHash uint64
	for _, network := range c.Networks {
		networksHash ^= network.hash()
	}
	_ = binary.Write(h, binary.LittleEndian, networksHash)

	var portsHash uint64
	for _, port := range c.Ports {
		portsHash ^= port.hash()
	}
	_ = binary.Write(h, binary.LittleEndian, portsHash)

	return h.Sum64()
}

// Network represents a container's connection to a Docker network.
type Network struct {
	Name       string   // Network name
	ID         string   // Network ID
	IPAddress  string   // IPv4 address on this network
	IP6Address string   // IPv6 address on this network (if any)
	Gateway    string   // Gateway for this network
	Aliases    []string // Container's DNS aliases on this network
}

// hash computes a hash of the Network.
func (n *Network) hash() uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(n.Name))
	_, _ = h.Write([]byte(n.ID))
	_, _ = h.Write([]byte(n.IPAddress))
	_, _ = h.Write([]byte(n.IP6Address))
	_, _ = h.Write([]byte(n.Gateway))

	if len(n.Aliases) > 0 {
		aliases := make([]string, len(n.Aliases))
		copy(aliases, n.Aliases)
		sort.Strings(aliases)
		for _, alias := range aliases {
			_, _ = h.Write([]byte(alias))
		}
	}

	return h.Sum64()
}

// Port represents a port mapping.
type Port struct {
	HostIP        string // Host IP (e.g., "0.0.0.0")
	HostPort      uint16 // Port on host
	ContainerPort uint16 // Port in container
	Protocol      string // "tcp" or "udp"
}

// hash computes a hash of the Port.
func (p *Port) hash() uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.HostIP))
	_ = binary.Write(h, binary.LittleEndian, p.HostPort)
	_ = binary.Write(h, binary.LittleEndian, p.ContainerPort)
	_, _ = h.Write([]byte(p.Protocol))
	return h.Sum64()
}
