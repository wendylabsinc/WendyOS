package containerd

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// recordingSocketProvider records the release calls a delete made. It stands in
// for both the System API and the data socket managers, whose Release and
// ReleaseApp signatures differ only in Ensure.
type recordingSocketProvider struct {
	released    [][2]string
	releasedApp []string
	swept       [][]string
}

func (p *recordingSocketProvider) Release(appID, serviceName string) {
	p.released = append(p.released, [2]string{appID, serviceName})
}
func (p *recordingSocketProvider) ReleaseApp(appID string) {
	p.releasedApp = append(p.releasedApp, appID)
}
func (p *recordingSocketProvider) SweepOrphanedRoots(activeAppIDs []string) {
	p.swept = append(p.swept, activeAppIDs)
}

type recordingSystemAPIProvider struct{ recordingSocketProvider }

func (p *recordingSystemAPIProvider) Ensure(string, string, []string) (string, error) {
	return "", nil
}

type recordingDataProvider struct{ recordingSocketProvider }

func (p *recordingDataProvider) Ensure(string, string) (string, error) { return "", nil }

// TestReleaseSocketsAfterDeleteReleasesEachDeletedService covers the leak: a
// delete that removed one service of a multi-service app released nothing,
// because the release was conditioned on the delete having addressed the whole
// app. The departing service stayed registered as a socket owner until the
// agent restarted, and the app's allowlist union stayed as wide as that service
// had made it.
func TestReleaseSocketsAfterDeleteReleasesEachDeletedService(t *testing.T) {
	systemAPI := &recordingSystemAPIProvider{}
	dataSocket := &recordingDataProvider{}
	client := &Client{systemAPISocketProvider: systemAPI, dataSocketProvider: dataSocket}

	client.releaseSocketsAfterDelete("com.example.app", []string{"worker"}, false, true)

	want := [][2]string{{"com.example.app", "worker"}}
	if !reflect.DeepEqual(systemAPI.released, want) {
		t.Fatalf("System API releases = %v, want %v", systemAPI.released, want)
	}
	if !reflect.DeepEqual(dataSocket.released, want) {
		t.Fatalf("data socket releases = %v, want %v", dataSocket.released, want)
	}
	if len(systemAPI.releasedApp) != 0 || len(dataSocket.releasedApp) != 0 {
		t.Fatalf("a partial delete released the whole app: %v %v", systemAPI.releasedApp, dataSocket.releasedApp)
	}
}

// TestReleaseSocketsAfterDeleteWholeAppReleasesOnce keeps the existing
// behaviour: when the delete took every container, the socket goes as a whole
// rather than one owner at a time.
func TestReleaseSocketsAfterDeleteWholeAppReleasesOnce(t *testing.T) {
	systemAPI := &recordingSystemAPIProvider{}
	dataSocket := &recordingDataProvider{}
	client := &Client{systemAPISocketProvider: systemAPI, dataSocketProvider: dataSocket}

	client.releaseSocketsAfterDelete("com.example.app", []string{"api", "worker"}, true, true)

	want := []string{"com.example.app"}
	if !reflect.DeepEqual(systemAPI.releasedApp, want) || !reflect.DeepEqual(dataSocket.releasedApp, want) {
		t.Fatalf("whole-app releases = %v %v, want %v", systemAPI.releasedApp, dataSocket.releasedApp, want)
	}
	if len(systemAPI.released) != 0 || len(dataSocket.released) != 0 {
		t.Fatalf("whole-app delete also released per service: %v %v", systemAPI.released, dataSocket.released)
	}
}

// TestReleaseSocketsAfterDeleteKeepsOwnersWhenADeleteFailed states the safety
// rule: a container that is still there is still an owner.
func TestReleaseSocketsAfterDeleteKeepsOwnersWhenADeleteFailed(t *testing.T) {
	systemAPI := &recordingSystemAPIProvider{}
	dataSocket := &recordingDataProvider{}
	client := &Client{systemAPISocketProvider: systemAPI, dataSocketProvider: dataSocket}

	client.releaseSocketsAfterDelete("com.example.app", []string{"worker"}, false, false)
	client.releaseSocketsAfterDelete("com.example.app", []string{"worker"}, true, false)

	if len(systemAPI.released)+len(systemAPI.releasedApp)+len(dataSocket.released)+len(dataSocket.releasedApp) != 0 {
		t.Fatal("a failed delete released socket ownership")
	}
}

// TestReleaseSocketsAfterDeleteToleratesMissingProviders covers the agent
// configurations that wire in neither socket manager.
func TestReleaseSocketsAfterDeleteToleratesMissingProviders(t *testing.T) {
	client := &Client{}
	client.releaseSocketsAfterDelete("com.example.app", []string{"worker"}, false, true)
	client.releaseSocketsAfterDelete("com.example.app", nil, true, true)
}

// TestSweepOrphanedSocketRootsPassesTheLiveAppSet checks the restore-time
// sweep reaches both providers with the set of apps that still have containers.
func TestSweepOrphanedSocketRootsPassesTheLiveAppSet(t *testing.T) {
	systemAPI := &recordingSystemAPIProvider{}
	dataSocket := &recordingDataProvider{}
	client := &Client{systemAPISocketProvider: systemAPI, dataSocketProvider: dataSocket}

	client.sweepOrphanedSocketRoots([]string{"com.example.app"})

	want := [][]string{{"com.example.app"}}
	if !reflect.DeepEqual(systemAPI.swept, want) || !reflect.DeepEqual(dataSocket.swept, want) {
		t.Fatalf("sweeps = %v %v, want %v", systemAPI.swept, dataSocket.swept, want)
	}
}

// TestAppIDsFromLabelsIsDistinctAndValidated pins what the sweep is told is
// live. Labels are external state, so a value that is not a legal app identity
// must not decide which socket directories survive; duplicates from a
// multi-service app collapse to one.
func TestAppIDsFromLabelsIsDistinctAndValidated(t *testing.T) {
	notifications := []appconfig.Entitlement{{Type: appconfig.EntitlementNotifications}}
	labels := []map[string]string{
		wendyLabels("com.example.app", "api", "1", nil, notifications, "", nil),
		wendyLabels("com.example.app", "worker", "1", nil, notifications, "", nil),
		wendyLabels("com.example.other", "", "1", nil, nil, "", nil),
		{labelKeyAppID: "../tampered"},
		{labelKeyAppID: ""},
		{},
	}

	got := appIDsFromLabels(labels)
	want := []string{"com.example.app", "com.example.other"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appIDsFromLabels = %v, want %v", got, want)
	}
}
