package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const (
	missTenant  = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	missStaleID = "7791d76e-a942-4a8e-b582-063d063b5623"
	missNewID   = "a20403ae-9251-4937-aa9b-bb1d773df071"
	missOtherID = "ede01a43-7ef8-49d9-bfaf-88192b5124af"

	missCheckStep      = "Check the device's power and network connection, then retry."
	missReenrolledStep = "If the device was enrolled again, it has a new entry: find it with 'wendy cloud discover --all --json'."
	missFindStep       = "Find the device's current entry with 'wendy cloud discover --all --json'."
	missChooseStep     = "Choose another device with --device or 'wendy device set-default'."
)

// stubCloudSelectorRosterV2 answers a tenant selector's first, online-only
// query with online, and the query that explains a miss with all (or allErr).
// It returns the selector of the asset missStaleID and the onlineOnly argument
// of every query made.
func stubCloudSelectorRosterV2(t *testing.T, online, all []*cloudpbv2.Asset, allErr error) (cloudDeviceSelector, *[]bool) {
	t.Helper()
	auth := config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{
		PrincipalURI: "spiffe://wendy.sh/tenant/" + missTenant + "/operator/test",
	}}}
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}})
	oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() { fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect })
	queries := &[]bool{}
	fetchDefaultCloudAssetsV2Fn = func(_ context.Context, _ *config.AuthConfig, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
		*queries = append(*queries, onlineOnly)
		if onlineOnly {
			return online, nil
		}
		return all, allErr
	}
	connectDefaultCloudAssetV2Fn = func(context.Context, *config.AuthConfig, *cloudpbv2.Asset, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{}, nil
	}
	return cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: missTenant, AssetUUID: missStaleID}, queries
}

// stubCloudSelectorRosterLegacy is stubCloudSelectorRosterV2 for an
// organization selector, of asset 42 in organization 7. It returns how many
// times each list was queried.
func stubCloudSelectorRosterLegacy(t *testing.T, online, all []*cloudpb.Asset, allErr error) (selector cloudDeviceSelector, onlineQueries, allQueries *int) {
	t.Helper()
	auth := pickerAuth(7)
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	oldFetch, oldAll, oldConnect := fetchDefaultCloudAssetsFn, fetchDefaultCloudAllAssetsFn, connectDefaultCloudAssetFn
	t.Cleanup(func() {
		fetchDefaultCloudAssetsFn, fetchDefaultCloudAllAssetsFn, connectDefaultCloudAssetFn = oldFetch, oldAll, oldConnect
	})
	onlineQueries, allQueries = new(int), new(int)
	fetchDefaultCloudAssetsFn = func(context.Context, *config.AuthConfig) ([]*cloudpb.Asset, error) {
		*onlineQueries++
		return online, nil
	}
	fetchDefaultCloudAllAssetsFn = func(context.Context, *config.AuthConfig) ([]*cloudpb.Asset, error) {
		*allQueries++
		return all, allErr
	}
	connectDefaultCloudAssetFn = func(context.Context, *config.AuthConfig, *cloudpb.Asset, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{}, nil
	}
	return cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: 7, AssetID: 42}, onlineQueries, allQueries
}

// checkCloudSelectorMiss checks the class, the text a person reads (the
// headline, then each step once) and the steps JSON mode lists separately.
func checkCloudSelectorMiss(t *testing.T, err error, class, headline string, steps []string) {
	t.Helper()
	if err == nil {
		t.Fatal("a selector whose device is not online connected")
	}
	if got := ErrorClass(err); got != class {
		t.Errorf("class = %q, want %q", got, class)
	}
	if got := NextSteps(err); !reflect.DeepEqual(got, steps) {
		t.Errorf("NextSteps = %q, want %q", got, steps)
	}
	if want := headline + "\n  " + strings.Join(steps, "\n  "); err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if got := messageWithoutNextSteps(err); got != headline {
		t.Errorf("message without steps = %q, want %q", got, headline)
	}
}

