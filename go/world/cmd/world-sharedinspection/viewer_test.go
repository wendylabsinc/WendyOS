package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func viewerVerifier(t *testing.T, public viewerPublic, now func() time.Time) *authority.Verifier {
	t.Helper()
	replay := authority.NewMemoryReplay(100)
	t.Cleanup(func() { _ = replay.Close() })
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: public.RootPublicKey, Tenant: public.Tenant,
		Site: public.Site, Domain: "simulation", Audience: public.Audience, Now: now, Replay: replay})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(public.Revocations); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestViewerOptionalOutputsKeepMissionRolesSeparate(t *testing.T) {
	in, office, now := inputFixture(t)
	baseline, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	if baseline["viewer-public.json"] != nil || baseline["viewer-profile.json"] != nil {
		t.Fatal("viewer enabled by default")
	}
	in.ViewerURL = "http://127.0.0.1:38194"
	files, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(baseline)+2 {
		t.Fatal("unrelated output added")
	}
	for name := range baseline {
		if files[name] == nil {
			t.Fatal("legacy output missing", name)
		}
	}
	public := files["viewer-public.json"].(viewerPublic)
	profile := files["viewer-profile.json"].(viewerProfile)
	if public.Version != 1 || public.Tenant != in.Tenant || public.Site != in.Site || public.Audience != viewerAudience ||
		profile.Name == "" || profile.ReadMode != "native_inference" || profile.Credentials.URL != in.ViewerURL ||
		profile.Credentials.Audience != viewerAudience || !profile.Credentials.AllowInsecureSimulation {
		t.Fatal("viewer contract differs")
	}
	viewer := profile.Credentials
	if viewer.Grant.Grant.Source.Device != in.Device || viewer.Grant.Grant.Kind != "operator" ||
		viewer.Grant.Grant.Principal != "sharedinspection/viewer-reader" ||
		!reflect.DeepEqual(viewer.Grant.Grant.Scopes, []string{"read", "evidence-read"}) || viewer.Grant.Grant.PublishAudience != "" {
		t.Fatal("viewer has unexpected identity or grants")
	}
	if viewer.Grant.Grant.NotBeforeUnixMS != now.Add(-time.Second).UnixMilli() ||
		viewer.Grant.Grant.NotAfterUnixMS != now.Add(59*time.Minute).UnixMilli() {
		t.Fatal("viewer issuance changed")
	}
	gateway := files["gateway.json"].(map[string]any)
	missionRoot := gateway["root_public_key"].(ed25519.PublicKey)
	if reflect.DeepEqual(public.RootPublicKey, missionRoot) {
		t.Fatal("viewer reused mission root")
	}
	v := viewerVerifier(t, public, func() time.Time { return now })
	if _, err = v.VerifyGrant(viewer.Grant, "read"); err != nil {
		t.Fatal(err)
	}
	if _, err = v.VerifyGrant(viewer.Grant, "evidence-read"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"mission", "stop", "execute", "propose", "publish", "replicate", "annotation-write", "evidence-write", "evidence-pin"} {
		if _, err = v.VerifyGrant(viewer.Grant, op); err == nil {
			t.Fatal("viewer gained capability", op)
		}
	}
	missionPublic := viewerPublic{Tenant: in.Tenant, Site: in.Site, Audience: audience, RootPublicKey: missionRoot,
		Revocations: gateway["revocations"].(authority.SignedRevocations)}
	missionVerifier := viewerVerifier(t, missionPublic, func() time.Time { return now })
	if _, err = missionVerifier.VerifyGrant(viewer.Grant, "read"); err == nil {
		t.Fatal("viewer key retargeted to mission root")
	}
	for _, name := range []string{"operator", "evidence-reader", "executor", "proposer"} {
		c := files[name+".json"].(client.Config)
		if c.URL != in.GatewayURL || c.Audience != audience || reflect.DeepEqual(c.PrivateKey, viewer.PrivateKey) {
			t.Fatal("legacy credential changed", name)
		}
		if _, err = missionVerifier.VerifyGrant(c.Grant, "read"); err != nil {
			t.Fatal("legacy read changed", name, err)
		}
		if _, err = v.VerifyGrant(c.Grant, "read"); err == nil {
			t.Fatal("mission credential accepted by viewer root", name)
		}
	}
	if !reflect.DeepEqual(files["operator.json"].(client.Config).Grant.Grant.Scopes, []string{"mission", "stop", "read"}) ||
		!reflect.DeepEqual(files["evidence-reader.json"].(client.Config).Grant.Grant.Scopes, []string{"read", "evidence-read"}) {
		t.Fatal("existing role scopes changed")
	}
}

