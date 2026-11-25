package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func TestEvidenceUsageRejectsMalformedAccounting(t *testing.T) {
	cfg, _ := credentials(t)
	store, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "t", Site: "s"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	original, err := store.Usage()
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*evidence.Usage)
	}{
		{"valid", func(*evidence.Usage) {}},
		{"wrong-site", func(u *evidence.Usage) { u.Site = "other" }},
		{"reservation-claim", func(u *evidence.Usage) { u.LimitSemantics = "reservations" }},
		{"missing-modality", func(u *evidence.Usage) { u.Modalities = u.Modalities[:3] }},
		{"duplicate-modality", func(u *evidence.Usage) { u.Modalities[1].Modality = u.Modalities[0].Modality }},
		{"negative", func(u *evidence.Usage) { u.Modalities[0].PinnedBytes = -1 }},
		{"inconsistent-total", func(u *evidence.Usage) { u.Totals.CommittedBytes = 1 }},
		{"beyond-global", func(u *evidence.Usage) { u.Modalities[0].QuotaBytes = u.QuotaBytes + 1 }},
		{"retention", func(u *evidence.Usage) { u.Modalities[0].MaxRetentionMS = u.MaxRetentionMS + 1 }},
		{"unbounded-counts", func(u *evidence.Usage) { u.Totals.Entries = 100001 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			raw, _ := json.Marshal(original)
			var out evidence.Usage
			json.Unmarshal(raw, &out)
			change.mutate(&out)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/evidence/usage" {
					t.Error("wrong usage request")
				}
				json.NewEncoder(w).Encode(out)
			}))
			defer server.Close()
			cfg.URL = server.URL
			c, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.EvidenceUsage(context.Background())
			if (e == nil) != (change.name == "valid") {
				t.Fatal(change.name, e)
			}
		})
	}
}
