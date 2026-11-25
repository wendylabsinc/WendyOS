package main

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"testing"
)

func TestPreparationRejectsChangedSelectionOrOutstandingPublication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime runtime
		request request
	}{
		{"unselected", runtime{role: "reader"}, request{Op: "prepare-inspection", InputDigest: "selected"}},
		{"changed", runtime{role: "reader", lastResult: &incident.Result{InputDigest: "new"}}, request{Op: "prepare-inspection", InputDigest: "old"}},
		{"pending", runtime{role: "reader", lastResult: &incident.Result{InputDigest: "selected"}, p: &publisher{pending: &signedPending{}}}, request{Op: "prepare-inspection", InputDigest: "selected"}},
		{"withdrawn", runtime{role: "reader", lastResult: &incident.Result{InputDigest: "selected"}, p: &publisher{}}, request{Op: "prepare-inspection", InputDigest: "selected"}},
		{"visual-role", runtime{role: "visual"}, request{Op: "prepare-inspection", InputDigest: "selected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.runtime.step(context.Background(), tc.request); err == nil {
				t.Fatal("non-current selection prepared")
			}
		})
	}
}
