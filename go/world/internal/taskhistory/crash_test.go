package taskhistory

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

type crashFixture struct {
	Path        string
	Root        ed25519.PublicKey
	Now         time.Time
	Origins     map[string]Origin
	Page        Page
	Revocations authority.SignedRevocations
}

func TestHistoryCrashChild(t *testing.T) {
	path := os.Getenv("WORLD_TASK_HISTORY_CRASH_FIXTURE")
	if path == "" {
		t.Skip("child only")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var f crashFixture
	if e = json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: f.Root, Tenant: "tenant", Site: "office", Audience: "gateway-b", Domain: "simulation", Now: func() time.Time { return f.Now }, Replay: authority.NewMemoryReplay(100)})
	if e != nil {
		t.Fatal(e)
	}
	if e = v.UpdateRevocations(f.Revocations); e != nil {
		t.Fatal(e)
	}
	s, e := Open(f.Path, Config{Tenant: "tenant", Site: "office", Origins: f.Origins, Verifier: v, Now: func() time.Time { return f.Now }})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	atomicWriteHook = func(phase string) {
		if phase == os.Getenv("WORLD_TASK_HISTORY_CRASH_PHASE") {
			if e = os.WriteFile(path+".ready", []byte(phase), 0600); e != nil {
				panic(e)
			}
			for {
				time.Sleep(time.Hour)
			}
		}
	}
	if e = s.Import(context.Background(), f.Page, "relay"); e != nil {
		t.Fatal(e)
	}
	t.Fatal("expected parent to kill blocked child")
}
func TestChildKillKeepsRecordAndCursorAtomic(t *testing.T) {
	for _, phase := range []string{"synced-temp", "renamed"} {
		t.Run(phase, func(t *testing.T) {
			r := newRig(t)
			one, two := r.receipt(t, "one"), r.receipt(t, "two")
			if e := r.s.Import(context.Background(), page(one), "relay"); e != nil {
				t.Fatal(e)
			}
			r.s.Close()
			rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: r.now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: r.now.Add(30 * time.Minute).UnixMilli()}, r.root)
			if e != nil {
				t.Fatal(e)
			}
			f := crashFixture{r.path, r.root.Public().(ed25519.PublicKey), r.now, r.cfg.Origins, page(two), rev}
			b, _ := json.Marshal(f)
			path := filepath.Join(t.TempDir(), "crash.json")
			if e = os.WriteFile(path, b, 0600); e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHistoryCrashChild$")
			cmd.Env = append(os.Environ(), "WORLD_TASK_HISTORY_CRASH_FIXTURE="+path, "WORLD_TASK_HISTORY_CRASH_PHASE="+phase)
			log := filepath.Join(t.TempDir(), "child.log")
			out, e := os.Create(log)
			if e != nil {
				t.Fatal(e)
			}
			defer out.Close()
			cmd.Stdout = out
			cmd.Stderr = out
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, e = os.Stat(path + ".ready"); e == nil {
					break
				}
				if time.Now().After(deadline) {
					data, _ := os.ReadFile(log)
					t.Fatal("child failed to reach write window", string(data))
				}
				time.Sleep(10 * time.Millisecond)
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = cmd.Wait()
			r.s, e = Open(r.path, r.cfg)
			if e != nil {
				t.Fatal(e)
			}
			cursor, e := r.s.Cursor("gateway-a")
			want := uint64(1)
			if phase == "renamed" {
				want = 2
			}
			if e != nil || cursor.After != want {
				t.Fatal(cursor, e)
			}
			list, e := r.s.List(context.Background(), "gateway-a", "", 20, "")
			if e != nil || len(list.Events) != int(want) {
				t.Fatal("events and cursor disagree", list, cursor, e)
			}

		})
	}
}
