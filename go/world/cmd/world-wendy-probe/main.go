// world-wendy-probe verifies synthetic state through an explicitly selected
// loopback mapping. It neither selects a device nor opens a Wendy connection.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

const diagnosticType = "wendy.diagnostic.transport.v1"
const maxFile = 64 << 10
const maxResponse = 1 << 20
const recordTTL = 20 * time.Minute

type options struct {
	phase, publisher, reader, trust, endpoint, tenant, site, node, runID, checkpoint string
	frame                                                                            world.FrameRef
	deadline                                                                         time.Duration
}
type trustBundle struct {
	RootPublicKey []byte                      `json:"root_public_key"`
	Revocations   authority.SignedRevocations `json:"revocations"`
}
type checkpoint struct {
	Schema       int                    `json:"schema"`
	Tenant       string                 `json:"tenant"`
	Site         string                 `json:"site"`
	Node         string                 `json:"node"`
	RunID        string                 `json:"run_id"`
	Frame        world.FrameRef         `json:"frame"`
	PublishedAt  time.Time              `json:"published_at"`
	Record       authority.SignedRecord `json:"record"`
	DeniedStatus int                    `json:"unauthorized_status"`
}
type report struct {
	Schema                         int            `json:"schema"`
	Phase                          string         `json:"phase"`
	VerifiedAt                     time.Time      `json:"verified_at"`
	Endpoint                       string         `json:"endpoint"`
	Transport                      string         `json:"transport"`
	WendyTransportIdentityVerified bool           `json:"wendy_transport_identity_verified"`
	Domain                         string         `json:"domain"`
	Tenant                         string         `json:"tenant"`
	Site                           string         `json:"site"`
	Node                           string         `json:"node"`
	RunID                          string         `json:"run_id"`
	Entity                         string         `json:"entity"`
	Source                         world.Source   `json:"source"`
	Frame                          world.FrameRef `json:"frame"`
	Version                        world.Version  `json:"version"`
	Origin                         world.Origin   `json:"origin"`
	ComponentSHA256                string         `json:"signed_component_sha256"`
	SignedFieldsExact              bool           `json:"signed_fields_exact"`
	OriginProofVerified            bool           `json:"origin_proof_verified"`
	ReadAgeHintNS                  time.Duration  `json:"read_age_hint_ns"`
	DeniedStatus                   int            `json:"unauthorized_status,omitempty"`
	CheckpointReadback             bool           `json:"checkpoint_readback"`
	RestartObservation             string         `json:"restart_observation"`
}