// A saved or typed cloud:// selector names one asset ID. When that asset is not
// online, the error must say whether Cloud still has it (offline) or not
// (unenrolled, or enrolled again under a new ID), and point at the device's
// new entry when the old record was left behind (WDY-4295). Whether Cloud keeps
// or deletes the old record on re-enrollment is not known, so both are covered.
func TestCloudSelectorMissTellsOfflineFromUnenrolled(t *testing.T) {
	v2 := func(id, name string) *cloudpbv2.Asset { return &cloudpbv2.Asset{Id: id, Name: name} }
	offline := "cloud asset " + missStaleID + " in tenant " + missTenant + " is enrolled but currently reported offline"
	newEntry := `Another device named "thor" is enrolled in this tenant. If this one was enrolled again, use the new entry: ` +
		"cloud://cloud.example:443/tenant/" + missTenant + "/asset/" + missNewID
	for _, tc := range []struct {
		name        string
		online, all []*cloudpbv2.Asset
		class       string
		headline    string
		steps       []string
	}{
		{
			name:     "enrolled but offline",
			online:   []*cloudpbv2.Asset{v2(missOtherID, "arm")},
			all:      []*cloudpbv2.Asset{v2(missStaleID, "thor"), v2(missOtherID, "arm")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, missReenrolledStep, missChooseStep},
		},
		{
			name:     "old record deleted: unenrolled, or enrolled again under a new ID",
			online:   []*cloudpbv2.Asset{v2(missNewID, "thor")},
			all:      []*cloudpbv2.Asset{v2(missNewID, "thor")},
			class:    "no_device",
			headline: "cloud asset " + missStaleID + " is not enrolled in tenant " + missTenant + "; it may have been unenrolled, or enrolled again under a new ID",
			steps:    []string{missFindStep, missChooseStep},
		},
		{
			name:     "old record kept beside the new one",
			online:   []*cloudpbv2.Asset{v2(missNewID, "thor")},
			all:      []*cloudpbv2.Asset{v2(missStaleID, "thor"), v2(missNewID, "thor")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, newEntry, missChooseStep},
		},
		{
			name:     "two old records kept: the online device is the new one",
			online:   []*cloudpbv2.Asset{v2(missNewID, "Thor")},
			all:      []*cloudpbv2.Asset{v2(missStaleID, "thor"), v2(missOtherID, "thor"), v2(missNewID, "Thor")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, newEntry, missChooseStep},
		},
		{
			name:     "new entry offline too",
			all:      []*cloudpbv2.Asset{v2(missStaleID, "thor"), v2(missNewID, "thor")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, newEntry, missChooseStep},
		},
		{
			name:     "several devices share the name: none is singled out",
			online:   []*cloudpbv2.Asset{v2(missNewID, "thor"), v2(missOtherID, "thor")},
			all:      []*cloudpbv2.Asset{v2(missStaleID, "thor"), v2(missNewID, "thor"), v2(missOtherID, "thor")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, missReenrolledStep, missChooseStep},
		},
		{
			name:     "unnamed devices are not matched by name",
			online:   []*cloudpbv2.Asset{v2(missNewID, "")},
			all:      []*cloudpbv2.Asset{v2(missStaleID, ""), v2(missNewID, "")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, missReenrolledStep, missChooseStep},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, queries := stubCloudSelectorRosterV2(t, tc.online, tc.all, nil)
			_, err := connectCloudDeviceSelector(context.Background(), selector)
			checkCloudSelectorMiss(t, err, tc.class, tc.headline, tc.steps)
			if !reflect.DeepEqual(*queries, []bool{true, false}) {
				t.Errorf("queries (onlineOnly) = %v, want one online-only query and one of every device", *queries)
			}
		})
	}
}

