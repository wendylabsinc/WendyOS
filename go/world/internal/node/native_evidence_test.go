package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeEvidenceSignedRangeAndCallbackContract(t *testing.T) {
	f := newFixture(t, nil, "native-media")
	credentials := evidenceCredentials(t, f, "viewer", "", "read", "evidence-read")
	q := NativeEvidenceRequest{Source: NativeEvidenceSource{Device: "original-device", App: "sensors", Service: "camera", ComponentType: "capture", Stream: "camera-a"}, SHA256: strings.Repeat("a", 64), Size: 5, ContentType: "image/png", Offset: 3, Length: 48 << 10}
	reads := 0
	wrongReply := false
	h, err := NewNativeEvidenceReadHandler(ComponentReadConfig{Tenant: "tenant", Site: "office", Audience: f.audience}, f.v, func(_ context.Context, got NativeEvidenceRequest) (NativeEvidenceResponse, error) {
		reads++
		if got != q {
			t.Fatal("signed original request changed")
		}
		r := NativeEvidenceResponse{Source: got.Source, SHA256: got.SHA256, Size: got.Size, ContentType: got.ContentType, Offset: got.Offset, Data: []byte{1, 2}, RetainUntilUnixMS: time.Now().Add(time.Hour).UnixMilli()}
		if wrongReply {
			r.Source.Device = "another-device"
		}
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(body []byte, op string, signedBody []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", NativeEvidencePath, bytes.NewReader(body))
		if err := AuthorizeRequest(r, credentials, f.audience, op, signedBody); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body, _ := json.Marshal(q)
	w := call(body, "evidence-read", body)
	var got NativeEvidenceResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ValidateFor(q) != nil || !bytes.Equal(got.Data, []byte{1, 2}) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cache header missing")
	}
	if w = call(body, "read", body); w.Code != 401 {
		t.Fatal("read scope disclosed media", w.Code)
	}
	if w = call(body, "evidence-read", []byte("changed")); w.Code != 401 {
		t.Fatal("changed signed range accepted", w.Code)
	}
	invalid := append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"owner":"other"}`)...)
	if w = call(invalid, "evidence-read", invalid); w.Code != 400 {
		t.Fatal("unknown field accepted", w.Code)
	}
	if reads != 1 {
		t.Fatal("unaccepted request reached callback")
	}
	wrongReply = true
	if w = call(body, "evidence-read", body); w.Code != 503 || bytes.Contains(w.Body.Bytes(), []byte(`"data"`)) {
		t.Fatal("unbound reply disclosed", w.Code)
	}
	for _, route := range []struct {
		method, path string
		code         int
	}{{"GET", NativeEvidencePath, 405}, {"POST", NativeEvidencePath + "?x=1", 404}, {"POST", "/v1/evidence/get", 404}} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		if w.Code != route.code {
			t.Fatal(route, w.Code)
		}
	}
}

func TestNativeEvidenceBoundsAndCancellation(t *testing.T) {
	q := NativeEvidenceRequest{Source: NativeEvidenceSource{Device: "d", App: "a", Service: "s", ComponentType: "c", Stream: "x"}, SHA256: strings.Repeat("1", 64), Size: 8 << 20, ContentType: "audio/wav", Length: 48 << 10}
	for _, change := range []func(*NativeEvidenceRequest){func(q *NativeEvidenceRequest) { q.Offset = q.Size }, func(q *NativeEvidenceRequest) { q.Offset = -1 }, func(q *NativeEvidenceRequest) { q.Length++ }, func(q *NativeEvidenceRequest) { q.Length = 0 }, func(q *NativeEvidenceRequest) { q.Size++ }, func(q *NativeEvidenceRequest) { q.Source.Device = "" }, func(q *NativeEvidenceRequest) { q.ContentType = "text/plain" }, func(q *NativeEvidenceRequest) { q.SHA256 = strings.Repeat("A", 64) }} {
		bad := q
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid range accepted", bad)
		}
	}
	f := newFixture(t, nil, "native-media")
	c := evidenceCredentials(t, f, "reader", "", "evidence-read")
	h, err := NewNativeEvidenceReadHandler(ComponentReadConfig{Tenant: "tenant", Site: "office", Audience: f.audience, RequestTimeout: time.Millisecond}, f.v, func(ctx context.Context, _ NativeEvidenceRequest) (NativeEvidenceResponse, error) {
		<-ctx.Done()
		return NativeEvidenceResponse{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(q)
	r := httptest.NewRequest(http.MethodPost, NativeEvidencePath, bytes.NewReader(body))
	if err := AuthorizeRequest(r, c, f.audience, "evidence-read", body); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("cancelled read returned media", w.Code)
	}
	if _, err := NewNativeEvidenceReadHandler(ComponentReadConfig{Tenant: "other", Site: "office", Audience: f.audience}, f.v, func(context.Context, NativeEvidenceRequest) (NativeEvidenceResponse, error) {
		return NativeEvidenceResponse{}, nil
	}); err == nil {
		t.Fatal("wrong verifier scope accepted")
	}
}