func TestViewerProfileUsesExistingAuthorizedComponentRead(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	in, office, _ := inputFixture(t)
	in.ViewerURL = "http://" + server.Listener.Addr().String()
	files, err := commission(in, office, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	public := files["viewer-public.json"].(viewerPublic)
	profile := files["viewer-profile.json"].(viewerProfile)
	verifier := viewerVerifier(t, public, time.Now)
	reads := 0
	handler, err := node.NewComponentReadHandler(node.ComponentReadConfig{Tenant: in.Tenant, Site: in.Site, Audience: viewerAudience, MaxRecords: 16}, verifier,
		func(context.Context) ([]world.Component, error) { reads++; return []world.Component{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	api, err := client.New(profile.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, err := api.Query(ctx)
	if err != nil || snapshot.Components == nil || len(snapshot.Components) != 0 || reads != 1 || len(snapshot.Cursor) != 64 {
		t.Fatal("authorized query failed", err)
	}
	wrong := profile.Credentials
	wrong.Audience = audience
	other, err := client.New(wrong)
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Query(ctx)
	var status *client.HTTPStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized || reads != 1 {
		t.Fatal("wrong audience read accepted", err)
	}
	if err = api.Do(ctx, "read", http.MethodGet, "/v1/sites", nil, nil); !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
		t.Fatal("sites fabricated", err)
	}
	if err = api.Do(ctx, "publish", http.MethodGet, "/v1/components", nil, nil); !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized || reads != 1 {
		t.Fatal("mutation capability accepted", err)
	}
}

func TestViewerOutputIsBoundedPrivateAndExclusive(t *testing.T) {
	in, office, now := inputFixture(t)
	in.ViewerURL = "http://127.0.0.1:38194"
	files, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, name := range []string{"viewer-public.json", "viewer-profile.json"} {
		path := filepath.Join(out, name)
		if err = writeNew(path, files[name]); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0600 || len(raw) > 64<<10 {
			t.Fatal("output bounds/permissions changed", err)
		}
		if err = writeNew(path, files[name]); err == nil {
			t.Fatal("viewer output overwritten")
		}
		if name == "viewer-public.json" {
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) != nil {
				t.Fatal("public json invalid")
			}
			want := []string{"version", "tenant", "site", "audience", "root_public_key", "revocations"}
			if len(obj) != len(want) || strings.Contains(string(raw), "private_key") {
				t.Fatal("private data entered public config")
			}
			for _, key := range want {
				if obj[key] == nil {
					t.Fatal("public field missing", key)
				}
			}
		} else {
			var profile viewerProfile
			if err = readJSON(path, &profile, 64<<10, true); err != nil {
				t.Fatal(err)
			}
			if _, err = client.New(profile.Credentials); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestViewerRequiresExplicitHTTPOrigin(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:8094", "http://localhost", "http://user@localhost:8094", "http://localhost:8094/path", "http://localhost:8094?token=x", "http://localhost:8094#fragment"} {
		in, office, now := inputFixture(t)
		in.ViewerURL = endpoint
		if _, err := commission(in, office, now); err == nil {
			t.Fatal("invalid viewer endpoint accepted", endpoint)
		}
	}
}
