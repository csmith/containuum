package containuum

import (
	"encoding/json"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func inspectFromJSON(t *testing.T, data string) container.InspectResponse {
	t.Helper()
	var inspect container.InspectResponse
	require.NoError(t, json.Unmarshal([]byte(data), &inspect))
	return inspect
}

func TestConvertContainer(t *testing.T) {
	c := convertContainer(inspectFromJSON(t, `{
		"Id": "abc123",
		"Name": "/web",
		"State": {"Status": "running"},
		"Config": {"Image": "nginx", "Labels": {"web": "true"}}
	}`))

	assert.Equal(t, "abc123", c.ID)
	assert.Equal(t, "web", c.Name)
	assert.Equal(t, "nginx", c.Image)
	assert.Equal(t, "running", c.State)
	assert.Equal(t, map[string]string{"web": "true"}, c.Labels)
}

func TestConvertContainerNetworks(t *testing.T) {
	c := convertContainer(inspectFromJSON(t, `{
		"Id": "abc123",
		"Name": "/web",
		"State": {"Status": "running"},
		"Config": {"Image": "nginx"},
		"NetworkSettings": {
			"Networks": {
				"dual": {
					"NetworkID": "net1",
					"IPAddress": "172.18.0.2",
					"GlobalIPv6Address": "2001:db8::2",
					"Gateway": "172.18.0.1",
					"Aliases": ["web"]
				},
				"unset": {
					"NetworkID": "net2",
					"IPAddress": "",
					"GlobalIPv6Address": "",
					"Gateway": ""
				}
			}
		}
	}`))

	assert.ElementsMatch(t, []Network{
		{
			Name:       "dual",
			ID:         "net1",
			IPAddress:  "172.18.0.2",
			IP6Address: "2001:db8::2",
			Gateway:    "172.18.0.1",
			Aliases:    []string{"web"},
		},
		{
			Name: "unset",
			ID:   "net2",
		},
	}, c.Networks)
}

func TestConvertContainerPorts(t *testing.T) {
	c := convertContainer(inspectFromJSON(t, `{
		"Id": "abc123",
		"Name": "/web",
		"State": {"Status": "running"},
		"Config": {"Image": "nginx"},
		"NetworkSettings": {
			"Ports": {
				"80/tcp": [
					{"HostIp": "0.0.0.0", "HostPort": "8080"},
					{"HostIp": "::", "HostPort": "8080"}
				],
				"53/udp": [{"HostIp": "", "HostPort": "5353"}],
				"443/tcp": null,
				"9000/tcp": [{"HostIp": "127.0.0.1", "HostPort": ""}]
			}
		}
	}`))

	assert.ElementsMatch(t, []Port{
		{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
		{ContainerPort: 443, Protocol: "tcp"},
	}, c.Ports)
}
