package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func budgetRig(t *testing.T, quota, image int64) *retentionRig {
	return retentionSetup(t, func(c *Config) {
		c.QuotaBytes = quota
		c.MaxBlobBytes = quota
		c.ModalityBudgets = map[string]ModalityBudget{"image": {QuotaBytes: image}}
	})
}
func budgetPut(t *testing.T, r *retentionRig, body, kind, class string) PutResponse {
	t.Helper()
	q := request([]byte(body))
	q.ContentType = kind
	q.RetentionClass = class
	p, err := r.s.Put("capture", q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func budgetUsage(t *testing.T, s *Store) Usage {
	t.Helper()
	u, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Validate(); err != nil {
		t.Fatalf("invalid server accounting %+v: %v", u, err)
	}
	return u
}
func modalityCount(t *testing.T, u Usage, name string) ModalityUsage {
	t.Helper()
	for _, m := range u.Modalities {
		if m.Modality == name {
			return m
		}
	}
	t.Fatal("missing modality", name)
	return ModalityUsage{}
}

func TestModalityPressureEvictsOnlyThatModality(t *testing.T) {
	r := budgetRig(t, 20, 6)
	audio := budgetPut(t, r, "aaaa", "audio/wav", RetentionRolling)
	first := budgetPut(t, r, "111", "image/png", RetentionRolling)
	second := budgetPut(t, r, "222", "image/jpeg", RetentionRolling)
	r.reopen(t)
	fresh := budgetPut(t, r, "3333", "image/png", RetentionRolling)
	available(t, r.s, audio, true)
	available(t, r.s, first, false)
	available(t, r.s, second, false)
	available(t, r.s, fresh, true)
	u := budgetUsage(t, r.s)
	if u.Totals.AllocatedBytes != 8 || u.Totals.EvictableBytes != 8 || modalityCount(t, u, "image").AllocatedBytes != 4 || modalityCount(t, u, "audio").AllocatedBytes != 4 {
		t.Fatal(u)
	}
}

func TestModalityCeilingsDoNotReserveGlobalSpace(t *testing.T) {
	r := budgetRig(t, 10, 8)
	audio := budgetPut(t, r, "aaaa", "audio/wav", RetentionRolling)
	image := budgetPut(t, r, "1111", "image/png", RetentionRolling)
	budgetPut(t, r, "2222", "image/png", RetentionRolling)
	available(t, r.s, audio, false)
	available(t, r.s, image, true)
	if u := budgetUsage(t, r.s); u.Totals.AllocatedBytes != 8 || u.LimitSemantics != "ceilings" {
		t.Fatal(u)
	}
}

func TestModalityRefusalPlansWithoutCollateralDeletion(t *testing.T) {
	for _, protection := range []string{"protected", "partial", "pin", "task", "package"} {
		t.Run(protection, func(t *testing.T) {
			r := budgetRig(t, 10, 6)
			audio := budgetPut(t, r, "aaaa", "audio/wav", RetentionRolling)
			q := request([]byte("1111"))
			q.ContentType = "image/png"
			q.RetentionClass = RetentionRolling
			if protection == "protected" {
				q.RetentionClass = RetentionProtected
			}
			if protection == "partial" {
				q.Data = []byte("1")
			}
			protected, err := r.s.Put("capture", q)
			if err != nil {
				t.Fatal(err)
			}
			until := r.now.Add(30 * time.Minute).UnixMilli()
			if protection == "pin" {
				_, err = r.s.Pin("investigator", PinRequest{ID: "case", SHA256: []string{protected.SHA256}, RetainUntilUnixMS: until})
			}
			if protection == "task" || protection == "package" {
				_, err = r.s.ReplaceHold(protection, "executor", "case", references(protected), until)
			}
			if err != nil {
				t.Fatal(err)
			}
			rolling := budgetPut(t, r, "2", "image/jpeg", RetentionRolling)
			before := budgetUsage(t, r.s)
			q = request([]byte("333"))
			q.ContentType = "image/png"
			q.RetentionClass = RetentionRolling
			if _, err = r.s.Put("capture", q); !errors.Is(err, ErrQuota) {
				t.Fatal("expected quota refusal", err)
			}
			after := budgetUsage(t, r.s)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed accounted data", before, after)
			}
			available(t, r.s, audio, true)
			available(t, r.s, rolling, true)
			m := modalityCount(t, after, "image")
			if protection == "partial" && (m.IncompleteBytes != 4 || m.CommittedBytes != 2 || m.EvictableBytes != 1) {
				t.Fatal(m)
			}
			if protection == "pin" || protection == "task" || protection == "package" {
				if m.PinnedBytes != 4 || m.EvictableBytes != 1 {
					t.Fatal(m)
				}
			}
			r.reopen(t)
			if got := budgetUsage(t, r.s); !reflect.DeepEqual(after, got) {
				t.Fatal("restart accounting changed", got)
			}
		})
	}
}

func TestModalityRetentionIsFixedAtAllocation(t *testing.T) {
	r := retentionSetup(t, func(c *Config) {
		c.ModalityBudgets = map[string]ModalityBudget{"image": {MaxRetentionMS: 2000}, "audio": {MaxRetentionMS: 5000}}
	})
	image := budgetPut(t, r, "image", "image/png", RetentionProtected)
	audio := budgetPut(t, r, "audio", "audio/wav", RetentionRolling)
	data := budgetPut(t, r, "data", "application/json", RetentionProtected)
	if image.RetainUntilUnixMS != r.now.Add(2*time.Second).UnixMilli() || audio.RetainUntilUnixMS != r.now.Add(5*time.Second).UnixMilli() || data.RetainUntilUnixMS != r.now.Add(time.Hour).UnixMilli() {
		t.Fatal(image, audio, data)
	}
	q := request([]byte("new"))
	q.ContentType = "image/png"
	q.RetainUntilUnixMS = r.now.Add(3 * time.Second).UnixMilli()
	if _, err := r.s.Put("capture", q); err == nil {
		t.Fatal("explicit retention exceeded image cap")
	}
	r.now = r.now.Add(500 * time.Millisecond)
	r.cfg.ModalityBudgets["image"] = ModalityBudget{MaxRetentionMS: 1000}
	r.reopen(t)
	q = request([]byte("image"))
	q.ContentType = "image/png"
	q.Data = nil
	if got, err := r.s.Put("capture", q); err != nil || got != image {
		t.Fatal("retry changed existing expiry", got, err)
	}
	q.ContentType = "audio/wav"
	if _, err := r.s.Put("capture", q); err == nil {
		t.Fatal("content type changed to escape policy")
	}
	fresh := budgetPut(t, r, "fresh", "image/png", RetentionProtected)
	if fresh.RetainUntilUnixMS != r.now.Add(time.Second).UnixMilli() {
		t.Fatal(fresh)
	}
	r.now = r.now.Add(1600 * time.Millisecond)
	available(t, r.s, image, false)
	available(t, r.s, fresh, false)
	available(t, r.s, audio, true)
}

func TestModalityBudgetConfigurationIsBoundedAndDetached(t *testing.T) {
	for _, policy := range []map[string]ModalityBudget{{"unknown": {}}, {"image": {QuotaBytes: -1}}, {"image": {QuotaBytes: 11}}, {"audio": {MaxRetentionMS: 999}}, {"video": {MaxRetentionMS: 3600001}}} {
		dir := filepath.Join(t.TempDir(), "absent")
		if s, err := Open(dir, Config{Tenant: "t", Site: "s", QuotaBytes: 10, MaxBlobBytes: 10, MaxRetention: time.Hour, ModalityBudgets: policy}); err == nil {
			s.Close()
			t.Fatal("bad policy accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid policy created store")
		}
	}
	r := budgetRig(t, 10, 4)
	r.cfg.ModalityBudgets["image"] = ModalityBudget{QuotaBytes: 10}
	if u := budgetUsage(t, r.s); modalityCount(t, u, "image").QuotaBytes != 4 {
		t.Fatal("caller mutated policy")
	}
	q := request([]byte("12345"))
	q.ContentType = "image/png"
	if _, err := r.s.Put("capture", q); !errors.Is(err, ErrQuota) {
		t.Fatal("oversized modality allocation", err)
	}
}

func TestRestartLowerModalityCapsPlansAllBeforeDeletion(t *testing.T) {
	r := budgetRig(t, 20, 10)
	audio := budgetPut(t, r, "aaaa", "audio/wav", RetentionRolling)
	protected := budgetPut(t, r, "iiii", "image/png", RetentionProtected)
	r.s.Close()
	r.cfg.ModalityBudgets = map[string]ModalityBudget{"audio": {QuotaBytes: 1}, "image": {QuotaBytes: 1}}
	if s, err := Open(r.dir, r.cfg); !errors.Is(err, ErrQuota) {
		if s != nil {
			s.Close()
		}
		t.Fatal(err)
	}
	r.cfg.ModalityBudgets = nil
	r.reopen(t)
	available(t, r.s, audio, true)
	available(t, r.s, protected, true)
	// A satisfiable cap reduction only evicts its own eligible rolling objects.
	r.s.Close()
	r.cfg.ModalityBudgets = map[string]ModalityBudget{"audio": {QuotaBytes: 1}}
	r.reopen(t)
	available(t, r.s, audio, false)
	available(t, r.s, protected, true)
}

func TestModalityCrashRecovery(t *testing.T) {
	if dir := os.Getenv("EVIDENCE_BUDGET_CRASH_DIR"); dir != "" {
		now := time.Unix(1700000000, 0)
		cfg := config(&now)
		cfg.QuotaBytes = 30
		cfg.MaxBlobBytes = 30
		cfg.ModalityBudgets = map[string]ModalityBudget{"image": {QuotaBytes: 6}}
		s, err := Open(dir, cfg)
		if err != nil {
			panic(err)
		}
		q := request([]byte("abcdefgh"))
		q.ContentType = "audio/wav"
		q.Data = q.Data[:3]
		q.RetentionClass = RetentionRolling
		if _, err = s.Put("capture", q); err != nil {
			panic(err)
		}
		if os.Getenv("EVIDENCE_BUDGET_CRASH_PHASE") == "evict" {
			for _, body := range []string{"111", "222"} {
				q = request([]byte(body))
				q.ContentType = "image/png"
				q.RetentionClass = RetentionRolling
				if _, err = s.Put("capture", q); err != nil {
					panic(err)
				}
			}
			s.removeHook = func(string) error { os.Exit(73); return nil }
			q = request([]byte("3333"))
			q.ContentType = "image/png"
			q.RetentionClass = RetentionRolling
			s.Put("capture", q)
			panic("eviction hook not reached")
		}
		os.Exit(73)
	}
	for _, phase := range []string{"partial", "evict"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestModalityCrashRecovery$")
			cmd.Env = append(os.Environ(), "EVIDENCE_BUDGET_CRASH_DIR="+dir, "EVIDENCE_BUDGET_CRASH_PHASE="+phase)
			raw, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child %v %s", err, raw)
			}
			now := time.Unix(1700000000, 0)
			cfg := config(&now)
			cfg.QuotaBytes = 30
			cfg.MaxBlobBytes = 30
			cfg.ModalityBudgets = map[string]ModalityBudget{"image": {QuotaBytes: 6}}
			s, err := Open(dir, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			u := budgetUsage(t, s)
			audio := modalityCount(t, u, "audio")
			if audio.AllocatedBytes != 8 || audio.CommittedBytes != 3 || audio.IncompleteBytes != 8 {
				t.Fatal(audio)
			}
			if phase == "evict" {
				if image := modalityCount(t, u, "image"); image.AllocatedBytes != 3 {
					t.Fatal("deleted manifest resurrected", image)
				}
			}
			q := request([]byte("abcdefgh"))
			q.ContentType = "audio/wav"
			q.RetentionClass = RetentionRolling
			q.Offset = 3
			q.Data = q.Data[3:]
			p, err := s.Put("capture", q)
			if err != nil || !p.Complete {
				t.Fatal(p, err)
			}
			got, err := s.Get(GetRequest{SHA256: p.SHA256, Length: 8})
			if err != nil || !bytes.Equal(got.Data, []byte("abcdefgh")) {
				t.Fatal(got, err)
			}
			raw, _ = json.Marshal(u)
			t.Log(string(raw))
		})
	}
}

func TestNoModalityPolicyPreservesLegacyVictimOrder(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.QuotaBytes = 10; c.MaxBlobBytes = 10 })
	audio := budgetPut(t, r, "a", "audio/wav", RetentionRolling)
	image := budgetPut(t, r, "12345678", "image/png", RetentionRolling)
	budgetPut(t, r, "xyz", "image/png", RetentionRolling)
	available(t, r.s, audio, false)
	available(t, r.s, image, false)
}
