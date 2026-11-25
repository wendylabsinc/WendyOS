package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestComponentReaderOriginalsAndStableCursorWithoutReplicaWrites(t *testing.T) {
	f := newFixture(t, nil, "native-reader")
	c := component(f, 3, 9)
	c.Timing.SampleTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := c
	d.Key.Entity = "other"
	reads := 0
	read := func(context.Context) ([]world.Component, error) {
		reads++
		if reads%2 == 1 {
			return []world.Component{d, c}, nil
		}
		return []world.Component{c, d}, nil
	}
	h, err := NewComponentReadHandler(ComponentReadConfig{Tenant: "tenant", Site: "office", Audience: f.audience, MaxRecords: 16}, f.v, read)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func() QueryResponse {
		r := httptest.NewRequest(http.MethodGet, "/v1/components", nil)
		if err := AuthorizeRequest(r, f.credentials, f.audience, "read", nil); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var got QueryResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b := invoke(), invoke()
	if a.Cursor != b.Cursor || !reflect.DeepEqual(a.Components, b.Components) || len(a.Components) != 2 {
		t.Fatal("unstable projection", a, b)
	}
	want, _ := json.Marshal(c)
	got, _ := json.Marshal(a.Components[0])
	if !bytes.Equal(got, want) {
		t.Fatal("original component changed", string(got))
	}
	records, err := f.s.Records()
	if err != nil || len(records) != 0 {
		t.Fatal("read bridge wrote replica state", err)
	}
}

func TestComponentReaderHasOnlyAuthenticatedReadRoute(t *testing.T) {
	f := newFixture(t, nil, "native-reader")
	reads := 0
	h, err := NewComponentReadHandler(ComponentReadConfig{Tenant: "tenant", Site: "office", Audience: f.audience, MaxRecords: 16}, f.v,
		func(context.Context) ([]world.Component, error) { reads++; return []world.Component{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/v1/components", 401}, {"POST", "/v1/components", 405},
		{"GET", "/v1/sites", 404}, {"GET", "/v1/task-relay", 404}, {"GET", "/v1/watch", 404},
		{"POST", "/v1/tasks", 404}, {"POST", "/v1/evidence/get", 404}, {"GET", "/v1/components?scope=other", 404},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.code {
			t.Fatalf("%s %s: %d", test.method, test.path, w.Code)
		}
	}
	if reads != 0 {
		t.Fatal("unaccepted request read native data")
	}
	bad := ComponentReadConfig{Tenant: "other", Site: "office", Audience: f.audience, MaxRecords: 16}
	if _, err := NewComponentReadHandler(bad, f.v, func(context.Context) ([]world.Component, error) { return nil, nil }); err == nil {
		t.Fatal("mismatched verifier commissioning accepted")
	}
}

func TestComponentReaderFailureAndCancellationNeverReusePreviousResult(t *testing.T) {
	f := newFixture(t, nil, "native-reader")
	reads := 0
	h, err := NewComponentReadHandler(ComponentReadConfig{Tenant: "tenant", Site: "office", Audience: f.audience, MaxRecords: 16, RequestTimeout: 10 * time.Millisecond}, f.v,
		func(ctx context.Context) ([]world.Component, error) {
			reads++
			switch reads {
			case 1:
				return []world.Component{component(f, 1, 1)}, nil
			case 2:
				return nil, errors.New("native source unavailable")
			case 3:
				return nil, nil
			default:
				<-ctx.Done()
				return nil, ctx.Err()
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{200, 503, 503, 503} {
		r := httptest.NewRequest(http.MethodGet, "/v1/components", nil)
		if err := AuthorizeRequest(r, f.credentials, f.audience, "read", nil); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(w.Code, want, w.Body.String())
		}
		if want == 503 && bytes.Contains(w.Body.Bytes(), []byte(`"components"`)) {
			t.Fatal("returned prior components")
		}
	}
}

func TestComponentReaderScopeBoundsAndDuplicateKeys(t *testing.T) {
	f := newFixture(t, nil, "native-reader")
	cfg := ComponentReadConfig{Tenant: "tenant", Site: "office", Audience: f.audience, MaxRecords: 1}
	c := component(f, 1, 1)
	for _, mutate := range []func(*world.Component){
		func(c *world.Component) { c.Key.Site = "another" },
		func(c *world.Component) { c.Tombstone = true },
		func(c *world.Component) { c.Version.Sequence = 0 },
		func(c *world.Component) { c.Timing.ClockDomain = "utc" },
		func(c *world.Component) { c.Payload = json.RawMessage(`invalid`) },
	} {
		d := c
		mutate(&d)
		if _, err := componentReadResponse(cfg, []world.Component{d}); err == nil {
			t.Fatal("invalid projection accepted")
		}
	}
	if _, err := componentReadResponse(cfg, []world.Component{c, c}); err == nil {
		t.Fatal("overbound read accepted")
	}
	cfg.MaxRecords = 2
	if _, err := componentReadResponse(cfg, []world.Component{c, c}); err == nil {
		t.Fatal("duplicate key accepted")
	}
	empty, err := componentReadResponse(cfg, []world.Component{})
	if err != nil || empty.Components == nil || len(empty.Cursor) != 64 {
		t.Fatal("explicit empty read refused", err)
	}
}
