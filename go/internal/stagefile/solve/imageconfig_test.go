package solve

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/llbgen"
)

// The image config is the half of a build LLB cannot describe. Everything a
// Stagefile declares that is metadata rather than filesystem — env, workdir,
// entrypoint, cmd, healthcheck, user — has to be stamped here, or the LLB path
// produces the same rootfs as the Dockerfile path inside an image that behaves
// differently. Nothing downstream would notice: the build succeeds either way.

func stampBase(t *testing.T, inner map[string]any) []byte {
	t.Helper()
	dt, err := json.Marshal(map[string]any{
		"os":           "linux",
		"architecture": "arm64",
		"config":       inner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dt
}

func stamp(t *testing.T, base []byte, cfg *llbgen.ImageConfig) map[string]json.RawMessage {
	t.Helper()
	out, err := imageConfig(base, cfg, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	var img map[string]json.RawMessage
	if err := json.Unmarshal(out, &img); err != nil {
		t.Fatalf("produced config is not JSON: %v", err)
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(img["config"], &inner); err != nil {
		t.Fatalf("produced config's config object is not JSON: %v", err)
	}
	return inner
}

func decodeStrings(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not a string list: %v", err)
	}
	return out
}

// A stage that sets one variable must keep the base image's PATH — the whole
// reason the base config is edited rather than replaced.
func TestEnvIsMergedOntoTheBase(t *testing.T) {
	base := stampBase(t, map[string]any{
		"Env": []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C"},
	})
	inner := stamp(t, base, &llbgen.ImageConfig{
		Env: map[string]string{"MODE": "prod", "LANG": "C.UTF-8"},
	})

	got := decodeStrings(t, inner["Env"])
	want := []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C.UTF-8", "MODE=prod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env = %v, want %v", got, want)
	}
}

// An overridden key keeps its position rather than being appended after the
// base's entry. An image config's Env may legally repeat a key and a runtime
// takes the last one, so appending would work — until something reads the list
// instead of resolving it.
func TestEnvOverrideReplacesInPlace(t *testing.T) {
	base := stampBase(t, map[string]any{"Env": []string{"A=1", "B=2", "C=3"}})
	inner := stamp(t, base, &llbgen.ImageConfig{Env: map[string]string{"B": "changed"}})

	got := decodeStrings(t, inner["Env"])
	want := []string{"A=1", "B=changed", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env = %v, want %v", got, want)
	}
}

func TestWorkdirIsStamped(t *testing.T) {
	base := stampBase(t, map[string]any{"WorkingDir": "/"})
	inner := stamp(t, base, &llbgen.ImageConfig{Workdir: "/srv"})

	var got string
	if err := json.Unmarshal(inner["WorkingDir"], &got); err != nil {
		t.Fatal(err)
	}
	if got != "/srv" {
		t.Fatalf("WorkingDir = %q, want /srv", got)
	}
}

// A Dockerfile's ENTRYPOINT resets CMD, so a base image's Cmd must not survive
// into an image that declares an entrypoint — its arguments would be appended
// to the entrypoint at run time, starting a different process than the
// Dockerfile path starts.
func TestEntrypointResetsTheBaseCmd(t *testing.T) {
	base := stampBase(t, map[string]any{"Cmd": []string{"python3"}})
	inner := stamp(t, base, &llbgen.ImageConfig{Entrypoint: []string{"/srv/app"}})

	if _, ok := inner["Cmd"]; ok {
		t.Fatalf("base Cmd survived an entrypoint: %s", inner["Cmd"])
	}
	if got := decodeStrings(t, inner["Entrypoint"]); !reflect.DeepEqual(got, []string{"/srv/app"}) {
		t.Fatalf("Entrypoint = %v", got)
	}
}

// A stage declaring both keeps its own cmd: the reset above must not eat it.
func TestDeclaredCmdSurvivesTheEntrypointReset(t *testing.T) {
	base := stampBase(t, map[string]any{"Cmd": []string{"python3"}})
	inner := stamp(t, base, &llbgen.ImageConfig{
		Entrypoint: []string{"/srv/app"},
		Cmd:        []string{"--serve"},
	})

	if got := decodeStrings(t, inner["Cmd"]); !reflect.DeepEqual(got, []string{"--serve"}) {
		t.Fatalf("Cmd = %v, want the declared --serve", got)
	}
}

// Durations are nanoseconds in an image config and Go-style strings in a
// Stagefile, and the exec form is marked by a "CMD" first element — without it
// a runtime reads the probe as a shell string.
func TestHealthcheckIsConvertedToTheImageConfigForm(t *testing.T) {
	inner := stamp(t, stampBase(t, nil), &llbgen.ImageConfig{
		Healthcheck: &ir.Healthcheck{
			Exec:        []string{"curl", "-f", "http://localhost/health"},
			Interval:    "30s",
			Timeout:     "5s",
			StartPeriod: "1m",
			Retries:     3,
		},
	})

	var got dockerHealthcheck
	if err := json.Unmarshal(inner["Healthcheck"], &got); err != nil {
		t.Fatal(err)
	}
	want := dockerHealthcheck{
		Test:        []string{"CMD", "curl", "-f", "http://localhost/health"},
		Interval:    30_000_000_000,
		Timeout:     5_000_000_000,
		StartPeriod: 60_000_000_000,
		Retries:     3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Healthcheck =\n %+v\nwant\n %+v", got, want)
	}
}

func TestHealthcheckRejectsAnUnparseableDuration(t *testing.T) {
	_, err := imageConfig(stampBase(t, nil), &llbgen.ImageConfig{
		Healthcheck: &ir.Healthcheck{Exec: []string{"true"}, Interval: "half a minute"},
	}, "linux/arm64")
	if err == nil {
		t.Fatal("accepted an unparseable healthcheck interval")
	}
}

// A stage that declares no user still gets the non-root default, and it is the
// same constant both backends substitute.
func TestUserDefaultsToTheSharedConstant(t *testing.T) {
	inner := stamp(t, stampBase(t, nil), &llbgen.ImageConfig{})
	var got string
	if err := json.Unmarshal(inner["User"], &got); err != nil {
		t.Fatal(err)
	}
	if got != ir.DefaultUser {
		t.Fatalf("User = %q, want %q", got, ir.DefaultUser)
	}
}

// A stage that declares nothing must not invent fields — an absent healthcheck
// is not an empty one, and an empty Env list is not the same as no override.
func TestNothingDeclaredStampsNothingExtra(t *testing.T) {
	base := stampBase(t, map[string]any{"Env": []string{"PATH=/usr/bin"}})
	inner := stamp(t, base, &llbgen.ImageConfig{})

	if _, ok := inner["Healthcheck"]; ok {
		t.Error("an absent healthcheck was stamped")
	}
	if _, ok := inner["WorkingDir"]; ok {
		t.Error("an absent workdir was stamped")
	}
	if got := decodeStrings(t, inner["Env"]); !reflect.DeepEqual(got, []string{"PATH=/usr/bin"}) {
		t.Errorf("Env = %v, want the base's untouched", got)
	}
}
