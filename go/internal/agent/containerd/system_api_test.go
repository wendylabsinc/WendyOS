package containerd

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestAppSystemAPIOwnersFromLabelsRestoresEntitledMultiServiceContainers(t *testing.T) {
	notifications := []appconfig.Entitlement{{Type: appconfig.EntitlementNotifications}}
	labels := []map[string]string{
		wendyLabels("com.example.app", "api", "1", nil, notifications, "", nil),
		wendyLabels("com.example.app", "worker", "1", nil, notifications, "", nil),
		wendyLabels("com.example.other", "", "1", nil, nil, "", nil),
		{
			labelKeyAppID: "../tampered",
			appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNotifications: "",
		},
	}

	got := appSystemAPIOwnersFromLabels(labels)
	grant := services.SystemAPIGrant{Capabilities: []string{services.SystemAPICapabilityNotifications}}
	want := []appSystemAPIOwner{
		{appID: "com.example.app", serviceName: "api", grant: grant},
		{appID: "com.example.app", serviceName: "worker", grant: grant},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owners = %+v, want %+v", got, want)
	}
}

// A data-read allowlist travels in the container labels, so an agent restart
// restores the socket with the same read scope, not a wider one.
func TestAppSystemAPIOwnersFromLabelsRestoresDataReadAllowlist(t *testing.T) {
	labels := []map[string]string{
		wendyLabels("com.example.viewer", "", "1", nil, []appconfig.Entitlement{
			{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector", "apps/com.example.thermal/grid"}},
		}, "", nil),
		wendyLabels("com.example.all", "", "1", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementDataRead}}, "", nil),
	}
	got := appSystemAPIOwnersFromLabels(labels)
	want := []appSystemAPIOwner{
		{appID: "com.example.viewer", grant: services.SystemAPIGrant{
			Capabilities:      []string{services.SystemAPICapabilityDataRead},
			DataReadAllowlist: []string{"apps/com.example.detector", "apps/com.example.thermal/grid"},
		}},
		{appID: "com.example.all", grant: services.SystemAPIGrant{Capabilities: []string{services.SystemAPICapabilityDataRead}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owners = %+v, want %+v", got, want)
	}
}

// Every data-read allowlist the agent accepts comes back from the container
// labels as it went in, so a restart never changes an app's read scope. It
// refuses those that would not: [""] would come back as no allowlist, which
// reads every stream, and an entry with a comma as two entries.
func TestDataReadAllowlistSurvivesLabels(t *testing.T) {
	for _, tc := range []struct {
		allow   []string
		changes bool
	}{
		{[]string{"apps/com.example.detector"}, false},
		{[]string{"apps/com.example.detector/markers", "agent/campaign/people/markers/", "apps/"}, false},
		{[]string{""}, true},
		{[]string{"apps/a,agent/x"}, true},
	} {
		entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementDataRead, Allowlist: tc.allow}}
		created := services.SystemAPIGrantFor(entitlements)
		restored := services.SystemAPIGrantFor(parseEntitlementsFromAnnotations(appconfig.BuildEntitlementAnnotations(entitlements)))
		if changed := !reflect.DeepEqual(created, restored); changed != tc.changes {
			t.Fatalf("allowlist %q: restored %+v from %+v", tc.allow, restored, created)
		}
		if accepted := appconfig.ValidateDataReadAllowlists(entitlements) == nil; accepted == tc.changes {
			t.Errorf("allowlist %q: accepted=%v, but the labels change it=%v", tc.allow, accepted, tc.changes)
		}
	}
}
