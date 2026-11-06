package services

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// testAssetUUID is a canonical Cloud asset UUID standing in for the delivery
// target's asset record across these tests.
const testAssetUUID = "5b8f3a2e-1c4d-4e9a-8b7c-6d5e4f3a2b1c"

// stubPeerDialer stands in for MeshDialer so the proxy can be tested without a
// broker or a real peer. It records the last target it was asked to dial: the
// proxy must dial the RESOLVED target — the roster-supplied device identity —
// because that identity is what the pinned TLS hop is authorized against.
type stubPeerDialer struct {
	addr  string // when set, dial this instead of a mesh peer
	err   error
	calls *int
	last  *DeliveryTarget
}

func (d stubPeerDialer) DialTarget(_ context.Context, target DeliveryTarget, _ uint16) (net.Conn, string, error) {
	if d.calls != nil {
		(*d.calls)++
	}
	if d.last != nil {
		*d.last = target
	}
	if d.err != nil {
		return nil, "", d.err
	}
	c, err := net.Dial("tcp", d.addr)
	if err != nil {
		return nil, "", err
	}
	return c, "lan-direct", nil
}

func testTarget() *agentpbv2.PushTarget {
	return &agentpbv2.PushTarget{AssetUuid: testAssetUUID, RegistryPort: 5000, Repository: "myapp:latest"}
}

func testDeliveryTarget() DeliveryTarget {
	return DeliveryTarget{AssetUUID: testAssetUUID}
}

// A push that cannot reach the target must surface WHY. Without this the
// failure reaches buildkit as a bare "connection reset by peer" on loopback,
// which says nothing about whether the mesh is unreachable or the registry
// rejected our certificate — the two causes with completely different fixes.
func TestPushProxy_SurfacesDialFailure(t *testing.T) {
	var calls int
	dialer := stubPeerDialer{err: errors.New("no route to peer"), calls: &calls}

	proxy, err := startPushProxy(context.Background(), dialer, testDeliveryTarget(), 5000, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("startPushProxy: %v", err)
	}
	defer proxy.stop()

	req, _ := http.NewRequest(http.MethodHead, "http://"+proxy.addr+"/v2/", nil)
	req.SetBasicAuth(pushProxyUser, proxy.credential)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	if got := proxy.latestError(); got == nil {
		t.Fatal("the proxy must record why the outbound dial failed, not discard it")
	}
	if calls != 1 {
		t.Fatalf("proxy dialled %d times, want one attempt; BuildKit owns request retries", calls)
	}
}

func TestPushProxy_CleanupFailureDoesNotReplaceActionableCause(t *testing.T) {
	actionableCause := errors.New("registry connection reset")
	actionable := annotateDeliveryFailure(actionableCause, 8<<20, 96<<20)

	for _, cleanup := range []error{context.Canceled, net.ErrClosed} {
		proxy := &pushProxy{}
		proxy.recordError(actionable)
		proxy.recordError(annotateDeliveryFailure(cleanup, 0, 0))

		got := proxy.latestError()
		if !errors.Is(got, actionableCause) {
			t.Fatalf("cleanup error %v replaced actionable cause: %v", cleanup, got)
		}
		if errors.Is(got, cleanup) {
			t.Fatalf("latest error = %v, want the prior actionable cause", got)
		}
	}
}

func TestPushProxy_ActionableFailureReplacesEarlierCleanup(t *testing.T) {
	proxy := &pushProxy{}
	proxy.recordError(context.Canceled)

	actionableCause := errors.New("registry refused connection")
	proxy.recordError(annotateDeliveryFailure(actionableCause, 0, 0))

	got := proxy.latestError()
	if !errors.Is(got, actionableCause) {
		t.Fatalf("latest error = %v, want later actionable cause", got)
	}
}

func TestPushProxy_RecordsCleanupWhenItIsTheOnlyCause(t *testing.T) {
	proxy := &pushProxy{}
	proxy.recordError(context.Canceled)
	if got := proxy.latestError(); !errors.Is(got, context.Canceled) {
		t.Fatalf("latest error = %v, want the only available cancellation cause", got)
	}
}

