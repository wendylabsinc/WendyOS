package viewerbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testMedia struct {
	data  []byte
	mime  string
	until int64
}

func mediaDigest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (f *grpcFixture) GetEvidence(_ context.Context, q *appv1.GetEvidenceRequest) (*appv1.GetEvidenceReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evidenceCalls++
	if q.Source == nil {
		return nil, status.Error(codes.PermissionDenied, "owner required")
	}
	source := EvidenceSource{Device: q.Source.Device, App: q.Source.App, Service: q.Source.Service, ComponentType: q.Source.ComponentType, Stream: q.Source.Stream}
	item, ok := f.evidence[source]
	if !ok || mediaDigest(item.data) != q.Sha256 || q.Offset < 0 || q.Offset >= int64(len(item.data)) || q.Length < 1 || q.Length > 48<<10 {
		return nil, status.Error(codes.PermissionDenied, "original media unavailable")
	}
	if item.until <= f.w.Latest.UnixMilli() {
		return nil, status.Error(codes.NotFound, "original media expired")
	}
	length := min(int64(q.Length), int64(len(item.data))-q.Offset)
	r := &appv1.GetEvidenceReply{Sha256: q.Sha256, Size: int64(len(item.data)), Offset: q.Offset, Data: bytes.Clone(item.data[q.Offset : q.Offset+length]), ContentType: item.mime, RetainUntilUnixMs: item.until}
	switch f.evidenceMode {
	case "wrong-size":
		r.Size++
	case "wrong-type":
		r.ContentType = "image/jpeg"
	case "wrong-digest":
		r.Sha256 = "bad"
	case "wrong-offset":
		r.Offset++
	case "short":
		r.Data = r.Data[:len(r.Data)-1]
	case "extra":
		r.Data = append(r.Data, 0)
	case "expired":
		r.RetainUntilUnixMs = f.w.Earliest.UnixMilli()
	case "invalid-retention":
		r.RetainUntilUnixMs = 253402300800000
	case "unknown-field":
		r.ProtoReflect().SetUnknown([]byte{0x78, 0x01})
	case "expiry-during-read":
		f.w.Earliest = time.UnixMilli(item.until)
		f.w.Latest = f.w.Earliest.Add(time.Millisecond)
		f.w.Monotonic += time.Minute
	case "wrong-generation":
		f.mode = "wrong-generation"
	}
	return r, nil
}

