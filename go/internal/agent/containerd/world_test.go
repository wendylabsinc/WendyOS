package containerd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

type worldProviderFixture struct {
	calls                   int
	app, service, directory string
	generation              uint64
	publish                 []appconfig.WorldPublication
	subscribe               []appconfig.WorldSubscription
	wantSubscribe           []appconfig.WorldSubscription
	fail                    bool
}

func (p *worldProviderFixture) EnsureAccess(string, string, uint64, []appconfig.WorldPublication, []appconfig.WorldSubscription) (string, error) {
	panic("restoration must not create authority")
}
func (p *worldProviderFixture) ValidateAccessMount(app, service string, generation uint64, directory string, publish []appconfig.WorldPublication, subscribe []appconfig.WorldSubscription) error {
	p.calls++
	p.app = app
	p.service = service
	p.generation = generation
	p.directory = directory
	p.publish = publish
	p.subscribe = subscribe
	if p.fail || p.wantSubscribe != nil && !reflect.DeepEqual(subscribe, p.wantSubscribe) {
		return errors.New("no existing generation binding")
	}
	return nil
}

func TestWorldReadOnlySubscriptionBindsRestoredMount(t *testing.T) {
	want := []appconfig.WorldSubscription{{App: "com.example.camera", Service: "front", ComponentType: "wendy.sensor.capture.v1", Stream: "camera-a"}}
	ent := appconfig.Entitlement{Type: appconfig.EntitlementWorld, WorldSubscribe: want}
	labels := worldTestLabels()
	labels[appconfig.EntitlementAnnotationKeyPrefix+appconfig.EntitlementWorld] = appconfig.EntitlementAnnotationValue(ent)
	provider := &worldProviderFixture{wantSubscribe: want}
	c := &Client{worldSocketProvider: provider}
	if err := c.validateWorldMount(labels); err != nil {
		t.Fatal(err)
	}
	if len(provider.publish) != 0 || !reflect.DeepEqual(provider.subscribe, want) {
		t.Fatal("read-only authority changed")
	}
	ent.WorldSubscribe = append([]appconfig.WorldSubscription(nil), want...)
	ent.WorldSubscribe[0].Stream = "camera-b"
	labels[appconfig.EntitlementAnnotationKeyPrefix+appconfig.EntitlementWorld] = appconfig.EntitlementAnnotationValue(ent)
	if c.validateWorldMount(labels) == nil {
		t.Fatal("changed subscription reused old generation binding")
	}
}
func (p *worldProviderFixture) Release(string, string, uint64) {}
func (p *worldProviderFixture) ReleaseApp(string)              {}

func worldTestLabels() map[string]string {
	labels := wendyLabels("com.example.camera", "tracker", "v1", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementWorld, WorldPublish: []appconfig.WorldPublication{{ComponentType: "wendy.pose.v1", Stream: "people"}}}}, "", nil)
	labels[worldGenerationLabel] = "17"
	labels[worldDirectoryLabel] = "/private/world/service-example"
	return labels
}

func TestWorldRestoreOnlyValidatesExactPersistedBinding(t *testing.T) {
	provider := &worldProviderFixture{}
	client := &Client{worldSocketProvider: provider}
	if err := client.validateWorldMount(worldTestLabels()); err != nil {
		t.Fatal(err)
	}
	want := []appconfig.WorldPublication{{ComponentType: "wendy.pose.v1", Stream: "people"}}
	if provider.calls != 1 || provider.generation != 17 || provider.app != "com.example.camera" || provider.service != "tracker" || provider.directory != "/private/world/service-example" || !reflect.DeepEqual(provider.publish, want) {
		t.Fatalf("binding lost: %+v", provider)
	}
	provider.fail = true
	if err := client.validateWorldMount(worldTestLabels()); err == nil {
		t.Fatal("restart silently recreated authority")
	}
}

func TestWorldInvalidPersistedScopeNeverReachesProvider(t *testing.T) {
	for name, edit := range map[string]func(map[string]string){
		"missing generation":      func(l map[string]string) { delete(l, worldGenerationLabel) },
		"zero generation":         func(l map[string]string) { l[worldGenerationLabel] = "0" },
		"noncanonical generation": func(l map[string]string) { l[worldGenerationLabel] = "017" },
		"missing service":         func(l map[string]string) { delete(l, labelKeyServiceName) },
		"relative directory":      func(l map[string]string) { l[worldDirectoryLabel] = "service" },
		"unclean directory":       func(l map[string]string) { l[worldDirectoryLabel] = "/private/../world" },
		"invalid scope": func(l map[string]string) {
			l[appconfig.EntitlementAnnotationKeyPrefix+appconfig.EntitlementWorld] = "present=true"
		},
		"orphan metadata": func(l map[string]string) {
			delete(l, appconfig.EntitlementAnnotationKeyPrefix+appconfig.EntitlementWorld)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := &worldProviderFixture{}
			c := &Client{worldSocketProvider: p}
			labels := worldTestLabels()
			edit(labels)
			if c.validateWorldMount(labels) == nil || p.calls != 0 {
				t.Fatal("invalid persisted scope admitted")
			}
		})
	}
	if (&Client{}).validateWorldMount(worldTestLabels()) == nil {
		t.Fatal("absent provider admitted")
	}
	if err := (&Client{}).validateWorldMount(nil); err != nil {
		t.Fatal(err)
	}
}

func TestWorldDeploymentIdentityIsNonzero(t *testing.T) {
	a, err := worldGeneration()
	if err != nil || a == 0 {
		t.Fatal(a, err)
	}
	b, err := worldGeneration()
	if err != nil || b == 0 || a == b {
		t.Fatal(b, err)
	}
}
