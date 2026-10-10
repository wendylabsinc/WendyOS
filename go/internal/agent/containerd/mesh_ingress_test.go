package containerd

import (
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

type recordingMDNS struct {
	starts []string
	stops  []string
	ports  [][]appconfig.PortMapping
	onStop func()
}

func (m *recordingMDNS) StartMeshApp(containerName, appID, ip, bridge string, ports []appconfig.PortMapping) error {
	m.starts = append(m.starts, containerName+":"+appID+":"+ip+":"+bridge)
	m.ports = append(m.ports, append([]appconfig.PortMapping(nil), ports...))
	return nil
}
func (m *recordingMDNS) StopMeshApp(containerName string) {
	m.stops = append(m.stops, containerName)
	if m.onStop != nil {
		m.onStop()
	}
}

func TestMeshIngressPublishesOnlySuccessfullyForwardedPorts(t *testing.T) {
	original := addMeshIngressPortForward
	t.Cleanup(func() { addMeshIngressPortForward = original })
	addMeshIngressPortForward = func(host uint16, _ string, _ uint16) error {
		if host == 8081 {
			return errors.New("iptables unavailable")
		}
		return nil
	}
	r := meshingress.NewRegistry()
	mdns := &recordingMDNS{}
	c := &Client{logger: zap.NewNop(), meshIngress: r, meshMDNS: mdns}
	ports := []appconfig.PortMapping{{Host: 8080, Container: 80}, {Host: 8081, Container: 81}}
	if err := c.applyMeshIngressPorts("app_a", "app", "10.3.0.2", ports); err != nil {
		t.Fatal(err)
	}
	if !r.Allowed(8080) || r.Allowed(8081) {
		t.Fatal("a port without a successful forward was authorized")
	}
	if len(mdns.ports) != 1 || len(mdns.ports[0]) != 1 || mdns.ports[0][0] != ports[0] {
		t.Fatalf("collector received unforwarded ports: %+v", mdns.ports)
	}
	c.teardownMeshEgress(nil, "app_a", "app", "")
	if r.Allowed(8080) {
		t.Fatal("teardown left ingress authorized")
	}
}

func TestMeshIngressConflictFailsBeforeChangingForward(t *testing.T) {
	r := meshingress.NewRegistry()
	if err := r.Claim("first", 8080); err != nil {
		t.Fatal(err)
	}
	c := &Client{logger: zap.NewNop(), meshIngress: r}
	if err := c.applyMeshIngressPorts("second", "app", "10.3.0.3", []appconfig.PortMapping{{Host: 8080, Container: 80}}); err == nil {
		t.Fatal("conflicting container was allowed to start ingress")
	}
	if !r.Allowed(8080) {
		t.Fatal("first container lost its ingress")
	}
}

func TestMeshIngressRequiresRegistryAndValidPorts(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	if err := c.applyMeshIngressPorts("app", "app", "10.3.0.2", []appconfig.PortMapping{{Host: 8080, Container: 80}}); err == nil {
		t.Fatal("missing registry allowed ingress")
	}
	c.meshIngress = meshingress.NewRegistry()
	if err := c.applyMeshIngressPorts("app", "app", "10.3.0.2", []appconfig.PortMapping{{Host: 0, Container: 80}}); err == nil {
		t.Fatal("port zero allowed ingress")
	}
	if err := c.applyMeshIngressPorts("app", "app", "10.3.0.2", []appconfig.PortMapping{{Host: 8080, Container: 80}, {Host: 8080, Container: 81}}); err == nil {
		t.Fatal("duplicate host port allowed ingress")
	}
}

func TestVerifiedMeshResultIP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
		wantIP string
	}{
		{"valid", `{"ips":[{"address":"10.79.99.165/28"}]}`, "10.79.99.165"},
		{"foreign subnet", `{"ips":[{"address":"10.79.100.165/28"}]}`, ""},
		{"wrong mask", `{"ips":[{"address":"10.79.99.165/24"}]}`, ""},
		{"multiple addresses", `{"ips":[{"address":"10.79.99.165/28"},{"address":"10.79.99.166/28"}]}`, ""},
		{"ipv6", `{"ips":[{"address":"fd00::1/64"}]}`, ""},
		{"malformed", `{`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verifiedMeshResultIP(tc.result, "10.79.99.160/28")
			if tc.wantIP == "" && err == nil || tc.wantIP != "" && (err != nil || got != tc.wantIP) {
				t.Fatalf("got %q, %v; want %q", got, err, tc.wantIP)
			}
		})
	}
}

