package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveAppStoreAPIBase(t *testing.T) {
	t.Setenv("WENDY_APPSTORE_API", "")
	if got := resolveAppStoreAPIBase("https://example.com/"); got != "https://example.com" {
		t.Errorf("flag precedence: got %q", got)
	}
	t.Setenv("WENDY_APPSTORE_API", "https://env.example.com/")
	if got := resolveAppStoreAPIBase(""); got != "https://env.example.com" {
		t.Errorf("env precedence: got %q", got)
	}
	t.Setenv("WENDY_APPSTORE_API", "")
	if got := resolveAppStoreAPIBase(""); got != defaultAppStoreAPIBase {
		t.Errorf("default: got %q", got)
	}
}

func TestResolveAppImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/apps/jellyfin/image" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"app_id":"jellyfin","source":"dockerhub","repository":"jellyfin/jellyfin","tag":"latest","reference":"docker.io/jellyfin/jellyfin:latest"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := context.Background()

	res, err := resolveAppImage(ctx, srv.URL, "jellyfin")
	if err != nil {
		t.Fatalf("resolveAppImage: %v", err)
	}
	if res.Reference != "docker.io/jellyfin/jellyfin:latest" {
		t.Errorf("reference = %q", res.Reference)
	}
	if res.Source != "dockerhub" {
		t.Errorf("source = %q", res.Source)
	}

	if _, err := resolveAppImage(ctx, srv.URL, "does-not-exist"); err == nil {
		t.Error("expected error for unknown app, got nil")
	}
}
