package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

type detachedJSONRunKey struct{}

func detachedJSONRun(ctx context.Context) bool {
	value, _ := ctx.Value(detachedJSONRunKey{}).(bool)
	return value
}

func runProgressWriter(ctx context.Context) io.Writer {
	if detachedJSONRun(ctx) {
		return os.Stderr
	}
	return os.Stdout
}

type detachedRunEndpoint struct {
	App string `json:"app"`
	URL string `json:"url"`
}

type detachedRunResult struct {
	Status    string                `json:"status"`
	App       string                `json:"app"`
	Device    string                `json:"device"`
	Readiness string                `json:"readiness"`
	URL       string                `json:"url,omitempty"`
	Endpoints []detachedRunEndpoint `json:"endpoints"`
}

var vmTCPPortMapping = func(ctx context.Context, name string, port int) (int, error) {
	s, err := vm.NewStore()
	if err != nil {
		return 0, err
	}
	return s.TCPPortMapping(ctx, name, port)
}

// Only the ordinary CLI run command emits this result. Internal provisioning
// and fleet deploys retain their own output contracts. Resolving an endpoint
// does not probe readiness or execute any host-side postStart action.
func (o runOptions) reportDetachedRun(ctx context.Context, conn *grpcclient.AgentConnection, appID string, configs ...*appconfig.AppConfig) error {
	if !o.detachedOutput || o.deploy {
		return nil
	}
	result := detachedRunResult{
		Status: "started", App: appID, Device: conn.Addr,
		Readiness: "not_checked", Endpoints: []detachedRunEndpoint{},
	}
	if result.Device == "" {
		result.Device = conn.Host
	}
	// Bound best-effort routing lookups independently of app boot time.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	name, err := userVMForConnection(conn)
	if conn.SimulatorName != "" {
		result.Device = "vm:" + conn.SimulatorName
	}
	if name != "" {
		result.Device = "vm:" + name
	}
	if err != nil {
		cliNotice("App started, but its URL could not be determined: %v", err)
	} else {
		host := conn.Host
		if name != "" {
			host = "127.0.0.1"
		} else if conn.Reconnect != nil {
			// A cloud tunnel's name or private interface IP is not a host route.
			host = activeMeshHost(ctx, conn)
		}
		seen := map[string]bool{}
		for _, cfg := range configs {
			for _, candidate := range detachedAppURLs(cfg, host) {
				if name != "" {
					candidate, err = mappedVMAppURL(ctx, name, candidate)
					if err != nil {
						cliNotice("App started, but its URL could not be determined: %v", err)
						continue
					}
				}
				if seen[candidate] {
					continue
				}
				seen[candidate] = true
				result.Endpoints = append(result.Endpoints, detachedRunEndpoint{App: cfg.ContainerName(), URL: candidate})
			}
		}
	}
	if len(result.Endpoints) != 0 {
		result.URL = result.Endpoints[0].URL
	}
	if jsonOutput {
		return printJSON(result)
	}
	cliLogln("Readiness not checked (detached run).")
	for _, endpoint := range result.Endpoints {
		cliLogln("App URL (%s): %s", endpoint.App, endpoint.URL)
	}
	return nil
}

// HTTP entitlements and an explicit hostname-based openURL describe web
// endpoints. A bare TCP readiness port alone need not speak HTTP.
func detachedAppURLs(cfg *appconfig.AppConfig, host string) []string {
	if cfg == nil || host == "" || strings.HasPrefix(host, "unix:") {
		return nil
	}
	// urlSafeHost also unmaps IPv4-mapped addresses for hook expansion.
	// Normalize the comparison and entitlement URLs to the same host.
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		host = ip.To4().String()
	}
	var urls []string
	if cfg.Hooks != nil && cfg.Hooks.PostStart != nil {
		hookURL := cfg.Hooks.PostStart.OpenURL
		if strings.Contains(hookURL, "WENDY_HOSTNAME") {
			candidate := expandHookEnv(hookURL, urlSafeHost(host), cfg.AppID, cfg.ServiceName)
			if u, err := url.Parse(candidate); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() == host && u.User == nil {
				urls = append(urls, candidate)
			}
		}
	}
	for _, e := range cfg.Entitlements {
		if e.Type == appconfig.EntitlementHTTP && e.Port > 0 && e.Port <= 65535 {
			u := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(e.Port))}
			urls = append(urls, u.String())
		}
	}
	return urls
}

// Read the live QEMU listener; never invent a same-port URL from guest
// configuration, or reuse another VM's listener on the developer's machine.
func mappedVMAppURL(ctx context.Context, name, candidate string) (string, error) {
	u, err := url.Parse(candidate)
	if err != nil {
		return "", err
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return "", err
		}
	}
	hostPort, err := vmTCPPortMapping(ctx, name, port)
	if err != nil {
		return "", err
	}
	if hostPort == 0 {
		return "", fmt.Errorf("VM %q has no loopback forward for guest TCP port %d", name, port)
	}
	u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))
	return u.String(), nil
}