// proxyToBackend wires a proxy whose outbound hop is a plain-TCP test backend.
// The mesh dial and TLS wrapping are orthogonal to what these tests check.
func proxyToBackend(t *testing.T, handler http.Handler) *pushProxy {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	proxy, err := newPushProxy(stubPeerDialer{addr: backend.Listener.Addr().String()}, testDeliveryTarget(), 5000, nil)
	if err != nil {
		t.Fatalf("newPushProxy: %v", err)
	}
	// Wrapped rather than passed directly: serve replaces stop, and t.Cleanup
	// would otherwise capture the pre-serve value.
	t.Cleanup(func() { proxy.stop() })
	// Set before serve, so no goroutine can be reading dial yet.
	proxy.dial = func(ctx context.Context) (net.Conn, error) {
		return net.Dial("tcp", backend.Listener.Addr().String())
	}
	proxy.serve(context.Background())
	return proxy
}

// The whole point of WDY-2371: loopback is not a boundary on a shared build
// host, so an unauthenticated local process must not be able to push through
// this proxy using the agent's mesh identity and certificate.
func TestPushProxy_RefusesUnauthenticatedLocalCaller(t *testing.T) {
	var reached bool
	proxy := proxyToBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	resp, err := http.Get("http://" + proxy.addr + "/v2/evil/blobs/uploads/")
	if err != nil {
		t.Fatalf("requesting through the proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a caller with no credential", resp.StatusCode)
	}
	if reached {
		t.Fatal("an unauthenticated request reached the target device's registry")
	}
	// containerd's authorizer only retries with credentials once it sees a
	// challenge, so without this header a legitimate push never authenticates.
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestPushProxy_RefusesWrongCredential(t *testing.T) {
	proxy := proxyToBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request with the wrong credential reached the registry")
	}))

	req, _ := http.NewRequest(http.MethodGet, "http://"+proxy.addr+"/v2/", nil)
	req.SetBasicAuth(pushProxyUser, "not-the-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requesting through the proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a wrong credential", resp.StatusCode)
	}
}

// Each build mints its own credential, so one leaked from a previous build is
// not a standing key to the next one.
func TestPushProxy_CredentialIsPerBuild(t *testing.T) {
	first, err := newPushProxy(stubPeerDialer{}, testDeliveryTarget(), 5000, nil)
	if err != nil {
		t.Fatalf("newPushProxy: %v", err)
	}
	defer first.ln.Close()
	second, err := newPushProxy(stubPeerDialer{}, testDeliveryTarget(), 5000, nil)
	if err != nil {
		t.Fatalf("newPushProxy: %v", err)
	}
	defer second.ln.Close()

	if first.credential == "" {
		t.Fatal("a proxy must mint a credential")
	}
	if first.credential == second.credential {
		t.Fatal("two builds must not share a push credential")
	}
}

// The authenticated path must reach the registry with the body and method
// intact — and without the loopback credential, which is ours and means nothing
// to the target (it authenticates us by client certificate).
func TestPushProxy_ForwardsAuthenticatedRequestWithoutTheCredential(t *testing.T) {
	var (
		gotAuth   string
		gotMethod string
		gotPath   string
		gotBody   string
	)
	proxy := proxyToBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	}))

	req, _ := http.NewRequest(http.MethodPut,
		"http://"+proxy.addr+"/v2/myapp/blobs/uploads/abc", strings.NewReader("layer-bytes"))
	req.SetBasicAuth(pushProxyUser, proxy.credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requesting through the proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 forwarded from the registry", resp.StatusCode)
	}
	if string(body) != "done" {
		t.Fatalf("response body = %q, want the registry's %q", body, "done")
	}
	if gotAuth != "" {
		t.Fatalf("the loopback credential was forwarded to the registry: %q", gotAuth)
	}
	if gotMethod != http.MethodPut || gotPath != "/v2/myapp/blobs/uploads/abc" {
		t.Fatalf("request reached the registry as %s %s, want PUT /v2/myapp/blobs/uploads/abc", gotMethod, gotPath)
	}
	if gotBody != "layer-bytes" {
		t.Fatalf("body reached the registry as %q, want %q", gotBody, "layer-bytes")
	}
}