func parse(args []string) (options, error) {
	var o options
	f := flag.NewFlagSet("world-wendy-probe", flag.ContinueOnError)
	f.SetOutput(io.Discard) // Flags can contain private paths; never echo them on failure.
	f.StringVar(&o.phase, "phase", "", "publish or verify")
	f.StringVar(&o.publisher, "publisher", "", "private publisher configuration; publish phase only")
	f.StringVar(&o.reader, "reader", "", "private read-only configuration")
	f.StringVar(&o.trust, "trust", "", "generated public simulation trust bundle")
	f.StringVar(&o.endpoint, "endpoint", "", "observed HTTP numeric-loopback mapping URL")
	f.StringVar(&o.tenant, "tenant", "", "expected tenant")
	f.StringVar(&o.site, "site", "", "expected site")
	f.StringVar(&o.node, "node", "", "expected node audience")
	f.StringVar(&o.runID, "run-id", "", "expected generated simulation run")
	f.StringVar(&o.frame.ID, "frame-id", "", "expected frame ID")
	f.StringVar(&o.frame.Revision, "frame-revision", "", "expected frame revision")
	f.StringVar(&o.checkpoint, "checkpoint", "", "public checkpoint created by publish, read by verify")
	f.DurationVar(&o.deadline, "deadline", 30*time.Second, "network budget, 1 to 60 seconds")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return o, errors.New("invalid probe arguments")
	}
	if (o.phase != "publish" && o.phase != "verify") || o.reader == "" || o.trust == "" || o.checkpoint == "" || o.tenant == "" || o.site == "" || o.node == "" || o.frame.Validate() != nil || (o.phase == "publish" && o.publisher == "") || (o.phase == "verify" && o.publisher != "") || o.deadline < time.Second || o.deadline > time.Minute || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`).MatchString(o.runID) {
		return o, errors.New("phase, explicit scope, credentials, trust, endpoint and checkpoint are required")
	}
	for _, s := range []string{o.tenant, o.site, o.node, o.frame.ID, o.frame.Revision} {
		if len(s) > 256 || strings.ContainsAny(s, "\r\n\x00") {
			return o, errors.New("invalid expected scope")
		}
	}
	endpoint, err := loopbackEndpoint(o.endpoint)
	if err != nil {
		return o, err
	}
	o.endpoint = endpoint
	return o, nil
}

func loopbackEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("endpoint must be an explicit HTTP loopback origin")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return "", errors.New("endpoint must use a numeric loopback address")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("endpoint requires an explicit valid port")
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func readJSON(path string, out any, private bool) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("probe input must be a regular file, not a symlink or pipe")
	}
	// Avoid a blocking FIFO open or a symlink swap between the metadata check and open.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open required probe file")
	}
	f := os.NewFile(uintptr(fd), "probe-input")
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || (private && info.Mode().Perm()&0077 != 0) {
		return errors.New("credentials must be regular owner-only files")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(b) > maxFile {
		return errors.New("probe file exceeds bound or cannot be read")
	}
	defer clear(b)
	if authority.DecodeStrict(b, out) != nil {
		return errors.New("probe file has invalid or unexpected JSON")
	}
	return nil
}

func makeVerifier(o options, b trustBundle) (*authority.Verifier, error) {
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: b.RootPublicKey, Tenant: o.tenant, Site: o.site, Domain: "simulation", Audience: o.node, Now: time.Now, Replay: authority.NewMemoryReplay(64)})
	if err != nil || v.UpdateRevocations(b.Revocations) != nil {
		return nil, errors.New("simulation trust bundle is invalid, wrong-scope or expired")
	}
	return v, nil
}
func validateCredential(c client.Config, o options, v *authority.Verifier, publisher bool) error {
	g := c.Grant.Grant
	if _, err := loopbackEndpoint(c.URL); err != nil {
		return errors.New("credential origin is not a simulation loopback placeholder")
	}
	if !c.AllowInsecureSimulation || c.Audience != o.node || g.Domain != "simulation" || g.Tenant != o.tenant || g.Site != o.site || !strings.HasPrefix(g.ID, "lab/"+o.runID+"/") {
		return errors.New("credential does not match expected simulation scope")
	}
	if len(c.PrivateKey) != ed25519.PrivateKeySize || len(g.PublicKey) != ed25519.PublicKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(c.PrivateKey[:32]), c.PrivateKey) || !bytes.Equal(c.PrivateKey[32:], g.PublicKey) {
		return errors.New("credential key does not match its grant")
	}
	expected := []string{"read"}
	op := "read"
	if publisher {
		expected = []string{"publish", "read"}
		op = "publish"
		if g.PublishAudience != o.node || g.Kind != "app" {
			return errors.New("publisher is not bound to expected simulation node")
		}
	}
	if len(g.Scopes) != len(expected) {
		return errors.New("probe refuses broader credential authority")
	}
	for _, scope := range expected {
		found := false
		for _, got := range g.Scopes {
			if got == scope {
				found = true
			}
		}
		if !found {
			return errors.New("probe credential lacks its narrow authority")
		}
	}
	if _, err := v.VerifyGrant(c.Grant, op); err != nil {
		return errors.New("simulation grant is invalid, revoked or expired")
	}
	return nil
}

type transport struct {
	endpoint string
	http     *http.Client
}

func newTransport(endpoint string) *transport {
	d := &net.Dialer{Timeout: 2 * time.Second}
	t := &http.Transport{Proxy: nil, DialContext: d.DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	return &transport{endpoint: endpoint, http: &http.Client{Transport: t, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (t *transport) call(ctx context.Context, c client.Config, op, method, path string, body []byte) (int, []byte, error) {
	if len(body) > maxFile || !((path == "/v1/components" && (method == http.MethodGet && op == "read" || method == http.MethodPost && op == "publish")) || (path == "/v1/publishers/epoch" && method == http.MethodPost && op == "publish")) {
		return 0, nil, errors.New("unsupported probe operation")
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return 0, nil, errors.New("cannot generate request nonce")
	}
	now := time.Now()
	lifetime := min(time.Minute, time.Until(time.UnixMilli(c.Grant.Grant.NotAfterUnixMS)))
	proof, err := authority.SignRequest(c.Grant, c.PrivateKey, c.Audience, op, body, hex.EncodeToString(nonce), now, lifetime)
	if err != nil {
		return 0, nil, errors.New("cannot sign probe request")
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return 0, nil, errors.New("cannot encode request proof")
	}
	req, err := http.NewRequestWithContext(ctx, method, t.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("cannot create probe request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-World-Authorization", base64.StdEncoding.EncodeToString(encoded))
	response, err := t.http.Do(req)
	if err != nil {
		return 0, nil, errors.New("probe transport failed or deadline elapsed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return response.StatusCode, nil, errors.New("probe response exceeds bound or is incomplete")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return response.StatusCode, nil, errors.New("probe refuses redirects")
	}
	return response.StatusCode, data, nil
}
func (t *transport) query(ctx context.Context, c client.Config) (client.Snapshot, error) {
	status, b, err := t.call(ctx, c, "read", http.MethodGet, "/v1/components", nil)
	if err != nil {
		return client.Snapshot{}, err
	}
	var s client.Snapshot
	if status != http.StatusOK || authority.DecodeStrictLimit(b, &s, maxResponse) != nil || len(s.Components) > 2000 {
		return s, errors.New("signed read was denied or returned invalid data")
	}
	return s, nil
}
func digest(c world.Component) (string, error) {
	c.Timing.AgeHint = 0
	b, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func exact(s client.Snapshot, record authority.SignedRecord, v *authority.Verifier) (world.Component, error) {
	expected, _ := digest(record.Component)
	found := 0
	var result world.Component
	for _, c := range s.Components {
		if c.Key == record.Component.Key {
			found++
			actual, err := digest(c)
			if err != nil || actual != expected {
				return result, errors.New("readback changed signed component identity, version or content")
			}
			result = c
		}
	}
	if found != 1 {
		return result, errors.New("expected synthetic component is absent or duplicated")
	}
	if result.Timing.AgeHint < 0 || result.Timing.AgeHint >= result.TTL || !time.Now().Add(result.Timing.ErrorBound).Before(result.Timing.SampleTime.Add(result.TTL)) {
		return result, errors.New("probe observation is expired")
	}
	checked := record
	checked.Component = result
	if _, err := v.VerifyRecord(checked); err != nil {
		return result, errors.New("readback origin proof failed")
	}
	return result, nil
}
func component(o options, c client.Config, id string, epoch, sequence uint64) world.Component {
	payload, _ := json.Marshal(struct {
		ProbeID     string `json:"probe_id"`
		Step        uint64 `json:"step"`
		Description string `json:"description"`
	}{id, sequence, "synthetic transport diagnostic; not a sensor measurement"})
	return world.Component{Key: world.Key{Tenant: o.tenant, Site: o.site, Entity: "diagnostic/" + id, Type: diagnosticType, Source: c.Grant.Grant.Source}, Version: world.Version{Epoch: epoch, Sequence: sequence}, Frame: o.frame, Timing: world.Timing{SampleTime: time.Now().UTC(), ClockDomain: "simulation:" + o.runID, ErrorKnown: true, ErrorBound: time.Second}, TTL: recordTTL, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "world-wendy-probe-v1", Simulation: &world.SimulationContext{RunID: o.runID, ScenarioVersion: o.frame.Revision, ClockDomain: "simulation:" + o.runID}}, Payload: payload}
}
func sign(c client.Config, component world.Component) (authority.SignedRecord, []byte, error) {
	r, err := authority.SignRecord(c.Grant, c.PrivateKey, component, time.Now())
	if err != nil {
		return r, nil, errors.New("cannot sign synthetic diagnostic")
	}
	b, err := json.Marshal(r)
	return r, b, err
}
func (t *transport) publish(ctx context.Context, c client.Config, body []byte, expected world.Component) error {
	status, b, err := t.call(ctx, c, "publish", http.MethodPost, "/v1/components", body)
	if err != nil {
		return err
	}
	var ack struct {
		Result    world.Result    `json:"result"`
		Component world.Component `json:"component"`
	}
	if status != 200 || authority.DecodeStrictLimit(b, &ack, maxResponse) != nil || ack.Result != world.Applied {
		return errors.New("publication was not acknowledged as applied")
	}
	a, _ := json.Marshal(ack.Component)
	e, _ := json.Marshal(expected)
	if !bytes.Equal(a, e) {
		return errors.New("publication acknowledgement changed the submitted component")
	}
	return nil
}
func writeCheckpoint(path string, cp checkpoint) error {
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil || len(b) > maxFile {
		return errors.New("checkpoint exceeds bound")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("checkpoint must be a new writable path")
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return errors.New("cannot write checkpoint")
	}
	if f.Sync() != nil {
		return errors.New("cannot sync checkpoint")
	}
	if f.Close() != nil {
		return errors.New("cannot close checkpoint")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("cannot open checkpoint directory")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errors.New("cannot sync checkpoint directory")
	}
	success = true
	return nil
}

func run(ctx context.Context, args []string, out io.Writer) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.deadline)
	defer cancel()
	var trust trustBundle
	var reader client.Config
	if err = readJSON(o.trust, &trust, false); err != nil {
		return err
	}
	v, err := makeVerifier(o, trust)
	if err != nil {
		return err
	}
	if err = readJSON(o.reader, &reader, true); err != nil {
		return err
	}
	defer clear(reader.PrivateKey)
	if err = validateCredential(reader, o, v, false); err != nil {
		return err
	}
	t := newTransport(o.endpoint)
	defer t.http.CloseIdleConnections()
	var cp checkpoint
	var observed world.Component
	if o.phase == "publish" {
		if _, err = os.Lstat(o.checkpoint); !errors.Is(err, os.ErrNotExist) {
			return errors.New("publish requires a new checkpoint path")
		}
		var publisher client.Config
		if err = readJSON(o.publisher, &publisher, true); err != nil {
			return err
		}
		defer clear(publisher.PrivateKey)
		if err = validateCredential(publisher, o, v, true); err != nil {
			return err
		}
		if publisher.Grant.Grant.Source == reader.Grant.Grant.Source || bytes.Equal(publisher.Grant.Grant.PublicKey, reader.Grant.Grant.PublicKey) {
			return errors.New("reader and publisher must have separate identities")
		}
		if _, err = t.query(ctx, reader); err != nil {
			return err
		}
		status, b, err := t.call(ctx, publisher, "publish", http.MethodPost, "/v1/publishers/epoch", []byte("{}"))
		if err != nil {
			return err
		}
		var allocation struct {
			Epoch uint64 `json:"epoch"`
		}
		if status != 200 || authority.DecodeStrict(b, &allocation) != nil || allocation.Epoch == 0 {
			return errors.New("publisher epoch was not allocated")
		}
		random := make([]byte, 16)
		if _, err = rand.Read(random); err != nil {
			return errors.New("cannot generate diagnostic identity")
		}
		id := hex.EncodeToString(random)
		var final authority.SignedRecord
		for sequence := uint64(1); sequence <= 2; sequence++ {
			c := component(o, publisher, id, allocation.Epoch, sequence)
			record, body, err := sign(publisher, c)
			if err != nil {
				return err
			}
			if _, err = v.VerifyRecord(record); err != nil {
				return errors.New("generated origin proof failed local validation")
			}
			if err = t.publish(ctx, publisher, body, c); err != nil {
				return err
			}
			snapshot, err := t.query(ctx, reader)
			if err != nil {
				return err
			}
			observed, err = exact(snapshot, record, v)
			if err != nil {
				return err
			}
			final = record
		}
		_, deniedBody, err := sign(publisher, component(o, publisher, id, allocation.Epoch, 3))
		if err != nil {
			return err
		}
		denied, _, err := t.call(ctx, reader, "publish", http.MethodPost, "/v1/components", deniedBody)
		if err != nil {
			return err
		}
		if denied != 401 && denied != 403 {
			return errors.New("read-only publication was not explicitly denied")
		}
		snapshot, err := t.query(ctx, reader)
		if err != nil {
			return err
		}
		observed, err = exact(snapshot, final, v)
		if err != nil {
			return err
		}
		cp = checkpoint{Schema: 1, Tenant: o.tenant, Site: o.site, Node: o.node, RunID: o.runID, Frame: o.frame, PublishedAt: time.Now().UTC(), Record: final, DeniedStatus: denied}
		if err = writeCheckpoint(o.checkpoint, cp); err != nil {
			return err
		}
	} else {
		if err = readJSON(o.checkpoint, &cp, false); err != nil {
			return err
		}
		c := cp.Record.Component
		if cp.Schema != 1 || cp.Tenant != o.tenant || cp.Site != o.site || cp.Node != o.node || cp.RunID != o.runID || cp.Frame != o.frame || c.Frame != o.frame || c.Key.Tenant != o.tenant || c.Key.Site != o.site || c.Key.Type != diagnosticType || c.Version.Sequence != 2 || c.Provenance.Origin != world.Simulated || c.Provenance.Simulation == nil || c.Provenance.Simulation.RunID != o.runID || c.Provenance.Simulation.ClockDomain != "simulation:"+o.runID || c.Timing.ClockDomain != "simulation:"+o.runID || cp.Record.Grant.Grant.PublishAudience != o.node || !strings.HasPrefix(cp.Record.Grant.Grant.ID, "lab/"+o.runID+"/") || (cp.DeniedStatus != 401 && cp.DeniedStatus != 403) {
			return errors.New("checkpoint does not match expected synthetic scope")
		}
		if _, err = v.VerifyRecord(cp.Record); err != nil {
			return errors.New("checkpoint origin proof is invalid, revoked or expired")
		}
		snapshot, err := t.query(ctx, reader)
		if err != nil {
			return err
		}
		observed, err = exact(snapshot, cp.Record, v)
		if err != nil {
			return err
		}
	}
	hash, _ := digest(observed)
	deniedStatus := 0
	if o.phase == "publish" {
		deniedStatus = cp.DeniedStatus
	}
	result := report{Schema: 1, Phase: o.phase, VerifiedAt: time.Now().UTC(), Endpoint: o.endpoint, Transport: "raw-simulation-http-via-explicit-loopback-mapping", Domain: "simulation", Tenant: o.tenant, Site: o.site, Node: o.node, RunID: o.runID, Entity: observed.Key.Entity, Source: observed.Key.Source, Frame: observed.Frame, Version: observed.Version, Origin: observed.Provenance.Origin, ComponentSHA256: hash, SignedFieldsExact: true, OriginProofVerified: true, ReadAgeHintNS: observed.Timing.AgeHint, DeniedStatus: deniedStatus, CheckpointReadback: o.phase == "verify", RestartObservation: "App restart must be established by separate Wendy lifecycle evidence"}
	if json.NewEncoder(out).Encode(result) != nil {
		return errors.New("cannot write probe report")
	}
	return nil
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
