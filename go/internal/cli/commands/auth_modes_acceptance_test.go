package commands

// Acceptance fixtures use the installed CLI executable, not command helpers.
// Their Agent is closedtest's real access/TLS stack with inert app/Cloud peers.
// No container, shell, updater, installer or physical board is started here.
import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type authComputer struct{ dir, binary, fingerprint, public string }

func authComputerFor(t *testing.T) authComputer {
	t.Helper()
	bin := os.Getenv("WENDY_AUTH_MODES_CLI")
	if bin == "" {
		t.Skip("acceptance tier requires WENDY_AUTH_MODES_CLI built from this checkout")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("WENDY_AUTH_MODES_CLI must be absolute")
	}
	c := authComputer{dir: t.TempDir(), binary: bin}
	if err := os.WriteFile(filepath.Join(c.dir, "config.json"), []byte(`{"completionPromptDismissed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	k, err := localkey.LoadOrCreate(filepath.Join(c.dir, localkey.FileName))
	if err != nil {
		t.Fatal(err)
	}
	c.fingerprint, err = localkey.Fingerprint(k)
	if err != nil {
		t.Fatal(err)
	}
	der, err := localkey.PublicKeyDER(k)
	if err != nil {
		t.Fatal(err)
	}
	c.public = filepath.Join(c.dir, "public.pem")
	if err := os.WriteFile(c.public, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c authComputer) run(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	argv := []string{"--json=false"}
	if addr != "" {
		argv = append(argv, "--device", addr)
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, c.binary, argv...)
	cmd.Env = c.environment()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("CLI deadline: %s", strings.Join(args, " "))
	}
	if bytes.Contains(out, []byte("PRIVATE KEY")) {
		t.Fatal("CLI disclosed private key material")
	}
	t.Logf("$ wendy %s\n%s", strings.Join(args, " "), out)
	return string(out), err
}
func (c authComputer) environment() []string {
	var env []string
	// No ambient socket, credentials, config, device, proxy or session broker.
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(name, "WENDY_") || strings.HasPrefix(name, "SUDO_") || name == "HOME" {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "HOME="+c.dir, "WENDY_CONFIG_DIR="+c.dir, "WENDY_ANALYTICS=false", "WENDY_RUN_NO_CLOUD_FALLBACK=1")
	// Insecure Cloud transport is confined to the synthetic loopback Cloud.
	if os.Getenv("WENDY_AUTH_MODES_SYNTHETIC_CLOUD") == "true" {
		env = append(env, "WENDY_CLOUD_INSECURE=true")
	}
	return env
}

func (c authComputer) ok(t *testing.T, addr, want string, args ...string) string {
	t.Helper()
	out, err := c.run(t, addr, args...)
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("CLI: %v; want %q in %q", err, want, out)
	}
	return out
}
func (c authComputer) refused(t *testing.T, addr, want string, args ...string) {
	t.Helper()
	out, err := c.run(t, addr, args...)
	if err == nil || !strings.Contains(out, want) {
		t.Fatalf("want diagnostic %q, got err=%v out=%q", want, err, out)
	}
}
func authDevice(t *testing.T) *closedtest.Device { return closedtest.Start(t, t.TempDir()) }
func authClosed(t *testing.T) (*closedtest.Device, authComputer) {
	t.Helper()
	c := authComputerFor(t)
	d := authDevice(t)
	c.ok(t, d.Addr, "Closed.", "device", "access", "close", "--yes")
	d.Restart(t)
	c.ok(t, d.Addr, "Your role: owner", "device", "access")
	return d, c
}
func authAdd(t *testing.T, d *closedtest.Device, owner, c authComputer, role string) {
	t.Helper()
	owner.ok(t, d.Addr, c.fingerprint, "device", "keys", "add", c.public, "--role", role, "--label", role+" fixture")
	c.ok(t, d.Addr, "Your role: "+role, "device", "access")
}
func authSnapshot(t *testing.T, d *closedtest.Device) []byte {
	t.Helper()
	b, err := json.Marshal(d.Keys.List())
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func authUnchanged(t *testing.T, d *closedtest.Device, b []byte) {
	t.Helper()
	if !bytes.Equal(b, authSnapshot(t, d)) || !d.Keys.Closed() {
		t.Fatal("refusal changed trusted keys or shape")
	}
}

func TestAuthModesA02Open(t *testing.T) {
	c := authComputerFor(t)
	d := authDevice(t)
	c.ok(t, d.Addr, "Access: Open", "device", "access")
	for _, args := range [][]string{{"device", "keys", "list"}, {"device", "access", "keyless", "on"}} {
		c.refused(t, d.Addr, "close it first", args...)
	}
	if d.Keys.Closed() || len(d.Keys.List()) != 0 {
		t.Fatal("Open management refusal created access state")
	}
}
func TestAuthModesA03FirstOwner(t *testing.T) {
	d, c := authClosed(t)
	c.ok(t, d.Addr, c.fingerprint, "device", "keys", "list")
	if len(d.Keys.List()) != 1 || d.Keys.KeylessApps() {
		t.Fatal("first owner state")
	}
	b := authSnapshot(t, d)
	c.refused(t, d.Addr, "already closed", "device", "access", "close", "--yes")
	authUnchanged(t, d, b)
	stranger := authComputerFor(t)
	stranger.refused(t, d.Addr, stranger.fingerprint, "device", "access")
}
func TestAuthModesA04Consent(t *testing.T) {
	c := authComputerFor(t)
	d := authDevice(t)
	c.refused(t, d.Addr, "--yes", "device", "access", "close")
	if d.Keys.Closed() {
		t.Fatal("close without consent changed Open state")
	}
	c.ok(t, d.Addr, "Closed.", "device", "access", "close", "--yes")
	d.Restart(t)
	other := authComputerFor(t)
	authAdd(t, d, c, other, "viewer")
	b := authSnapshot(t, d)
	c.refused(t, d.Addr, "--yes", "device", "access", "open")
	authUnchanged(t, d, b)
	c.refused(t, d.Addr, "--yes", "device", "keys", "remove", other.fingerprint)
	authUnchanged(t, d, b)
	c.ok(t, d.Addr, "Removed", "device", "keys", "remove", other.fingerprint, "--yes")
	d.Restart(t)
	c.ok(t, d.Addr, "Open.", "device", "access", "open", "--yes")
	d.Restart(t)
	c.ok(t, d.Addr, "Access: Open", "device", "access")
}
func TestAuthModesA05Untrusted(t *testing.T) {
	d, owner := authClosed(t)
	s := authComputerFor(t)
	b := authSnapshot(t, d)
	for _, cloudCert := range []bool{false, true} {
		if cloudCert {
			a := oidcEnrollmentAuth(t)
			raw, err := json.Marshal(&config.Config{Auth: []config.AuthConfig{*a}})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(s.dir, "config.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		out, err := s.run(t, d.Addr, "device", "access")
		if err == nil || !strings.Contains(out, s.fingerprint) || !strings.Contains(out, "owner") || strings.Contains(out, owner.fingerprint) {
			t.Fatalf("untrusted diagnostic: %v %q", err, out)
		}

		structured, err := s.run(t, d.Addr, "device", "access", "--json")
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
				Message   string `json:"message"`
			} `json:"error"`
		}
		if err == nil || json.Unmarshal([]byte(structured), &envelope) != nil || envelope.Error.Code != "device_auth_required" || envelope.Error.Retryable || !strings.Contains(envelope.Error.Message, "does not trust this computer's key") {
			t.Fatalf("structured key refusal must not be retryable transport: %v %s", err, structured)
		}
		authUnchanged(t, d, b)
	}
}
func TestAuthModesA06Roles(t *testing.T) {
	d, owner := authClosed(t)
	for _, role := range []string{"owner", "operator", "viewer"} {
		c := authComputerFor(t)
		authAdd(t, d, owner, c, role)
		out := owner.ok(t, d.Addr, c.fingerprint, "device", "keys", "list")
		if !strings.Contains(out, role+" fixture") || !strings.Contains(out, "(this computer)") {
			t.Fatal("missing label or current computer marker")
		}

		listed := owner.ok(t, d.Addr, c.fingerprint, "device", "keys", "list", "--json")
		var entries struct {
			Keys []trustedKeyJSON `json:"keys"`
		}
		if err := json.Unmarshal([]byte(listed), &entries); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range entries.Keys {
			if entry.Fingerprint == c.fingerprint {
				found = true
				if entry.Role != role || entry.AddedBy != owner.fingerprint || entry.ThisKey {
					t.Fatalf("key role/actor/current identity: %+v", entry)
				}
			}
		}
		if !found {
			t.Fatal("new key omitted from normal JSON list")
		}
		if role == "owner" {
			continue
		}

		b := authSnapshot(t, d)
		for _, args := range [][]string{{"device", "keys", "list"}, {"device", "keys", "add", c.public, "--role", "owner"}, {"device", "keys", "remove", owner.fingerprint, "--yes"}, {"device", "access", "open", "--yes"}, {"device", "access", "keyless", "on"}, {"device", "access", "close", "--yes"}} {
			c.refused(t, d.Addr, "owner", args...)
			authUnchanged(t, d, b)
		}
	}
}
func TestAuthModesA07ChangeRole(t *testing.T) {
	d, o := authClosed(t)
	c := authComputerFor(t)
	authAdd(t, d, o, c, "operator")
	b := authSnapshot(t, d)
	o.refused(t, d.Addr, "already", "device", "keys", "add", c.public, "--role", "viewer")
	authUnchanged(t, d, b)
	c.ok(t, d.Addr, "Your role: operator", "device", "access")
	o.ok(t, d.Addr, "Removed", "device", "keys", "remove", c.fingerprint, "--yes")
	d.Restart(t)
	authAdd(t, d, o, c, "viewer")
	b = authSnapshot(t, d)
	o.refused(t, d.Addr, "no trusted key", "device", "keys", "remove", "sha256:"+strings.Repeat("0", 64), "--yes")
	authUnchanged(t, d, b)
}
func TestAuthModesA08LastOwner(t *testing.T) {
	d, o := authClosed(t)
	b := authSnapshot(t, d)
	o.refused(t, d.Addr, "last owner", "device", "keys", "remove", o.fingerprint, "--yes")
	authUnchanged(t, d, b)
	other := authComputerFor(t)
	authAdd(t, d, o, other, "owner")
	other.ok(t, d.Addr, "Removed", "device", "keys", "remove", o.fingerprint, "--yes")
	d.Restart(t)
	o.refused(t, d.Addr, o.fingerprint, "device", "keys", "list")
	other.ok(t, d.Addr, "Your role: owner", "device", "access")
	if len(d.Keys.List()) != 1 {
		t.Fatal("removed owner survived")
	}
}
func TestAuthModesA18A19Keyless(t *testing.T) {
	d, o := authClosed(t)
	s := authComputerFor(t)
	operator := authComputerFor(t)
	viewer := authComputerFor(t)
	authAdd(t, d, o, operator, "operator")
	authAdd(t, d, o, viewer, "viewer")
	b := authSnapshot(t, d)
	o.ok(t, d.Addr, "Keyless apps on", "device", "access", "keyless", "on")
	d.Restart(t)
	s.ok(t, d.Addr, "Your role: none", "device", "access")
	s.refused(t, d.Addr, "owner", "device", "keys", "list")
	authUnchanged(t, d, b)
	o.ok(t, d.Addr, "Keyless apps off", "device", "access", "keyless", "off")
	d.Restart(t)
	s.refused(t, d.Addr, s.fingerprint, "device", "access")
	authUnchanged(t, d, b)
	o.ok(t, d.Addr, "Your role: owner", "device", "access")
	operator.ok(t, d.Addr, "Your role: operator", "device", "access")
	viewer.ok(t, d.Addr, "Your role: viewer", "device", "access")
	d.Restart(t)
	if d.Keys.KeylessApps() {
		t.Fatal("keyless off did not persist")
	}
}
func TestAuthModesA23Reopen(t *testing.T) {
	d, o := authClosed(t)
	other := authComputerFor(t)
	authAdd(t, d, o, other, "viewer")
	o.ok(t, d.Addr, "Open.", "device", "access", "open", "--yes")
	d.Restart(t)
	o.ok(t, d.Addr, "Access: Open", "device", "access")
	other.refused(t, d.Addr, "was Closed", "device", "access")
	// Independently confirmed Open, deliberate cleanup of the exact dialed name.
	other.ok(t, "", "", "device", "unpin", d.Addr)
	other.ok(t, d.Addr, "Closed.", "device", "access", "close", "--yes")
	d.Restart(t)
	other.ok(t, d.Addr, "Your role: owner", "device", "access")
	if len(d.Keys.List()) != 1 || d.Keys.List()[0].Fingerprint() != other.fingerprint || d.Keys.KeylessApps() {
		t.Fatal("dormant trusted keys returned")
	}
}

// WDY-4393 intentionally asserts the desired contract. Do not invert it to
// expect the reproduced stale pin. A held outgoing listener makes the timing
// deterministic without changing any production code.
func TestAuthModesA23ReopenDuringRestart(t *testing.T) {
	d, o := authClosed(t)
	o.ok(t, d.Addr, "Open.", "device", "access", "open", "--yes")
	_, _ = o.run(t, d.Addr, "device", "access") // one read while restart is pending
	d.Restart(t)
	o.ok(t, d.Addr, "Access: Open", "device", "access")
}

// WDY-4392 goes through public argument parsing, session selection, target
// resolution and signed Cloud request. A synthetic provisioning peer records
// submission; this is not proof of issuance or physical Enrolled startup.
func TestAuthModesA24EnrollNormalEntry(t *testing.T) {
	t.Setenv("WENDY_AUTH_MODES_SYNTHETIC_CLOUD", "true")
	for _, keyless := range []bool{false, true} {
		for _, role := range []string{"owner", "operator", "viewer", "keyless"} {
			t.Run(fmt.Sprintf("keyless=%t/%s", keyless, role), func(t *testing.T) {
				owner := authComputerFor(t)
				caller := owner
				if role != "owner" {
					caller = authComputerFor(t)
				}
				a := &acmeProvisioningServer{}
				d := closedtest.Start(t, t.TempDir(), func(s *grpc.Server) {
					pb.RegisterWendyProvisioningServiceServer(s, a)
					pb.RegisterWendyDeviceInfoServiceServer(s, a)
				})
				owner.ok(t, d.Addr, "Closed.", "device", "access", "close", "--yes")
				d.Restart(t)
				if role == "operator" || role == "viewer" {
					authAdd(t, d, owner, caller, role)
				}
				if keyless {
					owner.ok(t, d.Addr, "Keyless apps on", "device", "access", "keyless", "on")
					d.Restart(t)
				}
				cloud := &oidcEnrollmentServer{err: status.Error(codes.PermissionDenied, "fixture: Cloud enrollment authority required"), response: &cloudpb.EnrollDeviceResponse{CredentialKind: "eab", EabKeyId: "fixture-eab", EabHmacKey: strings.Repeat("ab", 32)}}
				srv := grpc.NewServer()
				cloudpb.RegisterDeviceEnrollmentServiceServer(srv, cloud)
				cloudpb.RegisterOperatorSessionServiceServer(srv, cloud)
				auth := oidcEnrollmentAuth(t)
				auth.CloudGRPC = serveTest(t, srv)
				raw, err := json.Marshal(&config.Config{Auth: []config.AuthConfig{*auth}})
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(caller.dir, "config.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				before := authSnapshot(t, d)
				out, err := caller.run(t, d.Addr, "device", "enroll", "--name", "auth-modes-fixture", "--cloud-grpc", auth.CloudGRPC)
				if role == "owner" {
					if err == nil || a.req != nil || cloud.req == nil || !strings.Contains(out, "fixture: Cloud enrollment authority required") {
						t.Fatalf("normal owner enrollment: %v %s; agent=%t cloud=%t", err, out, a.req != nil, cloud.req != nil)
					}
				} else if err == nil || a.req != nil || cloud.req != nil || (!strings.Contains(out, "owner") && !strings.Contains(out, "does not trust")) {
					t.Fatalf("nonowner enrollment: %v %s; agent=%t cloud=%t", err, out, a.req != nil, cloud.req != nil)
				}
				authUnchanged(t, d, before)
			})
		}
	}
}

// Keep generated service registrations linked in the acceptance binary.
var _ = agentpb.WendyAgentService_ServiceDesc

func TestAuthModesA10Inputs(t *testing.T) {
	d, o := authClosed(t)
	algs := map[string]func() (crypto.Signer, error){
		"RSA":       func() (crypto.Signer, error) { return rsa.GenerateKey(rand.Reader, 2048) },
		"P-256":     func() (crypto.Signer, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) },
		"Ed25519":   func() (crypto.Signer, error) { _, k, e := ed25519.GenerateKey(rand.Reader); return k, e },
		"ML-DSA-44": func() (crypto.Signer, error) { return mldsa.GenerateKey(mldsa.MLDSA44()) },
		"ML-DSA-87": func() (crypto.Signer, error) { return mldsa.GenerateKey(mldsa.MLDSA87()) },
	}
	for name, gen := range algs {
		t.Run(name, func(t *testing.T) {
			k, err := gen()
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalPKIXPublicKey(k.Public())
			if err != nil {
				t.Fatal(err)
			}
			pub := filepath.Join(t.TempDir(), "unsupported.pem")
			if err = os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
				t.Fatal(err)
			}
			before := authSnapshot(t, d)
			o.refused(t, d.Addr, "ML-DSA-65", "device", "keys", "add", pub, "--role", "viewer")
			authUnchanged(t, d, before)
			open := authDevice(t)
			conn, err := grpc.NewClient(open.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = pb.NewWendyAccessServiceClient(conn).CloseDevice(ctx, &pb.CloseDeviceRequest{PublicKey: der})
			if status.Code(err) != codes.InvalidArgument || open.Keys.Closed() {
				t.Fatalf("raw CloseDevice accepted %s: %v", name, err)
			}
		})
	}
	labelKey := authComputerFor(t)
	beforeLabels := authSnapshot(t, d)
	for _, label := range []string{strings.Repeat("x", 129), "invalid\nlabel"} {
		o.refused(t, d.Addr, "label", "device", "keys", "add", labelKey.public, "--role", "viewer", "--label", label)
		authUnchanged(t, d, beforeLabels)
	}
	o.ok(t, d.Addr, labelKey.fingerprint, "device", "keys", "add", labelKey.public, "--role", "viewer", "--label", strings.Repeat("x", 128))
	list := o.ok(t, d.Addr, strings.Repeat("x", 128), "device", "keys", "list")
	if !strings.Contains(list, labelKey.fingerprint) {
		t.Fatal("valid boundary label failed round trip")
	}
	before := authSnapshot(t, d)
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a public key"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"device", "keys", "add", junk, "--role", "viewer"},
		{"device", "keys", "add", filepath.Join(o.dir, localkey.FileName), "--role", "viewer"},
		{"device", "keys", "add", o.public},
		{"device", "keys", "add", o.public, "--role", "root"},
	} {
		out, err := o.run(t, d.Addr, args...)
		if err == nil || strings.TrimSpace(out) == "" {
			t.Fatalf("invalid key/role did not fail usefully: %v %s", err, out)
		}
		authUnchanged(t, d, before)
	}
}

func TestAuthModesA01PublicOutput(t *testing.T) {
	d, o := authClosed(t)
	c := authComputerFor(t)
	shown := c.ok(t, "", c.fingerprint, "auth", "local-key", "show")
	if err := os.WriteFile(c.public, []byte(shown), 0600); err != nil {
		t.Fatal(err)
	}
	authAdd(t, d, o, c, "operator")
	o.ok(t, d.Addr, "Removed", "device", "keys", "remove", c.fingerprint, "--yes")
	d.Restart(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.binary, "--device", d.Addr, "device", "keys", "add", "-", "--role", "viewer")
	cmd.Env = o.environment()
	cmd.Stdin = strings.NewReader(shown)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), c.fingerprint) {
		t.Fatalf("whole show output via stdin: %v %s", err, out)
	}
	c.ok(t, d.Addr, "Your role: viewer", "device", "access")
}

func TestAuthModesA01UnreadableIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files; unreadable-key tier requires an unprivileged user")
	}
	c := authComputerFor(t)
	path := filepath.Join(c.dir, localkey.FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	c.refused(t, "", "permission", "auth", "local-key", "show")
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unreadable key was replaced")
	}
	c.ok(t, "", c.fingerprint, "auth", "local-key", "show")
}
