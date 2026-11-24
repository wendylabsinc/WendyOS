package commands

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

// runWendy runs the CLI against the device at addr and returns what it
// printed (stdout and stderr) and its error.
func runWendy(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("WENDY_ANALYTICS", "false")
	origJSON, origDevice := jsonOutput, deviceFlag
	t.Cleanup(func() { jsonOutput, deviceFlag = origJSON, origDevice })
	root := NewRootCmd()
	root.SetArgs(append([]string{"--json=false", "--device", addr}, args...))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.Execute()
	t.Logf("$ wendy %s\n%s%v", strings.Join(args, " "), out.String(), errOrEmpty(err))
	return out.String(), err
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return "Error: " + err.Error()
}

func mustRun(t *testing.T, addr string, want string, args ...string) string {
	t.Helper()
	out, err := runWendy(t, addr, args...)
	if err != nil {
		t.Fatalf("wendy %s: %v", strings.Join(args, " "), err)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("wendy %s printed %q, want %q", strings.Join(args, " "), out, want)
	}
	return out
}

func useComputerKey(t *testing.T, key crypto.Signer) {
	t.Helper()
	orig := localOperatorKeyFn
	localOperatorKeyFn = func() (crypto.Signer, error) { return key, nil }
	t.Cleanup(func() { localOperatorKeyFn = orig })
}