// BuildKit retries a 502 by opening a new request from the descriptor content it
// still owns. An outbound failure from the first request must remain diagnostic
// history, not turn the successful final result into a delivery failure.
func TestPushProxy_RecoveredRequestRetryDoesNotBecomeFailure(t *testing.T) {
	var attempts int
	proxy := proxyToBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijacking failed backend connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))

	request := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut,
			"http://"+proxy.addr+"/v2/myapp/blobs/uploads/retry", strings.NewReader(body))
		req.SetBasicAuth(pushProxyUser, proxy.credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("requesting through proxy: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if status := request("first-attempt-body"); status != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502 to trigger BuildKit retry", status)
	}
	if status := request("first-attempt-body"); status != http.StatusCreated {
		t.Fatalf("retried status = %d, want 201", status)
	}
	if attempts != 2 {
		t.Fatalf("backend saw %d attempts, want 2", attempts)
	}
	if proxy.latestError() == nil {
		t.Fatal("test setup did not retain the first attempt's diagnostic")
	}
	buildErr, deliveryErr := classifyBuildAndDeliveryResult(nil, proxy.latestError())
	if buildErr != nil || deliveryErr != nil {
		t.Fatalf("recovered request classified as build=%v delivery=%v, want success", buildErr, deliveryErr)
	}
}

// When every request fails, the diagnostic must describe the last request only.
// Adding attempts together can exceed the layer size and is not progress.
func TestPushProxy_FailedRetriesReportLastRequestOnly(t *testing.T) {
	proxy := proxyToBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijacking failed backend connection: %v", err)
			return
		}
		_ = conn.Close()
	}))

	request := func(body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut,
			"http://"+proxy.addr+"/v2/myapp/blobs/uploads/retry", strings.NewReader(body))
		req.SetBasicAuth(pushProxyUser, proxy.credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("requesting through proxy: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
	}

	request(strings.Repeat("x", 64))
	request("last")

	got := proxy.latestError()
	if got == nil {
		t.Fatal("proxy did not retain the terminal request failure")
	}
	if strings.Contains(got.Error(), "64 B") || !strings.Contains(got.Error(), "4 B of a 4 B") {
		t.Fatalf("terminal diagnostic is not request-local: %v", got)
	}
}

// buildctl must be able to find the credential, and must find it in a file
// rather than anywhere argv-visible.
func TestDockerConfigWithPushAuth_WritesUsableCredential(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	if err := dockerConfigWithPushAuth(dir, "127.0.0.1:41000", "s3cret"); err != nil {
		t.Fatalf("dockerConfigWithPushAuth: %v", err)
	}

	path := filepath.Join(dir, "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config.json mode = %o, want 0600 — it holds a credential", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	var parsed struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("config.json is not valid docker config: %v", err)
	}
	entry, ok := parsed.Auths["127.0.0.1:41000"]
	if !ok {
		t.Fatalf("no auth entry for the proxy address, got %v", parsed.Auths)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		t.Fatalf("auth entry is not base64: %v", err)
	}
	if string(decoded) != pushProxyUser+":s3cret" {
		t.Fatalf("auth entry decodes to %q, want %q", decoded, pushProxyUser+":s3cret")
	}
}

func TestPushProxy_NoErrorWhenNothingConnects(t *testing.T) {
	proxy, err := startPushProxy(context.Background(),
		stubPeerDialer{addr: "127.0.0.1:1"}, testDeliveryTarget(), 5000, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("startPushProxy: %v", err)
	}
	defer proxy.stop()

	if got := proxy.latestError(); got != nil {
		t.Fatalf("a proxy nothing dialed must report no error, got: %v", got)
	}
}

// The hop the pinned certificate authorizes is the resolved device, not the
// asset number the CLI named: dialing anything else would let the image reach
// a device nobody resolved it to.
func TestPushProxy_DialsResolvedTarget(t *testing.T) {
	var dialed DeliveryTarget
	proxy, err := startPushProxy(context.Background(),
		stubPeerDialer{addr: "127.0.0.1:1", last: &dialed}, testDeliveryTarget(), 5000, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("startPushProxy: %v", err)
	}
	defer proxy.stop()

	req, _ := http.NewRequest(http.MethodHead, "http://"+proxy.addr+"/v2/", nil)
	req.SetBasicAuth(pushProxyUser, proxy.credential)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if dialed.AssetUUID != testAssetUUID {
		t.Fatalf("proxy dialled asset %q, want the resolved %q", dialed.AssetUUID, testAssetUUID)
	}
}

