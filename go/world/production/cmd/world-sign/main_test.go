package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/operator"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type testClock struct{ window trustedtime.Interval }

func (c testClock) Now() (trustedtime.Interval, error) { return c.window, nil }

type failCustody struct{ t *testing.T }

func (c failCustody) Get(context.Context, string) ([]byte, error) {
	c.t.Fatal("unexpected actual custody call")
	return nil, errors.New("custody disallowed")
}

type fixture struct {
	f          *testidentity.Fixture
	base, sign []string
	body       []byte
	window     trustedtime.Interval
	deps       dependencies
	trust      *wendycert.Verifier
	config     string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := testidentity.New(t, now)
	dir := t.TempDir()
	config := filepath.Join(dir, "fixture.json")
	root := filepath.Join(dir, "roots.pem")
	body := filepath.Join(dir, "body.json")
	cert := func(der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	der, err := x509.MarshalPKCS8PrivateKey(f.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(der)
	cfg := map[string]any{"currentContext": "unused", "apiKey": "NOT-A-REAL-TOKEN", "auth": []any{map[string]any{"name": "chosen", "certificates": []any{map[string]any{"pemCertificate": cert(f.ChainDER[0]), "pemCertificateChain": cert(f.ChainDER[1]), "pemPrivateKey": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "principalUri": f.Principal}}}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(root, []byte(cert(f.RootDER)), 0600); err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"limit":1}`)
	if err = os.WriteFile(body, b, 0600); err != nil {
		t.Fatal(err)
	}
	w := trustedtime.Interval{Earliest: now, Latest: now.Add(time.Second), BootID: "cli-unit-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-clock"}
	v, err := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{f: f, base: []string{"--config", config, "--context", "chosen", "--tenant", f.Tenant, "--principal", f.Principal}, sign: []string{"--trust-roots", root, "--time-source", "wendy", "--site", "office", "--audience", "local-read"}, body: b, window: w, deps: dependencies{custody: failCustody{t}, acquire: func(context.Context) (trustedtime.Provider, error) { return testClock{w}, nil }}, trust: v, config: config}
}
func arguments(command string, f fixture, extra ...string) []string {
	out := []string{command}
	out = append(out, f.base...)
	if command != "describe" {
		out = append(out, f.sign...)
	}
	return append(out, extra...)
}
func TestDescribeNoTimeNoCustody(t *testing.T) {
	f := newFixture(t)
	f.deps.acquire = func(context.Context) (trustedtime.Provider, error) {
		t.Fatal("describe acquired time")
		return nil, nil
	}
	var out bytes.Buffer
	if err := run(context.Background(), arguments("describe", f), &out, f.deps); err != nil {
		t.Fatal(err)
	}
	var m operator.Metadata
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Context != "chosen" || m.Principal != f.f.Principal || m.KeyStorage != "inline-pkcs8" {
		t.Fatal("public metadata incorrect")
	}
	for _, secret := range []string{f.config, "PRIVATE KEY", "NOT-A-REAL-TOKEN", "pemCertificate", "key-"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("describe leaked restricted field")
		}
	}
}
func TestReadAndCommissionCommands(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		command, typ string
		extra        []string
	}{{"read", request.Type, []string{"--policy-revision", "1", "--body-file", filepath.Join(filepath.Dir(f.config), "body.json")}}, {"read-policy", admission.PolicyTyp, []string{"--policy-id", "local-policy", "--revision", "1", "--lifetime", "5m"}}, {"revocations", admission.RevocationsTyp, []string{"--revision", "1", "--lifetime", "5m"}}}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			var out bytes.Buffer
			before, _ := os.ReadFile(f.config)
			if err := run(context.Background(), arguments(c.command, f, c.extra...), &out, f.deps); err != nil {
				t.Fatal(err)
			}
			proof := strings.TrimSuffix(out.String(), "\n")
			if strings.Count(out.String(), "\n") != 1 {
				t.Fatal("non-proof output")
			}
			raw, _, err := jws.Verify(proof, c.typ, f.trust, f.window.Earliest, f.window.Latest)
			if err != nil {
				t.Fatal(err)
			}
			switch c.command {
			case "read":
				_, err = request.Verify(proof, request.Expected{Tenant: f.f.Tenant, Site: "office", Audience: "local-read", Operation: request.OperationRead, Method: request.Method, Path: request.Path, PolicyRevision: 1}, f.body, f.trust, f.window.Earliest, f.window.Latest)
			case "read-policy":
				var p admission.Policy
				err = jws.DecodeStrict(raw, &p)
				if len(p.Entries) != 1 || p.Entries[0].Principal != f.f.Principal {
					t.Fatal("policy principal broadened")
				}
			case "revocations":
				var r admission.Revocations
				err = jws.DecodeStrict(raw, &r)
				if len(r.RevokedPrincipals) != 0 {
					t.Fatal("bootstrap revoked identities")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(f.config)
			if !bytes.Equal(before, after) {
				t.Fatal("command mutated config")
			}
		})
	}
}
func TestInvalidFlagsFailBeforeTimeAndOutput(t *testing.T) {
	f := newFixture(t)
	f.deps.acquire = func(context.Context) (trustedtime.Provider, error) {
		t.Fatal("invalid input acquired time")
		return nil, nil
	}
	cases := [][]string{nil, {"unknown", "SECRET"}, arguments("describe", f, "--context", "chosen"), arguments("describe", f, "--time-source", "wendy"), arguments("read", f, "--policy-revision", "0"), arguments("read-policy", f, "--policy-id", "bad id", "--revision", "1", "--lifetime", "5m"), arguments("read-policy", f, "--policy-id", "p", "--revision", "1", "--lifetime", "11m"), arguments("revocations", f, "--policy-id", "unused", "--revision", "1", "--lifetime", "5m")}
	for i, args := range cases {
		var out bytes.Buffer
		err := run(context.Background(), args, &out, f.deps)
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("case %d accepted or leaked input", i)
		}
	}
	// The named clock is mandatory; no wall-time or caller supplied timestamp mode.
	args := arguments("read", f, "--policy-revision", "1", "--body-file", filepath.Join(filepath.Dir(f.config), "body.json"))
	for i := range args {
		if args[i] == "wendy" {
			args[i] = "wall"
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), args, &out, f.deps); err == nil {
		t.Fatal("wall-time fallback accepted")
	}
}
func TestTimeFailureNoProof(t *testing.T) {
	f := newFixture(t)
	f.deps.acquire = func(context.Context) (trustedtime.Provider, error) { return nil, errors.New("test quorum unavailable") }
	var out bytes.Buffer
	err := run(context.Background(), arguments("read-policy", f, "--policy-id", "p", "--revision", "1", "--lifetime", "5m"), &out, f.deps)
	if err == nil || out.Len() != 0 {
		t.Fatal("time failure emitted proof")
	}
}
