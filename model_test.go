package containuum

import "testing"

func TestPortHash(t *testing.T) {
	t.Run("identical ports produce same hash", func(t *testing.T) {
		p1 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
		p2 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}

		if p1.hash() != p2.hash() {
			t.Error("identical ports should produce the same hash")
		}
	})

	t.Run("different host IP produces different hash", func(t *testing.T) {
		p1 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
		p2 := Port{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}

		if p1.hash() == p2.hash() {
			t.Error("different host IPs should produce different hashes")
		}
	})

	t.Run("different host port produces different hash", func(t *testing.T) {
		p1 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
		p2 := Port{HostIP: "0.0.0.0", HostPort: 8081, ContainerPort: 80, Protocol: "tcp"}

		if p1.hash() == p2.hash() {
			t.Error("different host ports should produce different hashes")
		}
	})

	t.Run("different container port produces different hash", func(t *testing.T) {
		p1 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
		p2 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 443, Protocol: "tcp"}

		if p1.hash() == p2.hash() {
			t.Error("different container ports should produce different hashes")
		}
	})

	t.Run("different protocol produces different hash", func(t *testing.T) {
		p1 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
		p2 := Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "udp"}

		if p1.hash() == p2.hash() {
			t.Error("different protocols should produce different hashes")
		}
	})
}

func TestNetworkHash(t *testing.T) {
	t.Run("identical networks produce same hash", func(t *testing.T) {
		n1 := Network{
			Name:       "bridge",
			ID:         "abc123",
			IPAddress:  "172.17.0.2",
			IP6Address: "fe80::1",
			Gateway:    "172.17.0.1",
			Aliases:    []string{"web", "api"},
		}
		n2 := Network{
			Name:       "bridge",
			ID:         "abc123",
			IPAddress:  "172.17.0.2",
			IP6Address: "fe80::1",
			Gateway:    "172.17.0.1",
			Aliases:    []string{"web", "api"},
		}

		if n1.hash() != n2.hash() {
			t.Error("identical networks should produce the same hash")
		}
	})

	t.Run("aliases in different order produce same hash", func(t *testing.T) {
		n1 := Network{
			Name:    "bridge",
			ID:      "abc123",
			Aliases: []string{"web", "api", "service"},
		}
		n2 := Network{
			Name:    "bridge",
			ID:      "abc123",
			Aliases: []string{"api", "service", "web"},
		}

		if n1.hash() != n2.hash() {
			t.Error("aliases in different order should produce the same hash (order-independent)")
		}
	})

	t.Run("empty aliases slice", func(t *testing.T) {
		n1 := Network{Name: "bridge", ID: "abc123", Aliases: []string{}}
		n2 := Network{Name: "bridge", ID: "abc123", Aliases: nil}

		// Both should hash successfully (though hashes may differ)
		h1 := n1.hash()
		h2 := n2.hash()

		if h1 == 0 || h2 == 0 {
			t.Error("hash should not be zero for valid networks")
		}
	})

	t.Run("different IP address produces different hash", func(t *testing.T) {
		n1 := Network{Name: "bridge", ID: "abc123", IPAddress: "172.17.0.2"}
		n2 := Network{Name: "bridge", ID: "abc123", IPAddress: "172.17.0.3"}

		if n1.hash() == n2.hash() {
			t.Error("different IP addresses should produce different hashes")
		}
	})
}

