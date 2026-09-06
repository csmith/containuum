package containuum

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

func TestConvertContainerPorts(t *testing.T) {
	inspect := container.InspectResponse{
		Config: &container.Config{
			Image: "nginx",
		},
		NetworkSettings: &container.NetworkSettings{
			NetworkSettingsBase: container.NetworkSettingsBase{
				Ports: nat.PortMap{
					"80/tcp": []nat.PortBinding{
						{HostIP: "0.0.0.0", HostPort: "8080"},
					},
					"443/tcp": nil, // exposed but not published
				},
			},
		},
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:    "abc123",
			Name:  "/web",
			State: &container.State{Status: "running"},
		},
	}

	c := convertContainer(inspect)

	if len(c.Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d: %+v", len(c.Ports), c.Ports)
	}

	byContainerPort := map[uint16]Port{}
	for _, p := range c.Ports {
		byContainerPort[p.ContainerPort] = p
	}

	if bound := byContainerPort[80]; bound.HostIP != "0.0.0.0" || bound.HostPort != 8080 || bound.Protocol != "tcp" {
		t.Errorf("unexpected bound port: %+v", bound)
	}

	if unbound := byContainerPort[443]; unbound.HostPort != 0 || unbound.HostIP != "" || unbound.Protocol != "tcp" {
		t.Errorf("unexpected unbound port: %+v", unbound)
	}
}
