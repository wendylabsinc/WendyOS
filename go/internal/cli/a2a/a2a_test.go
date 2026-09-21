package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEndpointValidation(t *testing.T) {
	for _, s := range []string{"http://192.168.1.2:80", "https://user:pass@example.com", "https://example.com?token=x", "file:///tmp/task", "https://agent.example/a2a", "http://localhost:8787/a2a"} {
		if _, err := NewClient(s, "test-access-token-123"); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	for _, s := range []string{"https://agent.example/", "http://127.0.0.1:8787", "http://[::1]:8787"} {
		if _, err := NewClient(s, "test-access-token-123"); err != nil {
			t.Fatal(err)
		}
	}
}
func TestClientNeverFollowsCredentialRedirects(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := NewClient(source.URL, "test-access-token-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), "GET", "/tasks", nil, nil); err == nil || called {
		t.Fatal("followed redirect", err)
	}
}

func TestClientPinsLocalhostAndRejectsWeakTokens(t *testing.T) {
	client, err := NewClient("http://localhost:8787", "test-access-token-123")
	if err != nil || client.URL != "http://127.0.0.1:8787" {
		t.Fatalf("client=%+v err=%v", client, err)
	}
	for _, token := range []string{"", "short", "long-but-invalid\ntoken"} {
		if _, err := NewClient("http://127.0.0.1:8787", token); err == nil {
			t.Fatal("accepted invalid token")
		}
	}
}