func generatedMedia(t *testing.T) map[string][]byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 160, 160))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var p, j bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	w := make([]byte, 44+96000)
	copy(w, "RIFF")
	binary.LittleEndian.PutUint32(w[4:], uint32(len(w)-8))
	copy(w[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(w[16:], 16)
	binary.LittleEndian.PutUint16(w[20:], 1)
	binary.LittleEndian.PutUint16(w[22:], 1)
	binary.LittleEndian.PutUint32(w[24:], 48000)
	binary.LittleEndian.PutUint32(w[28:], 96000)
	binary.LittleEndian.PutUint16(w[32:], 2)
	binary.LittleEndian.PutUint16(w[34:], 16)
	copy(w[36:], "data")
	binary.LittleEndian.PutUint32(w[40:], 96000)
	return map[string][]byte{"image/png": p.Bytes(), "image/jpeg": j.Bytes(), "audio/wav": w}
}
func mediaSources(f *grpcFixture) []EvidenceSource {
	var out []EvidenceSource
	for _, id := range []string{"camera-a", "people-front", "people-oblique", "camera-b", "audio-1"} {
		device := f.cfg.People.Source.Device
		if id == "camera-b" || id == "audio-1" {
			device = f.cfg.People.Device
		}
		out = append(out, EvidenceSource{Device: device, App: f.cfg.People.App, Service: id, ComponentType: sensorrecord.ComponentType, Stream: id})
	}
	return out
}

type evidenceHTTP struct {
	bridge   *HTTPBridge
	server   *httptest.Server
	f        *grpcFixture
	grant    authority.SignedGrant
	key      ed25519.PrivateKey
	audience string
	nonce    int
}

func startEvidenceHTTP(t *testing.T, f *grpcFixture, api appv1.WorldAppServiceClient, sources []EvidenceSource) *evidenceHTTP {
	t.Helper()
	pub, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rp, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := f.w.Earliest
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: f.cfg.People.Tenant, Site: f.cfg.People.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Hour - time.Second).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	c := HTTPConfig{Version: 1, Listen: "127.0.0.1:8094", AllowInsecureSimulation: true, Audience: "native-viewer", RootPublicKey: pub, Revocations: rev, Reader: f.cfg, EvidenceSources: sources}
	b, err := newHTTP(c, api, func() time.Time { w, _ := f.Now(); return w.Earliest })
	if err != nil {
		t.Fatal(err)
	}
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "viewer-media", Tenant: f.cfg.People.Tenant, Site: f.cfg.People.Site, Domain: "simulation", Principal: "viewer", Kind: "app", Source: world.Source{Device: "operator", App: "viewer", Service: "http", Stream: "read"}, PublicKey: rp, Scopes: []string{"read", "evidence-read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	h := &evidenceHTTP{bridge: b, server: httptest.NewServer(b.Handler), f: f, grant: grant, key: key, audience: c.Audience}
	t.Cleanup(func() { h.server.Close(); b.Close() })
	return h
}
func (h *evidenceHTTP) request(t *testing.T, method, path, operation string, body []byte) (int, []byte) {
	t.Helper()
	h.nonce++
	w, _ := h.f.Now()
	signed, err := authority.SignRequest(h.grant, h.key, h.audience, operation, body, fmt.Sprintf("native_media_nonce_%09d", h.nonce), w.Earliest, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(method, h.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(node.AuthorizationHeader, base64.StdEncoding.EncodeToString(auth))
	response, err := h.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}
func (h *evidenceHTTP) get(t *testing.T, q node.NativeEvidenceRequest) (int, node.NativeEvidenceResponse) {
	t.Helper()
	body, _ := json.Marshal(q)
	code, raw := h.request(t, "POST", node.NativeEvidencePath, "evidence-read", body)
	var got node.NativeEvidenceResponse
	if code == 200 && json.Unmarshal(raw, &got) != nil {
		t.Fatal("invalid response", string(raw))
	}
	return code, got
}

func TestNativeEvidenceActualSignedHTTPAndGRPCAllOwners(t *testing.T) {
	f, api := setup(t)
	sources := mediaSources(f)
	data := generatedMedia(t)
	f.evidence = map[EvidenceSource]testMedia{}
	for i, s := range sources {
		mime := "image/png"
		if i == 2 {
			mime = "image/jpeg"
		}
		if i == 4 {
			mime = "audio/wav"
		}
		f.evidence[s] = testMedia{data[mime], mime, f.w.Earliest.Add(time.Duration(30+i) * time.Second).UnixMilli()}
	}
	h := startEvidenceHTTP(t, f, api, sources)
	for _, s := range sources {
		item := f.evidence[s]
		var assembled []byte
		var until int64
		for len(assembled) < len(item.data) {
			q := node.NativeEvidenceRequest{Source: s, SHA256: mediaDigest(item.data), Size: int64(len(item.data)), ContentType: item.mime, Offset: int64(len(assembled)), Length: 48 << 10}
			code, r := h.get(t, q)
			if code != 200 || r.ValidateFor(q) != nil {
				t.Fatal("range failed", s, code, r)
			}
			if until != 0 && until != r.RetainUntilUnixMS {
				t.Fatal("retention changed")
			}
			until = r.RetainUntilUnixMS
			assembled = append(assembled, r.Data...)
		}
		if !bytes.Equal(assembled, item.data) || mediaDigest(assembled) != mediaDigest(item.data) || until != item.until {
			t.Fatal("original bytes/retention changed")
		}
	}
	if f.queries != 0 || f.sampleClocks != 0 {
		t.Fatal("historical evidence depended on result or simulation reads")
	}
	// Camera A and Camera B have identical PNG bytes but distinct source devices
	// and retention deadlines.
	if f.evidence[sources[0]].until == f.evidence[sources[3]].until {
		t.Fatal("test failed to distinguish owners")
	}
	q := node.NativeEvidenceRequest{Source: sources[0], SHA256: mediaDigest(data["image/png"]), Size: int64(len(data["image/png"])), ContentType: "image/png", Length: 20}
	before := f.evidenceCalls
	q.Source.Service = "ungranted"
	if code, _ := h.get(t, q); code != 503 || f.evidenceCalls != before {
		t.Fatal("unconfigured owner reached native API")
	}
}

func TestNativeEvidenceOutlivesResultAndExpiresAtOriginalRetention(t *testing.T) {
	f, api := setup(t)
	sources := mediaSources(f)[:1]
	data := generatedMedia(t)["image/png"]
	until := f.w.Earliest.Add(30 * time.Second).UnixMilli()
	f.evidence = map[EvidenceSource]testMedia{sources[0]: {data, "image/png", until}}
	h := startEvidenceHTTP(t, f, api, sources)
	code, raw := h.request(t, "GET", "/v1/components", "read", nil)
	var snapshot client.Snapshot
	if code != 200 || json.Unmarshal(raw, &snapshot) != nil || len(snapshot.Components) != 3 {
		t.Fatal("initial result missing", code, string(raw))
	}
	f.mu.Lock()
	f.now = f.now.Add(5 * time.Second)
	f.mu.Unlock()
	code, raw = h.request(t, "GET", "/v1/components", "read", nil)
	if code != 200 || json.Unmarshal(raw, &snapshot) != nil || len(snapshot.Components) != 0 {
		t.Fatal("original results did not expire", code, string(raw))
	}
	q := node.NativeEvidenceRequest{Source: sources[0], SHA256: mediaDigest(data), Size: int64(len(data)), ContentType: "image/png", Length: 30}
	queries := f.queries
	if code, r := h.get(t, q); code != 200 || r.RetainUntilUnixMS != until || f.queries != queries {
		t.Fatal("retained media tied to result expiry", code)
	}
	f.mu.Lock()
	f.w.Earliest = time.UnixMilli(until)
	f.w.Latest = f.w.Earliest.Add(time.Millisecond)
	f.w.Monotonic += 30 * time.Second
	f.mu.Unlock()
	if code, _ := h.get(t, q); code != 503 {
		t.Fatal("expired media disclosed", code)
	}
	// A later native incarnation may use the same bytes and owner. No old
	// deadline map can permanently reject it or silently keep the old deadline.
	newUntil := until + 30000
	f.mu.Lock()
	f.evidence[sources[0]] = testMedia{data, "image/png", newUntil}
	f.mu.Unlock()
	if code, r := h.get(t, q); code != 200 || r.RetainUntilUnixMS != newUntil {
		t.Fatal("new retained incarnation unavailable", code, r.RetainUntilUnixMS)
	}
}

func TestNativeEvidenceRejectsMalformedNativeRangesAndFinalExpiry(t *testing.T) {
	for _, mode := range []string{"wrong-size", "wrong-type", "wrong-digest", "wrong-offset", "short", "extra", "expired", "invalid-retention", "unknown-field", "expiry-during-read", "wrong-generation"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t)
			sources := mediaSources(f)[:1]
			data := generatedMedia(t)["image/png"]
			f.evidence = map[EvidenceSource]testMedia{sources[0]: {data, "image/png", f.w.Earliest.Add(30 * time.Second).UnixMilli()}}
			f.evidenceMode = mode
			h := startEvidenceHTTP(t, f, api, sources)
			q := node.NativeEvidenceRequest{Source: sources[0], SHA256: mediaDigest(data), Size: int64(len(data)), ContentType: "image/png", Length: 40}
			if code, r := h.get(t, q); code != 503 || len(r.Data) != 0 {
				t.Fatal("malformed/expired native reply disclosed", code, r)
			}
		})
	}
}

