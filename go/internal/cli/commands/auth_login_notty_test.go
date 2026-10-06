package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func shrinkBrowserLoginTimeout(t *testing.T) {
	t.Helper()
	prev := browserLoginTimeout
	browserLoginTimeout = 100 * time.Millisecond
	t.Cleanup(func() { browserLoginTimeout = prev })
}

// stubOpenBrowser replaces openBrowser and returns a func reporting the URLs
// it was asked to open.
func stubOpenBrowser(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var opened []string
	prev := openBrowser
	openBrowser = func(u string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, u)
		return nil
	}
	t.Cleanup(func() { openBrowser = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), opened...)
	}
}

const testLoginURLPrefix = "https://cloud.example.invalid/cli-auth?redirect_uri="

// Without a TTY the legacy login used to launch a browser and then wait
// forever for a callback nobody would deliver.
func TestPerformLogin_NonInteractivePrintsURLAndTimesOut(t *testing.T) {
	stubNonInteractive(t)
	stubHumanPresent(t, false)
	shrinkBrowserLoginTimeout(t)
	opened := stubOpenBrowser(t)

	var err error
	out := captureStdout(t, func() {
		err = performLogin(context.Background(), "https://cloud.example.invalid", "grpc.example.invalid:443")
	})

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if got := opened(); len(got) != 0 {
		t.Fatalf("opened a browser without a TTY: %v", got)
	}
	var urlLine bool
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, testLoginURLPrefix) && line == strings.TrimSpace(line) {
			urlLine = true
		}
	}
	if !urlLine {
		t.Fatalf("want the login URL alone on its own line, got:\n%s", out)
	}
	// The iOS-app QR code is for a person to scan: no one is there.
	if strings.Contains(out, "iOS app") || strings.Contains(out, "█") {
		t.Fatalf("printed the QR code with no one at the terminal:\n%s", out)
	}
	// The same flow backs `cloud org switch`, org v2 and device enrollment,
	// so the advice must not name `wendy auth login`.
	if !strings.Contains(err.Error(), "run the command again") || strings.Contains(err.Error(), "wendy auth login") {
		t.Fatalf("err = %v, want it to say to run the command again, without naming `wendy auth login`", err)
	}
}

// With a TTY nothing changes: the browser is opened with the login URL.
func TestPerformLogin_InteractiveOpensBrowser(t *testing.T) {
	stubInteractive(t)
	stubHumanPresent(t, true)
	shrinkBrowserLoginTimeout(t)
	opened := stubOpenBrowser(t)

	var err error
	out := captureStdout(t, func() {
		err = performLogin(context.Background(), "https://cloud.example.invalid", "grpc.example.invalid:443")
	})

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if !strings.Contains(out, "iOS app") {
		t.Fatalf("interactive login should still show the iOS-app QR code:\n%s", out)
	}
	got := opened()
	if len(got) != 1 || !strings.HasPrefix(got[0], testLoginURLPrefix) {
		t.Fatalf("opened = %v, want exactly the login URL", got)
	}
}

// Ctrl-C while waiting for the browser must end the wait at once, in either
// mode — not after browserLoginTimeout.
func TestPerformLogin_CancelEndsWaitImmediately(t *testing.T) {
	stubNonInteractive(t)
	stubHumanPresent(t, false)
	stubOpenBrowser(t)
	// browserLoginTimeout stays at its 5-minute default: only the cancel can end this quickly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	var err error
	_ = captureStdout(t, func() {
		err = performLogin(ctx, "https://cloud.example.invalid", "grpc.example.invalid:443")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s to honour the cancel", elapsed)
	}
}

// `wendy auth login > login.log` typed in a terminal: stdout is not a
// terminal, but a person is there to use the browser, so it still opens.
func TestPerformLogin_PersonWithRedirectedOutputOpensBrowser(t *testing.T) {
	stubNonInteractive(t)
	stubHumanPresent(t, true)
	shrinkBrowserLoginTimeout(t)
	opened := stubOpenBrowser(t)

	_ = captureStdout(t, func() {
		_ = performLogin(context.Background(), "https://cloud.example.invalid", "grpc.example.invalid:443")
	})
	if got := opened(); len(got) != 1 {
		t.Fatalf("opened = %v, want the browser opened for the person at the terminal", got)
	}
}

// The OIDC flow (`wendy auth login --issuer …`) follows the same rules with
// no one at the terminal: no browser, the sign-in URL alone on its own line,
// and a bounded wait that ends in an error saying what to do.
func TestPerformOIDCLogin_NonInteractivePrintsURLAndTimesOut(t *testing.T) {
	stubNonInteractive(t)
	stubHumanPresent(t, false)
	shrinkBrowserLoginTimeout(t)
	opened := stubOpenBrowser(t)

	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           issuer,
			"authorization_endpoint":           issuer + "/authorize",
			"token_endpoint":                   issuer + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	}))
	defer srv.Close()
	issuer = srv.URL

	var err error
	out := captureStdout(t, func() {
		err = performOIDCLogin(context.Background(), oidcLoginOptions{
			Issuer:           issuer,
			ClientID:         "wendy-cli",
			CloudResource:    "https://cloud.example.invalid",
			IdentityResource: "https://pki.example.invalid",
			IdentityEndpoint: "https://pki.example.invalid/v1/identity/certificate",
			CloudGRPC:        "grpc.example.invalid:443",
		})
	})

	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("err = %v, want an actionable timeout error", err)
	}
	if got := opened(); len(got) != 0 {
		t.Fatalf("opened a browser with no one at the terminal: %v", got)
	}
	var urlLine bool
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, issuer+"/authorize?") && line == strings.TrimSpace(line) {
			urlLine = true
		}
	}
	if !urlLine {
		t.Fatalf("want the authorize URL alone on its own line, got:\n%s", out)
	}
}
