package main

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
)

func TestPrepareRisingRequiresExactAcknowledgedSelection(t *testing.T) {
	for _, test := range []struct {
		name   string
		r      runtime
		digest string
	}{
		{"empty", runtime{}, ""},
		{"no publication", runtime{observedResult: &incidentchange.Result{InputDigest: "selected"}}, "selected"},
		{"changed", runtime{observedResult: &incidentchange.Result{InputDigest: "new"}, publishedResult: &incidentchange.Result{InputDigest: "selected"}, p: &publisher{}}, "selected"},
		{"withdrawn", runtime{observedResult: &incidentchange.Result{InputDigest: "selected"}, publishedResult: &incidentchange.Result{InputDigest: "selected"}, p: &publisher{last: &signedPending{component: world.Component{Tombstone: true}}}}, "selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.r.prepareInspection(context.Background(), test.digest); err == nil {
				t.Fatal("unacknowledged/changed selection accepted")
			}
		})
	}
}