func TestValidatePushTarget_AcceptsMeshPeer(t *testing.T) {
	if err := validatePushTarget(&agentpbv2.PushTarget{
		AssetUuid: testAssetUUID, RegistryPort: 5000, Repository: "myapp:latest",
	}); err != nil {
		t.Fatalf("validatePushTarget: %v", err)
	}
}

func TestValidatePushTarget_RejectsMissingTarget(t *testing.T) {
	if err := validatePushTarget(nil); err == nil {
		t.Fatal("a spec with no push target must be rejected")
	}
}

// Only canonical Cloud asset UUID text names a device: a bare number, a
// non-canonical spelling, or nothing at all cannot be resolved against the
// roster and must fail before any build runs.
func TestValidatePushTarget_RejectsNonCanonicalAsset(t *testing.T) {
	for _, asset := range []string{
		"",
		"214",
		"not-a-uuid",
		"5b8f3a2e1c4d4e9a8b7c6d5e4f3a2b1c",
		"5B8F3A2E-1C4D-4E9A-8B7C-6D5E4F3A2B1C",
	} {
		if err := validatePushTarget(&agentpbv2.PushTarget{
			AssetUuid: asset, RegistryPort: 5000, Repository: "a:latest",
		}); err == nil {
			t.Fatalf("asset %q was accepted; it is not canonical UUID text", asset)
		}
	}
}

func TestValidatePushTarget_RejectsBadPort(t *testing.T) {
	if err := validatePushTarget(&agentpbv2.PushTarget{
		AssetUuid: testAssetUUID, RegistryPort: 0, Repository: "a:latest",
	}); err == nil {
		t.Fatal("a zero registry port must be rejected")
	}
}

// A slash would make the first element a registry host once joined to the proxy
// address, quietly redirecting the push somewhere else.
func TestValidatePushTarget_RejectsHostInRepository(t *testing.T) {
	if err := validatePushTarget(&agentpbv2.PushTarget{
		AssetUuid: testAssetUUID, RegistryPort: 5000, Repository: "evil.example.com/a:latest",
	}); err == nil {
		t.Fatal("a repository containing a host component must be rejected")
	}
}

// The repository is concatenated into `--output type=image,name=<ref>,push=true`,
// where a comma opens a new key and an '=' opens a new value. Rejecting only '/'
// and space leaves the rest of that grammar reachable by the client.
func TestValidatePushTarget_RejectsBuildctlOutputOptionInjection(t *testing.T) {
	for _, repo := range []string{
		"a:latest,push=false",                 // drop the push, leaving a silent no-op
		"a:latest,type=local,dest=/etc",       // swap the exporter for a filesystem write
		"a=b:latest",                          // end the name, start another value
		"a:latest\toci-mediatypes=true",       // tab is whitespace but not a space
		"a:latest\npush=false",                // newline likewise, and forges a log line
		"a:latest,registry.insecure=true",     // relax the transport for the hop
		"",                                    // no repository at all
		":latest",                             // empty name with a tag
		"a:latest,name=other.example.com/x:1", // rename the push destination outright
	} {
		if err := validatePushTarget(&agentpbv2.PushTarget{
			AssetUuid: testAssetUUID, RegistryPort: 5000, Repository: repo,
		}); err == nil {
			t.Fatalf("repository %q was accepted; it can rewrite the buildctl output spec", repo)
		}
	}
}

// The shapes the CLI actually sends — lowercased app ids, which carry dots,
// hyphens and underscores — must keep working.
func TestValidatePushTarget_AcceptsRealAppRepositories(t *testing.T) {
	for _, repo := range []string{
		"sh.wendy.examples.hellopython:latest",
		"my-app:latest",
		"my_app2:1.0.0-rc1",
		"app",
	} {
		if err := validatePushTarget(&agentpbv2.PushTarget{
			AssetUuid: testAssetUUID, RegistryPort: 5000, Repository: repo,
		}); err != nil {
			t.Fatalf("repository %q was rejected: %v", repo, err)
		}
	}
}
