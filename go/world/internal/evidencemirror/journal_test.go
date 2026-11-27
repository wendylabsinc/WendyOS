package evidencemirror

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixturePlan() Plan {
	now := time.Now()
	src := Endpoint{Tenant: "tenant", Site: "site", Domain: "simulation", Audience: "source", URL: "http://127.0.0.1:10001", Principal: "reader", KeySHA256: strings.Repeat("a", 64)}
	dst := src
	dst.URL = "http://127.0.0.1:10002"
	dst.Audience = "destination"
	dst.Principal = "writer"
	return Plan{Version: 1, ID: "inspection-copy", Source: src, Destination: dst, Reference: Reference{SHA256: strings.Repeat("b", 64), Size: 100, ContentType: "image/png"}, PreparedUnixMS: now.UnixMilli(), RetainUntilUnixMS: now.Add(time.Minute).UnixMilli()}
}
func TestImmutablePrivatePlanAndPersistedExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan")
	p := fixturePlan()
	j, e := Create(path, p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Open(path); e == nil {
		t.Fatal("concurrent writer admitted")
	}
	s := j.Snapshot()
	s.Plan.RetainUntilUnixMS++
	if e = j.Update(s, time.Now()); e == nil {
		t.Fatal("expiry changed")
	}
	s = j.Snapshot()
	s.DestinationReceived = 50
	s.Phase = "uploading"
	if e = j.Update(s, time.Now()); e != nil {
		t.Fatal(e)
	}
	s = j.Snapshot()
	s.DestinationReceived = 1
	if e = j.Update(s, time.Now()); e == nil {
		t.Fatal("progress regressed")
	}
	if _, e = j.Status(time.UnixMilli(p.RetainUntilUnixMS)); e != nil {
		t.Fatal(e)
	}
	j.Close()
	j, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer j.Close()
	if j.Snapshot().Phase != "expired" {
		t.Fatal(j.Snapshot())
	}
	if _, e = j.Status(time.UnixMilli(p.PreparedUnixMS)); e == nil {
		t.Fatal("clock rollback resurrected plan")
	}
	raw, e := os.ReadFile(filepath.Join(path, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), "private_key") || strings.Contains(string(raw), "credentials") {
		t.Fatal("secret/config material in journal")
	}
}
func TestCorruptionAndDescriptorReplacementFailClosed(t *testing.T) {
	for _, kind := range []string{"checksum", "symlink", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "plan")
			j, e := Create(path, fixturePlan())
			if e != nil {
				t.Fatal(e)
			}
			if kind == "directory" {
				os.Rename(path, path+"-old")
				os.Mkdir(path, 0700)
				if e = j.Update(j.Snapshot(), time.Now()); e == nil {
					t.Fatal("replaced root accepted")
				}
				j.Close()
				return
			}
			j.Close()
			file := filepath.Join(path, "state.json")
			switch kind {
			case "checksum":
				os.WriteFile(file, []byte(`{"sha256":"wrong","state":{}}`), 0600)
			case "symlink":
				os.Rename(file, file+"-old")
				os.Symlink(file+"-old", file)
			case "hardlink":
				os.Link(file, file+"-alias")
			}
			if x, e := Open(path); e == nil {
				x.Close()
				t.Fatal("unsafe/corrupt journal accepted")
			}
		})
	}
}
func TestMirrorJournalCrashChild(t *testing.T) {
	path := os.Getenv("MIRROR_CRASH_PATH")
	if path == "" {
		t.Skip("child only")
	}
	j, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer j.Close()
	writeHook = func(phase string) {
		if phase == os.Getenv("MIRROR_CRASH_PHASE") {
			os.WriteFile(path+".ready", []byte(phase), 0600)
			for {
				time.Sleep(time.Hour)
			}
		}
	}
	s := j.Snapshot()
	s.Phase = "uploading"
	s.DestinationReceived = 50
	if e = j.Update(s, time.Now()); e != nil {
		t.Fatal(e)
	}
	t.Fatal("parent should kill child")
}
func TestChildKillAtomicProgress(t *testing.T) {
	for _, phase := range []string{"synced-temp", "renamed"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plan")
			j, e := Create(path, fixturePlan())
			if e != nil {
				t.Fatal(e)
			}
			j.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestMirrorJournalCrashChild$")
			cmd.Env = append(os.Environ(), "MIRROR_CRASH_PATH="+path, "MIRROR_CRASH_PHASE="+phase)
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			until := time.Now().Add(5 * time.Second)
			for {
				if _, e = os.Stat(path + ".ready"); e == nil {
					break
				}
				if time.Now().After(until) {
					t.Fatal("child did not reach boundary")
				}
				time.Sleep(10 * time.Millisecond)
			}
			cmd.Process.Kill()
			cmd.Wait()
			j, e = Open(path)
			if e != nil {
				t.Fatal(e)
			}
			defer j.Close()
			want := int64(0)
			if phase == "renamed" {
				want = 50
			}
			if j.Snapshot().DestinationReceived != want {
				b, _ := json.Marshal(j.Snapshot())
				t.Fatal(string(b))
			}
		})
	}
}
