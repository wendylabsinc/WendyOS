package registry

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	"go.uber.org/zap"
)

type registryLab struct {
	fixture                    *deliverypki.Fixture
	manager                    *delivery.Manager
	server                     *httptest.Server
	images                     *testImageStore
	registry                   containerdRegistry
	operator, builder, rotated *http.Client
}

func newRegistryLab(t *testing.T) *registryLab {
	t.Helper()
	f := deliverypki.New(t)
	manager := f.Manager(t)
	images := newTestImageStore()
	cs, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := containerd.New("", containerd.WithDefaultNamespace("default"), containerd.WithServices(containerd.WithImageStore(images), containerd.WithContentStore(cs), containerd.WithLeasesService(&testLeasesManager{})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	reg := containerdRegistry{client: client, imagePrefix: "localhost:5000/", blobLeaseExpiration: time.Minute, manifestSizeLimit: maxManifestBody, deliverySource: manager.Get}
	handler := authorizationMiddleware(manager.Get, interceptor.OrgModeStrict)(newOCIHandler(newAuthorizedRegistry(safeDeleteRegistry{Interface: reg})))
	server := httptest.NewUnstartedServer(handler)
	server.TLS, err = mtls.NewTLSConfig(f.Target.PEM, f.Chain, f.Target.Key, zap.NewNop(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	mkClient := func(c deliverypki.Credential) *http.Client {
		transport := &http.Transport{TLSClientConfig: f.ClientTLS(t, c)}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 12 * time.Second}
	}
	return &registryLab{f, manager, server, images, reg, mkClient(f.Operator), mkClient(f.Builder), mkClient(f.OtherBuilder)}
}
func (l *registryLab) request(t *testing.T, client *http.Client, method, path, session string, data []byte) *http.Response {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(l.server.URL)
	u = base.ResolveReference(u)
	req, err := http.NewRequest(method, u.String(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	if session != "" {
		req.Header.Set(builddelivery.HTTPHeader, session)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
func wantHTTP(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d, want %d: %s", resp.StatusCode, want, body)
	}
}
func (l *registryLab) admit(t *testing.T, reference string) string {
	t.Helper()
	return l.fixture.Admit(t, l.manager, l.fixture.Claims(reference))
}

func uploadID(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	encoded := u.Path[strings.LastIndexByte(u.Path, '/')+1:]
	id, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(id)
}

func TestRegistryOperatorAndDelegatedPublicationOverRealTLS(t *testing.T) {
	l := newRegistryLab(t)
	session := l.admit(t, "lab/app:latest")
	manifest := makeManifest("sha256:" + strings.Repeat("a", 64))
	path := "/v2/lab/app/manifests/latest"
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, path, "", manifest), http.StatusForbidden)
	wantHTTP(t, l.request(t, l.rotated, http.MethodPut, path, session, manifest), http.StatusForbidden)
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, "/v2/other/manifests/latest", session, manifest), http.StatusForbidden)
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, "/v2/lab/app/manifests/another", session, manifest), http.StatusForbidden)
	if _, err := l.images.Get(context.Background(), "localhost:5000/lab/app:latest"); !errdefs.IsNotFound(err) {
		t.Fatal("unauthorized request changed image index")
	}
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, path, session, manifest), http.StatusCreated)
	image, err := l.images.Get(context.Background(), "localhost:5000/lab/app:latest")
	if err != nil || image.Target.Digest != digest.FromBytes(manifest) {
		t.Fatalf("publication missing: %v", err)
	}
	// Exact completion replay is read-only and succeeds even if its byte budget
	// was fully reserved. A different result may not enter the content store.
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, path, session, manifest), http.StatusCreated)
	other := makeManifest("sha256:" + strings.Repeat("b", 64))
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, path, session, other), http.StatusForbidden)
	if _, err := l.registry.client.ContentStore().Info(context.Background(), digest.FromBytes(other)); !errdefs.IsNotFound(err) {
		t.Fatal("completed grant staged another manifest")
	}
	wantHTTP(t, l.request(t, l.operator, http.MethodPut, path, "", other), http.StatusCreated)
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, path, session, manifest), http.StatusForbidden)
	current, _ := l.images.Get(context.Background(), "localhost:5000/lab/app:latest")
	if current.Target.Digest != digest.FromBytes(other) {
		t.Fatal("replay overwrote later operator publication")
	}
}

