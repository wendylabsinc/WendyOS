// Package go2project validates private World projections and observes an
// explicitly pinned local Go2 simulator. Observations never authorize motion.
package go2project

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

const PolicyBundle = "go2-moe-cts-164k-0.6715-v1"
const MaxStatusBytes = 1 << 20

type Pins struct {
	Endpoint        string `json:"endpoint"`
	VMName          string `json:"vm_name"`
	SourceDigest    string `json:"source_digest"`
	ProjectID       string `json:"project_id"`
	ProjectRevision string `json:"project_revision"`
	WorldEpoch      uint64 `json:"world_epoch"`
}
type Observation struct {
	ObservedUnixMS int64   `json:"observed_unix_ms"`
	StatusSHA256   string  `json:"status_sha256"`
	SimulationTime float64 `json:"simulation_time"`
	WorldEpoch     uint64  `json:"world_epoch"`
	ProjectEpoch   uint64  `json:"project_epoch"`
	Mode           string  `json:"mode"`
	RuntimeReady   bool    `json:"runtime_ready"`
	RuntimeHealthy bool    `json:"runtime_healthy"`
}

func Endpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("explicit numeric loopback HTTP origin required")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || (ip.String() != "127.0.0.1" && ip.String() != "::1") || ip.Zone() != "" {
		return "", errors.New("canonical loopback address required")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return "", errors.New("explicit canonical port required")
	}
	result := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if raw != result && raw != result+"/" {
		return "", errors.New("noncanonical loopback origin")
	}
	return result, nil
}
func (p Pins) Validate() error {
	endpoint, err := Endpoint(p.Endpoint)
	if err != nil || endpoint != p.Endpoint || !token(p.VMName) || len(p.SourceDigest) != 71 || p.SourceDigest[:7] != "sha256:" || !hexDigest(p.SourceDigest[7:]) || !lowerHex(p.ProjectID, 32) || !hexDigest(p.ProjectRevision) || p.WorldEpoch == 0 {
		return errors.New("invalid explicit simulation pins")
	}
	return nil
}

// Inspect performs exactly one GET. It disables proxies, redirects and content
// decompression, and accepts no caller-provided transport that could mutate state.
func Inspect(ctx context.Context, p Pins) (Observation, error) {
	if err := p.Validate(); err != nil {
		return Observation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableCompression: true, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: 2 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("simulator redirect refused") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Endpoint+"/api/status", nil)
	if err != nil {
		return Observation{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Observation{}, errors.New("simulator status unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxStatusBytes || response.Header.Get("Content-Encoding") != "" {
		return Observation{}, errors.New("simulator status response refused")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxStatusBytes+1))
	if err != nil || len(body) > MaxStatusBytes {
		return Observation{}, errors.New("simulator status exceeds bound or is incomplete")
	}
	observed, err := ValidateStatus(body, p)
	if err != nil {
		return Observation{}, err
	}
	observed.ObservedUnixMS = time.Now().UnixMilli()
	return observed, nil
}

func ValidateStatus(body []byte, p Pins) (Observation, error) {
	if err := p.Validate(); err != nil {
		return Observation{}, err
	}
	var fields map[string]json.RawMessage
	if err := strict(body, &fields, MaxStatusBytes); err != nil {
		return Observation{}, errors.New("malformed simulator status")
	}
	read := func(name string, out any) bool {
		raw, ok := fields[name]
		return ok && string(raw) != "null" && strict(raw, out, MaxStatusBytes) == nil
	}
	var sim, healthy, ready bool
	var robot, kind, vm, source, isolation, policy, clock, mode string
	var version int
	var epoch uint64
	var simtime float64
	if !read("simulation", &sim) || !sim || !read("robot", &robot) || robot != "go2" || !read("robot_kind", &kind) || kind != "go2" || !read("profile_version", &version) || version != 1 || !read("vm_name", &vm) || vm != p.VMName || !read("source_digest", &source) || source != p.SourceDigest || !read("dds_isolation", &isolation) || isolation != "udp-rtps-loopback" || !read("policy_bundle", &policy) || policy != PolicyBundle || !read("clock_mode", &clock) || clock != "device" {
		return Observation{}, errors.New("simulator identity mismatch")
	}
	if !read("healthy", &healthy) || !healthy || !read("ready", &ready) || !read("mode", &mode) || (mode != "paused" && mode != "standing" && mode != "moving") || !read("epoch", &epoch) || epoch != p.WorldEpoch || !read("time", &simtime) || math.IsNaN(simtime) || math.IsInf(simtime, 0) || simtime < 0 || string(fields["error"]) != "null" {
		return Observation{}, errors.New("simulator epoch, health or mode invalid")
	}
	var project struct {
		ProjectID   string          `json:"project_id"`
		Revision    string          `json:"revision"`
		Error       json.RawMessage `json:"error"`
		WatcherSeen *float64        `json:"watcher_seen"`
		Epoch       uint64          `json:"epoch"`
	}
	if !read("project", &project) || project.ProjectID != p.ProjectID || project.Revision != p.ProjectRevision || project.Epoch == 0 || project.Epoch > epoch || string(project.Error) != "null" || project.WatcherSeen == nil || *project.WatcherSeen <= 0 {
		return Observation{}, errors.New("simulator project mismatch or error")
	}
	return Observation{StatusSHA256: hash(body), SimulationTime: simtime, WorldEpoch: epoch, ProjectEpoch: project.Epoch, Mode: mode, RuntimeReady: ready, RuntimeHealthy: healthy}, nil
}
