package liteenroll

import (
	"context"
	"errors"
	wendy "github.com/wendylabsinc/wendy-go"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoveryRedirectKeepsNativeDiagnostic(t *testing.T) {
	const refusal = "CA discovery redirects are not allowed"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Error("redirect followed")
		}
		w.Header().Set("Location", "/blocked")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			native := *srv.Client()
			native.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New(refusal) }
			req, _ := http.NewRequest(method, srv.URL, nil)
			_, want := native.Do(req)
			host, err := newDiscoveryHTTP(srv.Client(), refusal)
			if err != nil {
				t.Fatal(err)
			}
			_, got := host.DoLite(context.Background(), wendy.HTTPRequest{Method: method, URL: srv.URL})
			if want == nil || got == nil || got.Error() != want.Error() {
				t.Fatalf("redirect error=%v want=%v", got, want)
			}
		})
	}
}