func TestLegacyCloudSelectorMissTellsOfflineFromUnenrolled(t *testing.T) {
	offline := "cloud asset 42 in organization 7 is enrolled but currently reported offline"
	for _, tc := range []struct {
		name        string
		online, all []*cloudpb.Asset
		class       string
		headline    string
		steps       []string
	}{
		{
			name:     "enrolled but offline",
			online:   []*cloudpb.Asset{cloudAssetFixture(9, "arm")},
			all:      []*cloudpb.Asset{cloudAssetFixture(42, "thor"), cloudAssetFixture(9, "arm")},
			class:    "device_offline",
			headline: offline,
			steps:    []string{missCheckStep, missReenrolledStep, missChooseStep},
		},
		{
			name:     "old record deleted: unenrolled, or enrolled again under a new ID",
			online:   []*cloudpb.Asset{cloudAssetFixture(43, "thor")},
			all:      []*cloudpb.Asset{cloudAssetFixture(43, "thor")},
			class:    "no_device",
			headline: "cloud asset 42 is not enrolled in organization 7; it may have been unenrolled, or enrolled again under a new ID",
			steps:    []string{missFindStep, missChooseStep},
		},
		{
			name:     "old record kept beside the new one",
			online:   []*cloudpb.Asset{cloudAssetFixture(43, "thor")},
			all:      []*cloudpb.Asset{cloudAssetFixture(42, "thor"), cloudAssetFixture(43, "thor")},
			class:    "device_offline",
			headline: offline,
			steps: []string{missCheckStep,
				`Another device named "thor" is enrolled in this organization. If this one was enrolled again, use the new entry: cloud://cloud.example:443/org/7/asset/43`,
				missChooseStep},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, onlineQueries, allQueries := stubCloudSelectorRosterLegacy(t, tc.online, tc.all, nil)
			_, err := connectCloudDeviceSelector(context.Background(), selector)
			checkCloudSelectorMiss(t, err, tc.class, tc.headline, tc.steps)
			if *onlineQueries != 1 || *allQueries != 1 {
				t.Errorf("queries: %d online-only, %d of every device; want one of each", *onlineQueries, *allQueries)
			}
		})
	}
}

// The query that explains a miss is best effort: when it fails, the selector
// reports what it always did, not the failed lookup.
func TestCloudSelectorMissKeepsItsMessageWhenTheRosterQueryFails(t *testing.T) {
	down := errors.New("listing devices: cloud unavailable")
	t.Run("tenant", func(t *testing.T) {
		selector, queries := stubCloudSelectorRosterV2(t, nil, nil, down)
		_, err := connectCloudDeviceSelector(context.Background(), selector)
		want := "cloud asset " + missStaleID + " in tenant " + missTenant + " is offline or unavailable; choose another device with --device or 'wendy device set-default'"
		if err == nil || err.Error() != want || errors.Is(err, down) || NextSteps(err) != nil || ErrorClass(err) != "" {
			t.Fatalf("error = %v (class %q, steps %q), want %q", err, ErrorClass(err), NextSteps(err), want)
		}
		if !reflect.DeepEqual(*queries, []bool{true, false}) {
			t.Errorf("queries (onlineOnly) = %v, want the miss to be looked up once", *queries)
		}
	})
	t.Run("organization", func(t *testing.T) {
		selector, _, allQueries := stubCloudSelectorRosterLegacy(t, nil, nil, down)
		_, err := connectCloudDeviceSelector(context.Background(), selector)
		want := "cloud asset 42 in organization 7 is offline or unavailable; choose another device with --device or 'wendy device set-default'"
		if err == nil || err.Error() != want || errors.Is(err, down) || NextSteps(err) != nil || ErrorClass(err) != "" {
			t.Fatalf("error = %v (class %q, steps %q), want %q", err, ErrorClass(err), NextSteps(err), want)
		}
		if *allQueries != 1 {
			t.Errorf("the miss was looked up %d times, want once", *allQueries)
		}
	})
}

// A selector whose device is online costs the one online-only query it always
// did: the second query exists only to explain a miss.
func TestCloudSelectorQueriesOnlyOnlineDevicesOnSuccess(t *testing.T) {
	t.Run("tenant", func(t *testing.T) {
		roster := []*cloudpbv2.Asset{{Id: missStaleID, Name: "thor"}}
		selector, queries := stubCloudSelectorRosterV2(t, roster, roster, nil)
		if _, err := connectCloudDeviceSelector(context.Background(), selector); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*queries, []bool{true}) {
			t.Errorf("queries (onlineOnly) = %v, want a single online-only query", *queries)
		}
	})
	t.Run("organization", func(t *testing.T) {
		roster := []*cloudpb.Asset{cloudAssetFixture(42, "thor")}
		selector, onlineQueries, allQueries := stubCloudSelectorRosterLegacy(t, roster, roster, nil)
		if _, err := connectCloudDeviceSelector(context.Background(), selector); err != nil {
			t.Fatal(err)
		}
		if *onlineQueries != 1 || *allQueries != 0 {
			t.Errorf("queries: %d online-only, %d of every device; want 1 and 0", *onlineQueries, *allQueries)
		}
	})
}