func TestContainerHash(t *testing.T) {
	t.Run("identical containers produce same hash", func(t *testing.T) {
		c1 := Container{
			ID:    "container123",
			Name:  "web",
			Image: "nginx:latest",
			State: "running",
			Labels: map[string]string{
				"env":     "prod",
				"version": "1.0",
			},
			Networks: []Network{
				{Name: "bridge", ID: "net1"},
			},
			Ports: []Port{
				{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			},
		}
		c2 := Container{
			ID:    "container123",
			Name:  "web",
			Image: "nginx:latest",
			State: "running",
			Labels: map[string]string{
				"env":     "prod",
				"version": "1.0",
			},
			Networks: []Network{
				{Name: "bridge", ID: "net1"},
			},
			Ports: []Port{
				{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			},
		}

		if c1.hash() != c2.hash() {
			t.Error("identical containers should produce the same hash")
		}
	})

	t.Run("labels in different map order produce same hash", func(t *testing.T) {
		// Maps in Go have random iteration order, but our hash should be consistent
		c1 := Container{
			ID: "container123",
			Labels: map[string]string{
				"a": "1",
				"b": "2",
				"c": "3",
			},
		}
		c2 := Container{
			ID: "container123",
			Labels: map[string]string{
				"c": "3",
				"a": "1",
				"b": "2",
			},
		}

		if c1.hash() != c2.hash() {
			t.Error("labels in different order should produce the same hash")
		}
	})

	t.Run("networks in different order produce same hash", func(t *testing.T) {
		c1 := Container{
			ID: "container123",
			Networks: []Network{
				{Name: "bridge", ID: "net1"},
				{Name: "custom", ID: "net2"},
			},
		}
		c2 := Container{
			ID: "container123",
			Networks: []Network{
				{Name: "custom", ID: "net2"},
				{Name: "bridge", ID: "net1"},
			},
		}

		if c1.hash() != c2.hash() {
			t.Error("networks in different order should produce the same hash (XOR is commutative)")
		}
	})

	t.Run("ports in different order produce same hash", func(t *testing.T) {
		c1 := Container{
			ID: "container123",
			Ports: []Port{
				{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
				{HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
			},
		}
		c2 := Container{
			ID: "container123",
			Ports: []Port{
				{HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
				{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			},
		}

		if c1.hash() != c2.hash() {
			t.Error("ports in different order should produce the same hash (XOR is commutative)")
		}
	})

	t.Run("different state produces different hash", func(t *testing.T) {
		c1 := Container{ID: "container123", State: "running"}
		c2 := Container{ID: "container123", State: "exited"}

		if c1.hash() == c2.hash() {
			t.Error("different states should produce different hashes")
		}
	})

	t.Run("different label value produces different hash", func(t *testing.T) {
		c1 := Container{
			ID:     "container123",
			Labels: map[string]string{"env": "prod"},
		}
		c2 := Container{
			ID:     "container123",
			Labels: map[string]string{"env": "dev"},
		}

		if c1.hash() == c2.hash() {
			t.Error("different label values should produce different hashes")
		}
	})

	t.Run("empty container", func(t *testing.T) {
		c := Container{}
		h := c.hash()

		if h == 0 {
			t.Error("hash should not be zero even for empty container")
		}
	})

	t.Run("nil vs empty collections", func(t *testing.T) {
		c1 := Container{
			ID:       "container123",
			Labels:   nil,
			Networks: nil,
			Ports:    nil,
		}
		c2 := Container{
			ID:       "container123",
			Labels:   map[string]string{},
			Networks: []Network{},
			Ports:    []Port{},
		}

		// Both should produce valid hashes
		h1 := c1.hash()
		h2 := c2.hash()

		if h1 == 0 || h2 == 0 {
			t.Error("hash should not be zero for valid containers")
		}

		// They should produce the same hash since both are logically empty
		if h1 != h2 {
			t.Error("nil and empty collections should produce the same hash")
		}
	})
}

func TestContainerListHash(t *testing.T) {
	t.Run("identical container lists produce same hash", func(t *testing.T) {
		containers1 := []Container{
			{ID: "c1", State: "running"},
			{ID: "c2", State: "running"},
		}
		containers2 := []Container{
			{ID: "c1", State: "running"},
			{ID: "c2", State: "running"},
		}

		h1 := computeHash(containers1)
		h2 := computeHash(containers2)

		if h1 != h2 {
			t.Error("identical container lists should produce the same hash")
		}
	})

	t.Run("different order produces same hash", func(t *testing.T) {
		containers1 := []Container{
			{ID: "c1", State: "running"},
			{ID: "c2", State: "exited"},
		}
		containers2 := []Container{
			{ID: "c2", State: "exited"},
			{ID: "c1", State: "running"},
		}

		h1 := computeHash(containers1)
		h2 := computeHash(containers2)

		if h1 != h2 {
			t.Error("container lists in different order should produce the same hash (XOR is commutative)")
		}
	})

	t.Run("different container state produces different hash", func(t *testing.T) {
		containers1 := []Container{
			{ID: "c1", State: "running"},
		}
		containers2 := []Container{
			{ID: "c1", State: "exited"},
		}

		h1 := computeHash(containers1)
		h2 := computeHash(containers2)

		if h1 == h2 {
			t.Error("different container states should produce different hashes")
		}
	})

	t.Run("empty container list", func(t *testing.T) {
		containers := []Container{}
		h := computeHash(containers)

		if h != 0 {
			t.Error("empty container list should produce zero hash (XOR identity)")
		}
	})
}
