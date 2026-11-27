package client

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAllReadMethodsValidatePartialReplicaScope(t *testing.T) {
	for _, method := range []string{"query", "watch", "selected-query", "selected-watch"} {
		t.Run(method, func(t *testing.T) {
			cfg, _ := credentials(t)
			p := subscription.Policy{Version: 1, Tenant: "t", Site: "s", Selection: selection.Selection{Types: []string{"gas"}}, RequiredTypes: []string{sitepub.Type}}
			scope := p.Scope()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(method, "selected") {
					json.NewEncoder(w).Encode(selection.Response{ReplicaScope: scope, Version: 1, Components: []world.Component{}, Revision: "revision", Complete: true, ObservedAt: time.Now()})
				} else {
					json.NewEncoder(w).Encode(Snapshot{ReplicaScope: scope, Components: []world.Component{}, Cursor: strings.Repeat("a", 64)})
				}
			}))
			defer server.Close()
			cfg.URL = server.URL
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			read := func() error {
				switch method {
				case "query":
					_, err := c.Query(context.Background())
					return err
				case "watch":
					_, err := c.Watch(context.Background(), "", 0)
					return err
				case "selected-query":
					_, err := c.QuerySelected(context.Background(), selection.Request{})
					return err
				default:
					_, err := c.WatchSelected(context.Background(), selection.WatchRequest{})
					return err
				}
			}
			if err = read(); err != nil {
				t.Fatal("valid partial scope refused", err)
			}
			for _, change := range []func(*selection.ReplicaScope){func(s *selection.ReplicaScope) { s.Partial = false }, func(s *selection.ReplicaScope) { s.Tenant = "other" }, func(s *selection.ReplicaScope) { s.Site = "other" }, func(s *selection.ReplicaScope) { s.PolicySHA256 = "bad" }, func(s *selection.ReplicaScope) { s.Selection.Types = []string{"other"} }, func(s *selection.ReplicaScope) { s.RequiredTypes = nil }} {
				scope = p.Scope()
				change(scope)
				if err = read(); err == nil {
					t.Fatal("malformed scope accepted")
				}
			}
			scope = nil
			if err = read(); err != nil {
				t.Fatal("legacy full response rejected", err)
			}
		})
	}
}