func TestMeshCheckResultForTask(t *testing.T) {
	const original = "/run/wendy/netns/app"
	const task = "/proc/123/ns/net"
	const result = `{"interfaces":[{"name":"bridge","mac":"aa"},{"name":"eth0","sandbox":"/run/wendy/netns/app","mac":"bb"}],"ips":[{"address":"10.79.99.165/28"}]}`
	got, err := meshCheckResultForTask(result, original, task)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"interfaces":[{"mac":"aa","name":"bridge"},{"mac":"bb","name":"eth0","sandbox":"/proc/123/ns/net"}],"ips":[{"address":"10.79.99.165/28"}]}` {
		t.Fatalf("unexpected CNI CHECK result: %s", got)
	}
	for _, bad := range []string{
		`{"interfaces":[{"name":"eth0","sandbox":"/tmp/other"}]}`,
		`{"interfaces":[{"name":"eth0","sandbox":"/run/wendy/netns/app"},{"name":"eth0","sandbox":"/run/wendy/netns/app"}]}`,
		`{"interfaces":[{"name":"other"}]}`,
		`{"interfaces":{}}`,
	} {
		if _, err := meshCheckResultForTask(bad, original, task); err == nil {
			t.Fatalf("accepted invalid CNI result: %s", bad)
		}
	}
}

func TestMeshIngressOldTaskExitCannotRevokeRestart(t *testing.T) {
	r := meshingress.NewRegistry()
	c := &Client{meshIngress: r}
	first := c.beginMeshIngressRun("app")
	if err := r.Claim("app", 8080); err != nil {
		t.Fatal(err)
	}
	second := c.beginMeshIngressRun("app")
	if r.Allowed(8080) {
		t.Fatal("restart left the old task authorized")
	}
	if err := r.Claim("app", 8080); err != nil {
		t.Fatal(err)
	}
	c.releaseMeshIngressRun("app", first)
	if !r.Allowed(8080) {
		t.Fatal("late exit from old task revoked replacement")
	}
	c.releaseMeshIngressRun("app", second)
	if r.Allowed(8080) {
		t.Fatal("replacement task exit left ingress authorized")
	}
}

func TestMeshMDNSLifecycleUsesIsolatedAppAndWithdrawsBeforePortRelease(t *testing.T) {
	r := meshingress.NewRegistry()
	mdns := &recordingMDNS{}
	c := &Client{logger: zap.NewNop(), meshIngress: r, meshMDNS: mdns}
	if err := c.applyMeshIngressPorts("browse", "app", "10.3.0.2", nil); err != nil {
		t.Fatal(err)
	}
	if len(mdns.starts) != 1 || mdns.starts[0] != "browse:app:10.3.0.2:"+bridgeName("app") {
		t.Fatalf("browse-only app had no scoped mDNS bridge: %+v", mdns.starts)
	}
	if err := r.ClaimForApp("browse", "app", 18080); err != nil {
		t.Fatal(err)
	}
	mdns.onStop = func() {
		if !r.AllowedApp("app", 18080) {
			t.Fatal("mDNS bridge stopped after ingress was revoked")
		}
	}
	c.releaseMeshIngress("browse")
	if len(mdns.stops) != 1 || r.AllowedApp("app", 18080) {
		t.Fatalf("stop did not withdraw bridge and port: stops=%v", mdns.stops)
	}
}