// Every access command against the agent's real access service, role check
// and key listener: close an Open device, manage keys, the keyless switch,
// a computer without a key, then reopen. A real agent restarts after each
// shape change; the test restarts its device from the same config dir.
func TestDeviceAccessCommandsEndToEnd(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	dir := t.TempDir()
	owner := useLocalKey(t)
	ownerFP, _ := localkey.Fingerprint(owner)

	dev := closedtest.Start(t, dir)
	mustRun(t, dev.Addr, "Access: Open", "device", "access")
	mustRun(t, dev.Addr, "Closed. This computer's key "+ownerFP+" is the owner", "device", "access", "close", "--yes")
	if dev.Restarts.Load() != 1 {
		t.Fatalf("close asked for %d restarts, want 1", dev.Restarts.Load())
	}

	dev.Restart(t) // into Closed
	mustRun(t, dev.Addr, "Your role: owner", "device", "access")

	bob, _ := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
	bobFP, _ := localkey.Fingerprint(bob)
	bobDER, _ := localkey.PublicKeyDER(bob)
	bobPub := filepath.Join(t.TempDir(), "bob.pub")
	if err := os.WriteFile(bobPub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: bobDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	useComputerKey(t, bob)

	// bob has no key on the device and keyless apps are off.
	out, err := runWendy(t, dev.Addr, "device", "access")
	if err == nil || !strings.Contains(err.Error(), "is Closed and does not trust this computer's key ("+bobFP+")") {
		t.Fatalf("keyless caller: out=%q err=%v, want the ask-an-owner refusal", out, err)
	}

	useComputerKey(t, owner)
	mustRun(t, dev.Addr, "Trusted "+bobFP+" as operator.", "device", "keys", "add", bobPub, "--role", "operator", "--label", "bob")
	out = mustRun(t, dev.Addr, bobFP, "device", "keys", "list")
	if !regexp.MustCompile(ownerFP + `\s+owner\s+\S+ \(this computer\)`).MatchString(out) {
		t.Fatalf("list does not mark this computer's owner key:\n%s", out)
	}

	useComputerKey(t, bob)
	mustRun(t, dev.Addr, "Your role: operator", "device", "access")
	if _, err := runWendy(t, dev.Addr, "device", "keys", "list"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("operator listed keys: %v", err)
	}

	useComputerKey(t, owner)
	mustRun(t, dev.Addr, "Removed "+bobFP+".", "device", "keys", "remove", bobFP, "--yes")
	mustRun(t, dev.Addr, "Keyless apps on", "device", "access", "keyless", "on")

	// bob, no longer trusted, gets in keyless.
	useComputerKey(t, bob)
	mustRun(t, dev.Addr, "Your role: none (connected without a key)", "device", "access")

	useComputerKey(t, owner)
	mustRun(t, dev.Addr, "Open. Every key was dropped", "device", "access", "open", "--yes")
	if _, err := os.Stat(filepath.Join(dir, trustedkeys.FileName)); !os.IsNotExist(err) {
		t.Fatal("access file left after open")
	}
	dev.Restart(t) // into Open: the pin on its key must not refuse it
	mustRun(t, dev.Addr, "Access: Open", "device", "access")
}

// Discovery shows a Closed device as such, with the legend entry explaining it.
func TestDiscoverShowsClosedShape(t *testing.T) {
	for _, c := range []struct {
		lan  models.LANDevice
		want string
	}{
		{models.LANDevice{Access: "closed"}, "Closed"},
		{models.LANDevice{Access: "open"}, "Unprovisioned"},
		{models.LANDevice{}, "Unprovisioned"},
		{models.LANDevice{IsMTLS: true, Access: "enrolled"}, "Provisioned"},
	} {
		if got := lanProvisionedDisplay(&c.lan); got != c.want {
			t.Errorf("%+v: %q, want %q", c.lan, got, c.want)
		}
	}
	legend := tui.DeviceTableLegend([]tui.PickerItem{{Provisioned: "Closed"}})
	if !strings.Contains(legend, tui.LegendClosed) {
		t.Fatalf("legend %q does not explain the Closed glyph", legend)
	}
	if strings.Contains(tui.DeviceTableLegend([]tui.PickerItem{{Provisioned: "Unprovisioned"}}), tui.GlyphClosed) {
		t.Fatal("Closed legend shown without a Closed row")
	}
}

// An operator pushes to a Closed device's registry over TLS with this
// computer's key, bound to the device key; with keyless apps on the registry
// is plain HTTP and no key dialer is used.
func TestRegistryOverLocalKey(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	owner := useLocalKey(t)
	dir := closedDir(t, owner)
	dev := closedtest.Start(t, dir)

	certPEM, keyPEM, err := mtls.ClosedServerIdentity(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mtls.NewTrustedKeyTLSConfig(certPEM, keyPEM, dev.Keys, trustedkeys.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	regPort := ln.Addr().(*net.TCPAddr).Port

	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", dev.Addr), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dial, err := registryDialerFor(ladderCtx(t), conn)
	if err != nil || dial == nil {
		t.Fatalf("registryDialerFor: nil=%v, %v; want a key dialer", dial == nil, err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dial(ctx, regPort)
	}}}
	resp, err := client.Get("http://registry/v2/")
	if err != nil {
		t.Fatalf("GET /v2/ over the local key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v2/ = %d", resp.StatusCode)
	}

	if err := dev.Keys.SetKeylessApps(true, dev.Keys.List()[0].Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if dial, err := registryDialerFor(ladderCtx(t), conn); err != nil || dial != nil {
		t.Fatalf("keyless apps on: registryDialerFor nil=%v, %v; want plain HTTP (nil)", dial == nil, err)
	}
}

// The whole output of `wendy auth local-key show` is what `keys add` takes.
func TestLocalKeyShowOutputIsAddableAsIs(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	var shown bytes.Buffer
	if err := runAuthLocalKeyShow(&shown, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "alice.pub")
	if err := os.WriteFile(path, shown.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	der, err := readPublicKeyArg(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fp := trustedkeys.Fingerprint(der); !strings.Contains(shown.String(), "Fingerprint: "+fp) {
		t.Fatalf("show printed a fingerprint other than its key's %s:\n%s", fp, shown.String())
	}
}

func TestReadPublicKeyArgRefusesPrivateKeysAndJunk(t *testing.T) {
	priv := filepath.Join(t.TempDir(), localkey.FileName)
	if _, err := localkey.LoadOrCreate(priv); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublicKeyArg(priv, nil); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("private key accepted: %v", err)
	}
	if _, err := readPublicKeyArg("-", strings.NewReader("not a key")); err == nil {
		t.Fatal("junk accepted")
	}
}

// Changing who may reach a device asks first; without a terminal it needs
// --yes and changes nothing.
func TestDeviceAccessChangesNeedYesWithoutATerminal(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	useLocalKey(t)
	dev := closedtest.Start(t, t.TempDir())
	for _, args := range [][]string{
		{"device", "access", "close"},
		{"device", "access", "open"},
		{"device", "keys", "remove", "sha256:00"},
	} {
		if _, err := runWendy(t, dev.Addr, args...); err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Errorf("wendy %s: err = %v, want a request for --yes", strings.Join(args, " "), err)
		}
	}
	mustRun(t, dev.Addr, "Access: Open", "device", "access")
}

type labelAccessClient struct {
	agentpbv2.WendyAccessServiceClient
	keys []*agentpbv2.TrustedKey
}

func (c labelAccessClient) ListTrustedKeys(context.Context, *agentpbv2.ListTrustedKeysRequest, ...grpc.CallOption) (*agentpbv2.ListTrustedKeysResponse, error) {
	return &agentpbv2.ListTrustedKeysResponse{Keys: c.keys}, nil
}

// A label is another owner's free text: it cannot reach the terminal with
// control characters or forge rows.
func TestDeviceKeysListEscapesLabels(t *testing.T) {
	useLocalKey(t)
	c := labelAccessClient{keys: []*agentpbv2.TrustedKey{
		{Fingerprint: "sha256:aa", Role: agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER, Label: "alice\nsha256:ff\towner\x1b[2J"},
		{Fingerprint: "sha256:bb", Role: agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER, Label: "bob"},
	}}
	var out bytes.Buffer
	if err := runDeviceKeysList(context.Background(), c, &out, false); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 3 || strings.ContainsRune(out.String(), '\x1b') {
		t.Fatalf("printed %d lines (want 3: header and 2 keys) or an escape:\n%q", lines, out.String())
	}
}

// The default label fits the agent's limit, cut on a character boundary.
func TestLocalKeyLabelFitsTheAgentLimit(t *testing.T) {
	long := strings.Repeat("é", maxKeyLabelLen) // 2 bytes each
	got := capKeyLabel("host\x07/" + long)
	if len(got) > maxKeyLabelLen || !utf8.ValidString(got) || strings.ContainsRune(got, '\x07') {
		t.Fatalf("capKeyLabel = %q (%d bytes), want valid UTF-8 within %d bytes, no control characters", got, len(got), maxKeyLabelLen)
	}
}