func TestRegistryUploadOwnershipAndLeastPrivilege(t *testing.T) {
	l := newRegistryLab(t)
	first := l.admit(t, "lab/app:latest")
	second := l.admit(t, "lab/app:latest")
	wantHTTP(t, l.request(t, l.builder, http.MethodGet, "/v2/", "", nil), http.StatusOK)
	start := l.request(t, l.builder, http.MethodPost, "/v2/lab/app/blobs/uploads/", first, nil)
	wantHTTP(t, start, http.StatusAccepted)
	location := start.Header.Get("Location")
	if !strings.HasPrefix(uploadID(t, location), "delivery-"+first+"-") {
		t.Fatalf("upload missing durable owner binding: %s", location)
	}
	wantHTTP(t, l.request(t, l.builder, http.MethodPatch, location, second, []byte("wrong job")), http.StatusForbidden)
	wantHTTP(t, l.request(t, l.rotated, http.MethodPatch, location, first, []byte("wrong key")), http.StatusForbidden)
	wantHTTP(t, l.request(t, l.builder, http.MethodPatch, strings.Replace(location, "lab/app", "other", 1), first, []byte("wrong repo")), http.StatusForbidden)
	guessed := location[:strings.LastIndexByte(location, '/')+1] + base64.RawURLEncoding.EncodeToString([]byte("delivery-"+first+"-"+uuid.NewString()))
	wantHTTP(t, l.request(t, l.builder, http.MethodPatch, guessed, first, []byte("unissued upload")), http.StatusForbidden)
	payload := []byte("authorized registry upload")
	u, _ := url.Parse(location)
	query := u.Query()
	query.Set("digest", digest.FromBytes(payload).String())
	u.RawQuery = query.Encode()
	wantHTTP(t, l.request(t, l.builder, http.MethodPut, u.String(), first, payload), http.StatusCreated)
	blob := "/v2/lab/app/blobs/" + digest.FromBytes(payload).String()
	wantHTTP(t, l.request(t, l.builder, http.MethodHead, blob, first, nil), http.StatusOK)
	for _, path := range []string{blob, "/v2/lab/app/tags/list", "/v2/_catalog", "/v2/lab/app/referrers/" + digest.FromBytes(payload).String()} {
		t.Run(path, func(t *testing.T) {
			wantHTTP(t, l.request(t, l.builder, http.MethodGet, path, first, nil), http.StatusForbidden)
		})
	}
	wantHTTP(t, l.request(t, l.builder, http.MethodHead, strings.Replace(blob, "lab/app", "other", 1), first, nil), http.StatusForbidden)
	for _, method := range []string{http.MethodDelete, http.MethodPut} {
		response := l.request(t, l.builder, method, "/v2/lab/app/manifests/other", first, nil)
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			t.Fatal("ungranted registry mutation accepted")
		}
	}
	// Cross-repository mounts are an ordinary upload fallback, never a read
	// authority on the source repository or an unrestricted CAS link.
	mount := l.request(t, l.builder, http.MethodPost, "/v2/lab/app/blobs/uploads/?mount="+digest.FromBytes(payload).String()+"&from=private/source", first, nil)
	wantHTTP(t, mount, http.StatusAccepted)
	if !strings.HasPrefix(uploadID(t, mount.Header.Get("Location")), "delivery-"+first+"-") {
		t.Fatal("mount fallback escaped upload ownership")
	}
}

func TestRegistryCASAndBodyBounds(t *testing.T) {
	l := newRegistryLab(t)
	manifest := makeManifest("sha256:" + strings.Repeat("c", 64))
	path := "/v2/lab/app/manifests/latest"
	session := l.admit(t, "lab/app:latest")
	wantHTTP(t, l.request(t, l.operator, http.MethodPut, path, "", manifest), http.StatusCreated)
	newer := makeManifest("sha256:" + strings.Repeat("d", 64))
	response := l.request(t, l.builder, http.MethodPut, path, session, newer)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		t.Fatal("stale grant overwrote an existing image")
	}
	current, _ := l.images.Get(context.Background(), "localhost:5000/lab/app:latest")
	if current.Target.Digest != digest.FromBytes(manifest) {
		t.Fatal("CAS changed reference")
	}
	limited := l.fixture.Claims("budget:latest")
	limited.MaxBytes = 4
	small := l.fixture.Admit(t, l.manager, limited)
	wantHTTP(t, l.request(t, l.builder, http.MethodPost, "/v2/budget/blobs/uploads/", small, []byte("too many bytes")), http.StatusForbidden)
	response = l.request(t, l.operator, http.MethodPut, "/v2/big/manifests/latest", "", bytes.Repeat([]byte("x"), maxManifestBody+1))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		t.Fatal("oversized manifest accepted")
	}
}

func TestRegistryGrantExpiryClosesIdleUpload(t *testing.T) {
	l := newRegistryLab(t)
	c := l.fixture.Claims("idle:latest")
	c.AdmitBefore = c.IssuedAt + 3
	c.ValidUntil = c.IssuedAt + 5
	session := l.fixture.Admit(t, l.manager, c)
	start := l.request(t, l.builder, http.MethodPost, "/v2/idle/blobs/uploads/", session, nil)
	wantHTTP(t, start, http.StatusAccepted)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", strings.TrimPrefix(l.server.URL, "https://"), l.fixture.ClientTLS(t, l.fixture.Builder))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(9 * time.Second))
	_, err = fmt.Fprintf(conn, "PATCH %s HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\n%s: %s\r\n\r\n", start.Header.Get("Location"), builddelivery.HTTPHeader, session)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPatch})
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				err = fmt.Errorf("expired idle upload succeeded")
			}
		} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
			// Closing the expired connection without a response is valid.
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(9 * time.Second):
		t.Fatal("expired upload retained blocked handler")
	}
	wantHTTP(t, l.request(t, l.builder, http.MethodHead, "/v2/idle/blobs/sha256:"+strings.Repeat("a", 64), session, nil), http.StatusForbidden)
}
