package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/operator"
	"github.com/wendylabsinc/wendy/go/world/production/readservice"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const liveReadEnvironment = "WENDY_WORLD_LIVE_READ_CONFIG"

type liveReadInput struct {
	Version          int                 `json:"version"`
	SimulationReader string              `json:"simulation_reader"`
	Dataset          readservice.Dataset `json:"dataset"`
	Report           string              `json:"report"`
}
type liveReadSummary struct {
	Sensors       []string `json:"sensors"`
	HazardEntity  string   `json:"hazard_entity"`
	HazardInputs  int      `json:"hazard_inputs"`
	PackageSHA256 string   `json:"package_sha256"`
	Components    int      `json:"components"`
}

// TestLiveReadService is intentionally absent from default unit-test networking.
// It needs a separately started, explicitly configured NativeSite and real Wendy
// time-server reachability. Its operator/root are generated UNTRUSTED fixtures;
// no user credential config, keychain, device, or enrollment is accessed.
func TestLiveReadService(t *testing.T) {
	inputPath := os.Getenv(liveReadEnvironment)
	if inputPath == "" {
		t.Skip("opt-in live simulation + authenticated-time integration")
	}
	raw, err := readFile(inputPath, 64<<10, true)
	if err != nil {
		t.Fatal(err)
	}
	var input liveReadInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Version != 1 || !filepath.IsAbs(input.SimulationReader) || !filepath.IsAbs(input.Report) || input.Dataset.Domain != "simulation" {
		t.Fatal("explicit live-test input required")
	}
	if _, err := os.Lstat(input.Report); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("report path must be new")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	clock, err := authenticatedtime.Acquire(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 3 * time.Second, MaxHoldover: 10 * time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	if err != nil {
		t.Fatal("real authenticated time acquisition failed", err)
	}
	fixture := testidentity.New(t, time.Now()) // certificate validity only; not authority time
	certificates, err := wendycert.New(wendycert.Config{Tenant: fixture.Tenant, Roots: [][]byte{fixture.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := clock.Now()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := certificates.Verify(fixture.ChainDER, w.Earliest, w.Latest)
	if err != nil {
		t.Fatal("test certificate is not valid at authenticated time", err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	rootPath := write("untrusted-test-root.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.RootDER}))
	privateDER, err := x509.MarshalPKCS8PrivateKey(fixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(privateDER)
	credentials := map[string]any{"auth": []any{map[string]any{"name": "untrusted-live-test", "certificates": []any{map[string]any{"pemCertificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Leaf.Raw})), "pemCertificateChain": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Issuer.Raw})), "pemPrivateKey": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})), "principalUri": fixture.Principal}}}}}
	credentialJSON, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := write("untrusted-operator.json", credentialJSON)
	clear(credentialJSON)
	selection := operator.Selection{ConfigPath: credentialPath, Context: "untrusted-live-test", Tenant: fixture.Tenant, Principal: fixture.Principal}
	signer := operator.Signer{Certificates: certificates, Clock: clock}
	site, audience := "untrusted-live-read-test", "loopback-world-read-test"
	policy, err := signer.SignReadPolicy(ctx, selection, operator.PolicyTarget{Site: site, Audience: audience, PolicyID: "untrusted-live-policy", Revision: 1, Lifetime: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	revocations, err := signer.SignRevocations(ctx, selection, operator.RevocationTarget{Site: site, Audience: audience, Revision: 1, Lifetime: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "untrusted-loopback-test"}, NotBefore: w.Earliest.Add(-time.Hour), NotAfter: w.Latest.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: false}
	tlsDER, err := x509.CreateCertificate(rand.Reader, template, template, &tlsKey.PublicKey, tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsKeyDER, err := x509.MarshalPKCS8PrivateKey(tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(tlsKeyDER)
	tlsPin := sha256.Sum256(tlsDER)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config{Version: 1, Tenant: fixture.Tenant, Site: site, Audience: audience, Listen: address, TLSCertificate: write("test-end-entity.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsDER})), TLSKey: write("test-tls-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: tlsKeyDER})), TrustRoots: rootPath, Ledger: filepath.Join(dir, "durable-admission.json"), PolicyIssuer: admission.Pin{Principal: fixture.Principal, SigningKeySHA256: identity.SigningKeySHA256}, RevocationIssuer: admission.Pin{Principal: fixture.Principal, SigningKeySHA256: identity.SigningKeySHA256}, Policy: write("policy.jws", []byte(policy)), Revocations: write("revocations.jws", []byte(revocations)), TimeSource: "wendy", Dataset: input.Dataset, SimulationReader: input.SimulationReader}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := write("service.json", cfgJSON)
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, TLSHandshakeTimeout: 2 * time.Second, DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, // exact end-entity verification below replaces PKI trust; never a CA or system-root install
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.Version < tls.VersionTLS13 || len(state.PeerCertificates) != 1 || sha256.Sum256(state.PeerCertificates[0].Raw) != tlsPin {
				return errors.New("test endpoint certificate pin mismatch")
			}
			cert := state.PeerCertificates[0]
			if cert.IsCA {
				return errors.New("test transport must be an end entity")
			}
			if err := cert.VerifyHostname("127.0.0.1"); err != nil {
				return err
			}
			current, err := clock.Now()
			if err != nil {
				return err
			}
			if current.Earliest.Before(cert.NotBefore) || !current.Latest.Before(cert.NotAfter) {
				return errors.New("test TLS certificate invalid at authenticated time")
			}
			return cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature)
		}}}
	httpClient := &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	defer transport.CloseIdleConnections()
	endpoint := "https://" + address
	send := func(method, path string, wire []byte) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(wire))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return 0, nil, errors.New("bounded response unavailable")
		}
		return response.StatusCode, data, nil
	}
	start := func() func() {
		t.Helper()
		serverCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- run(serverCtx, cfgPath) }()
		stopped := false
		cleanup := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("service stop: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("service did not stop")
			}
		}
		t.Cleanup(cleanup)
		until := time.Now().Add(12 * time.Second)
		for time.Now().Before(until) {
			select {
			case err := <-done:
				stopped = true
				stop()
				t.Fatalf("actual service startup failed: %v", err)
			default:
			}
			if status, _, err := send("GET", request.Path, nil); err == nil && status == 405 {
				return cleanup
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatal("actual service did not become ready through pinned TLS")
		return cleanup
	}
	stop := start()
	body, err := json.Marshal(readservice.Query{Version: 1, DatasetID: input.Dataset.ID, Limit: readservice.MaxComponents})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(proof string, payload []byte) []byte {
		t.Helper()
		b, e := json.Marshal(readservice.Envelope{Proof: proof, BodyBase64: base64.StdEncoding.EncodeToString(payload)})
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	sign := func() string {
		t.Helper()
		p, e := signer.SignRead(ctx, selection, operator.ReadTarget{Site: site, Audience: audience, PolicyRevision: 1}, body)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	// Independently sampled bounded intervals can differ slightly. Retry an
	// unconsumed proof briefly; success still requires the service's native clock.
	positive := func(proof string) (readservice.Response, []byte) {
		t.Helper()
		wire := envelope(proof, body)
		until := time.Now().Add(12 * time.Second)
		for {
			status, data, e := send("POST", request.Path, wire)
			if e != nil {
				t.Fatal(e)
			}
			if status == 200 {
				var result readservice.Response
				if json.Unmarshal(data, &result) != nil {
					t.Fatal("invalid service response")
				}
				return result, wire
			}
			if status != 403 || time.Now().After(until) {
				t.Fatalf("expected authorized read, got %d: %s", status, data)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	var result readservice.Response
	var wire []byte
	var acceptedProof string
	var summary liveReadSummary
	until := time.Now().Add(35 * time.Second)
	for {
		acceptedProof = sign()
		result, wire = positive(acceptedProof)
		summary, err = verifyLiveRead(result, input.Dataset, fixture.Tenant, site, audience)
		if err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("live sensors/hazard not complete", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	status, _, err := send("POST", request.Path, wire)
	if err != nil || status != 403 {
		t.Fatalf("replay status=%d err=%v", status, err)
	}
	fresh := sign()
	tampered := append(append([]byte(nil), body...), byte(' '))
	status, _, err = send("POST", request.Path, envelope(fresh, tampered))
	if err != nil || status != 403 {
		t.Fatalf("body tamper status=%d err=%v", status, err)
	}
	// The denied body must not consume the untouched proof.
	_, _ = positive(fresh)
	for _, route := range []struct {
		method, path string
		want         int
	}{{"POST", "/v1/components", 404}, {"POST", "/v1/tasks", 404}, {"POST", "/v1/task/stop", 404}, {"DELETE", request.Path, 405}} {
		status, _, err = send(route.method, route.path, wire)
		if err != nil || status != route.want {
			t.Fatalf("mutating route %s %s: status=%d err=%v", route.method, route.path, status, err)
		}
	}
	stop()
	stop = start()
	defer stop()
	current, err := clock.Now()
	if err != nil {
		t.Fatal(err)
	}
	_, err = request.Verify(acceptedProof, request.Expected{Tenant: fixture.Tenant, Site: site, Audience: audience, Operation: request.OperationRead, Method: request.Method, Path: request.Path, PolicyRevision: 1}, body, certificates, current.Earliest, current.Latest)
	if err != nil {
		t.Fatal("restart replay test proof expired before checking durable replay", err)
	}
	status, _, err = send("POST", request.Path, wire)
	if err != nil || status != 403 {
		t.Fatalf("restart replay status=%d err=%v", status, err)
	}
	until = time.Now().Add(35 * time.Second)
	for {
		after, _ := positive(sign())
		if _, err = verifyLiveRead(after, input.Dataset, fixture.Tenant, site, audience); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("fresh post-restart dataset incomplete", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	stop()
	report := map[string]any{"schema": "wendy.world.live-production-read-test.v1", "result": "passed", "identity": "generated UNTRUSTED ML-DSA test operator and root; no enrolled identity", "operator_principal": fixture.Principal, "authority_site": site, "authority_audience": audience, "dataset": input.Dataset, "tls": "exact pinned generated self-signed end entity, TLS 1.3, numeric loopback", "tls_certificate_sha256": hex.EncodeToString(tlsPin[:]), "time": clock.Report(), "snapshot": summary, "checks": []string{"actual world-authd.run startup", "native authenticated quorum", "live MuJoCo camera/gas/audio and inferred hazard provenance", "read 200", "replay 403", "body tamper 403 with untouched proof subsequently 200", "mutation routes 404/405", "service restart", "still-valid proof replay 403 after restart", "independent fresh proof 200 after restart"}, "limits": []string{"generated untrusted identity, not enrolled-operator interoperability", "all sensor data and robot execution are simulated", "no private keys or simulation reader tokens in report", "no physical actuation or production enrollment"}}
	reportBytes, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(input.Report, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(append(reportBytes, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

func verifyLiveRead(response readservice.Response, dataset readservice.Dataset, tenant, site, audience string) (liveReadSummary, error) {
	var out liveReadSummary
	if response.Schema != readservice.Schema || response.Dataset != dataset || response.Authority.Domain != "physical" || response.Authority.Tenant != tenant || response.Authority.Site != site || response.Authority.Audience != audience {
		return out, errors.New("read authority or simulation dataset changed")
	}
	expected := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}
	sensors := map[string]sensing.Sample{}
	var hazard *fusion.Hazard
	for _, raw := range response.Components {
		var c world.Component
		if json.Unmarshal(raw, &c) != nil {
			return out, errors.New("invalid component")
		}
		if c.Key.Tenant != dataset.SourceTenant || c.Key.Site != dataset.SourceSite || c.Frame.ID != dataset.Frame.ID || c.Frame.Revision != dataset.Frame.Revision || c.Timing.ClockDomain != "simulation:"+dataset.RunID {
			return out, errors.New("component scope changed")
		}
		switch c.Key.Type {
		case sensing.ComponentType:
			var s sensing.Sample
			if json.Unmarshal(c.Payload, &s) != nil {
				return out, errors.New("invalid sample")
			}
			if c.Provenance.Origin != world.Simulated || c.Provenance.Simulation == nil || c.Provenance.Simulation.RunID != dataset.RunID || s.RunID != dataset.RunID || s.SiteID != dataset.SourceSite || s.Frame != c.Frame || expected[s.SensorID] != s.Kind || s.Sequence == 0 || !sensing.DigestValid(s.PackageSHA256) {
				return out, errors.New("sensor lineage mismatch")
			}
			if _, exists := sensors[s.SensorID]; exists {
				return out, errors.New("duplicate expected sensor")
			}
			if s.Kind == "camera" || s.Kind == "audio" {
				mime := "image/png"
				if s.Kind == "audio" {
					mime = "audio/wav"
				}
				if s.Evidence == nil || s.Evidence.ContentType != mime || s.Evidence.Validate() != nil {
					return out, errors.New("camera/audio evidence reference missing")
				}
			}
			sensors[s.SensorID] = s
		case fusion.HazardType:
			if c.Provenance.Origin != world.Inferred || c.Provenance.ModelID != fusion.ModelID || len(c.Provenance.Inputs) != 4 {
				return out, errors.New("hazard ancestry missing")
			}
			for _, in := range c.Provenance.Inputs {
				if in.Origin != world.Simulated || in.Simulation == nil || in.Simulation.RunID != dataset.RunID {
					return out, errors.New("hazard lost synthetic ancestry")
				}
			}
			var h fusion.Hazard
			if json.Unmarshal(c.Payload, &h) != nil {
				return out, errors.New("invalid hazard")
			}
			hazard = &h
			out.HazardEntity = c.Key.Entity
		}
	}
	if len(sensors) != 4 || hazard == nil || len(hazard.Inputs) != 4 {
		return out, errors.New("four sensors and fused hazard required")
	}
	seen := map[string]bool{}
	for _, in := range hazard.Inputs {
		s, ok := sensors[in.SensorID]
		if !ok || seen[in.SensorID] || in.Kind != s.Kind || in.Sample.SensorID != s.SensorID || in.Sample.RunID != dataset.RunID || in.Sample.SiteID != dataset.SourceSite || in.Sample.Frame != s.Frame || in.Sample.PackageSHA256 != s.PackageSHA256 || in.SampleID != in.Sample.ID {
			return out, errors.New("hazard input lineage differs from live source")
		}
		seen[in.SensorID] = true
		if out.PackageSHA256 != "" && out.PackageSHA256 != s.PackageSHA256 {
			return out, errors.New("sensors use different world packages")
		}
		out.PackageSHA256 = s.PackageSHA256
	}
	for id := range sensors {
		out.Sensors = append(out.Sensors, id)
	}
	sort.Strings(out.Sensors)
	out.HazardInputs = len(hazard.Inputs)
	out.Components = len(response.Components)
	if out.HazardEntity == "" {
		return out, fmt.Errorf("missing hazard entity")
	}
	return out, nil
}