func TestNativeEvidenceConfigurationAndQueryOnlyCompatibility(t *testing.T) {
	f, api := setup(t)
	sources := mediaSources(f)
	valid, err := normalizedEvidenceSources(sources, f.cfg.People.Device)
	if err != nil || !reflect.DeepEqual(valid, sources) {
		t.Fatal(err)
	}
	sources[0].Service = "changed"
	if valid[0].Service == "changed" {
		t.Fatal("source config aliases caller")
	}
	for _, bad := range [][]EvidenceSource{{valid[0], valid[0]}, {{Device: "", App: valid[0].App, Service: "camera-a", ComponentType: sensorrecord.ComponentType, Stream: "camera-a"}}, {{Device: valid[0].Device, App: valid[0].App, Service: "camera-a", ComponentType: "wendy.inference.result.v1", Stream: "camera-a"}}} {
		if _, err := normalizedEvidenceSources(bad, f.cfg.People.Device); err == nil {
			t.Fatal("invalid exact source configuration accepted")
		}
	}
	h := startEvidenceHTTP(t, f, api, nil)
	if code, _ := h.request(t, "POST", node.NativeEvidencePath, "evidence-read", []byte(`{}`)); code != 404 || f.evidenceCalls != 0 {
		t.Fatal("query-only route changed", code)
	}
}
