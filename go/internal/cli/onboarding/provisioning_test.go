package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testPassword = "hunter2-correct-horse"

func provisionedJob(t *testing.T, p *Provisioning) (JobStore, JobHooks, *Job) {
	t.Helper()
	s, h, _ := jobFixture(t)
	j, err := s.Start(context.Background(), StartOptions{Options: Options{DeviceType: "raspberry-pi-5", Drive: "/dev/test"}, Provisioning: p}, h)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, j
}

func homeProvisioning() *Provisioning {
	return &Provisioning{DeviceName: "nano-lab", WiFi: []WiFiNetwork{{SSID: "Home", Password: testPassword, Priority: 2}, {SSID: "Cafe"}}}
}

// assertNoPassword fails when any file in the job root other than the
// private input file, or any given text, contains the password.
func assertNoPassword(t *testing.T, s JobStore, texts ...string) {
	t.Helper()
	for i, text := range texts {
		if strings.Contains(text, testPassword) {
			t.Fatalf("text %d leaks the password: %s", i, text)
		}
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".provisioning.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), testPassword) {
			t.Fatalf("%s leaks the password", e.Name())
		}
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStartRecordsAbsentProvisioningAsExplicitChoice(t *testing.T) {
	s, _, j := provisionedJob(t, nil)
	want := ProvisioningSummary{AutoName: true, NoWiFi: true}
	if j.Provisioning == nil || !j.Provisioning.equal(&want) {
		t.Fatalf("summary %+v, want %+v", j.Provisioning, want)
	}
	if !strings.Contains(marshal(t, j), `"provisioning":{"auto_name":true,"no_wifi":true}`) {
		t.Fatalf("job JSON does not state the choice: %s", marshal(t, j))
	}
	text := strings.Join(j.Instructions, " ")
	for _, want := range []string{"written as published", "auto-generated device name", "no Wi-Fi", "Ethernet"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instructions omit %q: %s", want, text)
		}
	}
	if s.hasProvisioning(j.ID) {
		t.Fatal("stored an input file for a job without input")
	}
	got, err := s.ReadProvisioning(j)
	if err != nil || got.DeviceName != "" || len(got.WiFi) != 0 || !got.NoWiFi {
		t.Fatalf("worker input %+v, %v", got, err)
	}
}

func TestStartKeepsWiFiPasswordsOutOfEveryResult(t *testing.T) {
	opts := StartOptions{Options: Options{DeviceType: "raspberry-pi-5", Drive: "/dev/test"}, Provisioning: homeProvisioning()}
	s, h, _ := jobFixture(t)
	j, err := s.Start(context.Background(), opts, h)
	if err != nil {
		t.Fatal(err)
	}
	want := ProvisioningSummary{DeviceName: "nano-lab", WiFi: []string{"Home", "Cafe"}, OpenWiFi: []string{"Cafe"}}
	if !j.Provisioning.equal(&want) {
		t.Fatalf("summary %+v", j.Provisioning)
	}
	text := strings.Join(j.Instructions, " ")
	if !strings.Contains(text, "the device name nano-lab") || !strings.Contains(text, "Wi-Fi Home, Cafe (open, no password)") || strings.Contains(text, "Ethernet") {
		t.Fatalf("instructions: %s", text)
	}
	// The StartOptions round trip must not carry the password either.
	var back StartOptions
	if err := json.Unmarshal([]byte(marshal(t, opts)), &back); err != nil {
		t.Fatal(err)
	}
	formatted := fmt.Sprintf("%v %+v %#v %v %+v %#v", opts, opts, opts, *opts.Provisioning, opts.Provisioning.WiFi, opts.Provisioning.WiFi[0])
	assertNoPassword(t, s, marshal(t, j), marshal(t, opts), marshal(t, back), marshal(t, opts.Provisioning), formatted)
	if back.Provisioning != nil {
		t.Fatal("StartOptions JSON carried provisioning")
	}
	path, _ := s.provisioningPath(j.ID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("input file mode %o", info.Mode().Perm())
	}
	got, err := s.ReadProvisioning(j)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceName != "nano-lab" || len(got.WiFi) != 2 || got.WiFi[0].Password != testPassword || got.WiFi[0].Priority != 2 || got.WiFi[1].Password != "" {
		t.Fatalf("worker input lost data: %+v", got)
	}
	// Probe and authorization results also stay clean.
	j = queueJob(t, s, h, j)
	text = strings.Join(j.Instructions, " ")
	assertNoPassword(t, s, marshal(t, j))
	status, err := s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPassword(t, s, marshal(t, status), text)
}

func TestStartValidatesProvisioningBeforePlanning(t *testing.T) {
	long := strings.Repeat("s", 33)
	for name, p := range map[string]*Provisioning{
		"bad name":            {DeviceName: "Nano"},
		"no wifi with wifi":   {NoWiFi: true, WiFi: []WiFiNetwork{{SSID: "Home", Password: testPassword}}},
		"empty ssid":          {WiFi: []WiFiNetwork{{SSID: "  ", Password: testPassword}}},
		"ssid newline":        {WiFi: []WiFiNetwork{{SSID: "Ho\nme", Password: testPassword}}},
		"long ssid":           {WiFi: []WiFiNetwork{{SSID: long, Password: testPassword}}},
		"password newline":    {WiFi: []WiFiNetwork{{SSID: "Home", Password: testPassword + "\n[device]"}}},
		"password spaces":     {WiFi: []WiFiNetwork{{SSID: "Home", Password: " " + testPassword}}},
		"security control":    {WiFi: []WiFiNetwork{{SSID: "Home", Password: testPassword, Security: "wp\x1ba2"}}},
		"second network bad":  {WiFi: []WiFiNetwork{{SSID: "Home", Password: testPassword}, {SSID: ""}}},
		"name with injection": {DeviceName: "lab\n[wifi]"},
	} {
		t.Run(name, func(t *testing.T) {
			s := JobStore{Root: filepath.Join(t.TempDir(), "jobs")}
			planned := false
			h := JobHooks{Plan: func(context.Context, Options) (*Plan, error) { planned = true; return nil, errors.New("unreachable") }}
			_, err := s.Start(context.Background(), StartOptions{Options: Options{DeviceType: "raspberry-pi-5"}, Provisioning: p}, h)
			if err == nil {
				t.Fatal("accepted invalid first-boot input")
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("error leaks the password: %v", err)
			}
			if planned {
				t.Fatal("planned before validating input")
			}
			if _, statErr := os.Stat(s.Root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("created the job store for invalid input")
			}
		})
	}
}

func TestStartNormalizesProvisioning(t *testing.T) {
	_, _, j := provisionedJob(t, &Provisioning{DeviceName: " nano-lab ", WiFi: []WiFiNetwork{{SSID: " Home ", Password: testPassword, Security: " WPA2 "}}})
	if j.Provisioning.DeviceName != "nano-lab" || j.Provisioning.WiFi[0] != "Home" || j.Provisioning.AutoName || j.Provisioning.NoWiFi {
		t.Fatalf("summary %+v", j.Provisioning)
	}
	_, _, j = provisionedJob(t, &Provisioning{NoWiFi: true})
	if !j.Provisioning.NoWiFi || !j.Provisioning.AutoName || j.Provisioning.Requested() {
		t.Fatalf("summary %+v", j.Provisioning)
	}
}

func TestRunConsumesProvisioningOnEveryOutcome(t *testing.T) {
	for name, execute := range map[string]func(context.Context, *Job, Checkpoint) error{
		"success": func(_ context.Context, _ *Job, c Checkpoint) error { return c("writing", true) },
		"write failure": func(_ context.Context, _ *Job, c Checkpoint) error {
			_ = c("writing", true)
			return errors.New("USB disconnected")
		},
		"before write": func(context.Context, *Job, Checkpoint) error { return errors.New("download failed") },
		"config failure": func(_ context.Context, _ *Job, c Checkpoint) error {
			_ = c("writing", true)
			return &ProvisioningError{Err: errors.New("no config partition")}
		},
		"eject failure": func(_ context.Context, _ *Job, c Checkpoint) error {
			_ = c("writing", true)
			return &ProvisioningError{Err: errors.New("disk4 is still mounted"), Configured: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, h, j := provisionedJob(t, homeProvisioning())
			j = queueJob(t, s, h, j)
			read := false
			_ = s.Run(context.Background(), j.ID, func(ctx context.Context, j *Job, c Checkpoint) error {
				p, err := s.ReadProvisioning(j)
				if err != nil || p.WiFi[0].Password != testPassword {
					t.Fatalf("worker could not read its input: %v", err)
				}
				read = true
				return execute(ctx, j, c)
			})
			if !read {
				t.Fatal("worker did not run")
			}
			if s.hasProvisioning(j.ID) {
				t.Fatal("input file survived the worker run")
			}
			status, err := s.Status(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertNoPassword(t, s, marshal(t, status))
		})
	}
	t.Run("host busy", func(t *testing.T) {
		s, h, j := provisionedJob(t, homeProvisioning())
		j = queueJob(t, s, h, j)
		host, err := s.newLock(filepath.Join(s.Root, "host-install.lock"))
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Lock(); err != nil {
			t.Fatal(err)
		}
		defer host.Unlock()
		if err := s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { return nil }); err == nil {
			t.Fatal("ran while the host was busy")
		}
		if s.hasProvisioning(j.ID) {
			t.Fatal("input file survived the refused run")
		}
	})
}

func TestPrewriteFailureWithDiscardedInputNeedsNewJob(t *testing.T) {
	s, h, j := provisionedJob(t, homeProvisioning())
	j = queueJob(t, s, h, j)
	if err := s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { return errors.New("download failed") }); err == nil {
		t.Fatal("missing failure")
	}
	j, err := s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobWaitingTarget || !strings.Contains(strings.Join(j.Instructions, " "), "Create a new installation job") {
		t.Fatalf("job does not say to start again: %+v", j)
	}
	h.Launch = func(context.Context, *Job) error { t.Fatal("launched without its first-boot input"); return nil }
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h); !errors.Is(err, ErrProvisioningUnavailable) {
		t.Fatalf("probe of a job without its input: %v", err)
	}
	// A job that never needed input retries as before.
	s, h, j = provisionedJob(t, &Provisioning{NoWiFi: true})
	j = queueJob(t, s, h, j)
	_ = s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { return errors.New("download failed") })
	queueJob(t, s, h, j)
}

func TestAuthorizationRefusesJobWhoseInputExpired(t *testing.T) {
	s, h, j := provisionedJob(t, homeProvisioning())
	j, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h)
	if err != nil || j.State != JobAwaitingErase {
		t.Fatalf("probe: %v %+v", err, j)
	}
	if !strings.Contains(strings.Join(j.Instructions, " "), "Wi-Fi Home, Cafe (open, no password)") {
		t.Fatalf("erase confirmation hides first-boot choices: %v", j.Instructions)
	}
	s.removeProvisioning(j.ID)
	h.Launch = func(context.Context, *Job) error { t.Fatal("launched without its first-boot input"); return nil }
	_, err = s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: j.Target.Fingerprint}, h)
	if !errors.Is(err, ErrProvisioningUnavailable) || !strings.Contains(err.Error(), "start a new job") {
		t.Fatalf("authorized a job without its input: %v", err)
	}
}

func TestProvisioningErrorExplainsWhatTheTargetHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"config", fmt.Errorf("provisioning: %w", &ProvisioningError{Err: errors.New("no config partition")}), []string{"written completely", "first-boot configuration was not", "new installation job", "Ethernet"}},
		{"eject", &ProvisioningError{Err: errors.New("disk4 is still mounted"), Configured: true}, []string{"could not be ejected", "Finder", "joins Wi-Fi Home, Cafe (open, no password)", "the device named nano-lab"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h, j := provisionedJob(t, homeProvisioning())
			j = queueJob(t, s, h, j)
			_ = s.Run(context.Background(), j.ID, func(_ context.Context, _ *Job, c Checkpoint) error {
				if err := c("writing", true); err != nil {
					return err
				}
				return tc.err
			})
			j, err := s.Status(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(j.Instructions, " ")
			if j.State != JobInterrupted || strings.Contains(text, "did not report success") {
				t.Fatalf("reads as an unknown write: %s %s", j.State, text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) && !strings.Contains(j.Error, want) {
					t.Fatalf("missing %q in %q / %q", want, text, j.Error)
				}
			}
		})
	}
}

func TestBootInstructionsFollowFirstBootChoices(t *testing.T) {
	media := &Plan{Method: "removable-media"}
	wifi := strings.Join(bootInstructions(&Job{Plan: media, Provisioning: &ProvisioningSummary{DeviceName: "nano-lab", WiFi: []string{"Home"}}}), " ")
	if strings.Contains(wifi, "Ethernet") || !strings.Contains(wifi, "joins Wi-Fi Home") || !strings.Contains(wifi, "the device named nano-lab") {
		t.Fatalf("Wi-Fi boot instructions: %s", wifi)
	}
	// Old job records have no summary: no Wi-Fi was seeded.
	none := strings.Join(bootInstructions(&Job{Plan: media}), " ")
	if !strings.Contains(none, "Connect Ethernet") || !strings.Contains(none, "this device") {
		t.Fatalf("Ethernet boot instructions: %s", none)
	}
}

func TestStartSweepsStaleProvisioningInput(t *testing.T) {
	s, h, j := provisionedJob(t, homeProvisioning())
	fresh, _ := s.provisioningPath(j.ID)
	stale := filepath.Join(s.Root, strings.Repeat("b", 32)+".provisioning.json")
	staleTemp := filepath.Join(s.Root, ".provisioning-123")
	for _, p := range []string{stale, staleTemp} {
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-25 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Start(context.Background(), StartOptions{Options: Options{DeviceType: "raspberry-pi-5"}}, h); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{stale, staleTemp} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale %s survived", filepath.Base(p))
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("swept a fresh input file")
	}
}

func TestReadProvisioningRefusesMismatchedOrDamagedInput(t *testing.T) {
	s, _, j := provisionedJob(t, homeProvisioning())
	path, _ := s.provisioningPath(j.ID)
	other := homeProvisioning()
	other.WiFi = other.WiFi[:1]
	if err := s.writeProvisioning(j.ID, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadProvisioning(j); err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("accepted input that differs from the job: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"wifi":[{"ssid":"Home","password":"`+testPassword+`"`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadProvisioning(j); err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("damaged input: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadProvisioning(j); !errors.Is(err, ErrProvisioningUnavailable) {
		t.Fatalf("missing input: %v", err)
	}
}

func TestReconcileAfterWriteDropsProvisioningInput(t *testing.T) {
	s, _, j := provisionedJob(t, homeProvisioning())
	now := time.Now()
	j.State, j.WriteStartedAt = JobRunning, &now
	if err := s.save(j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Status(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	if s.hasProvisioning(j.ID) {
		t.Fatal("input survived a crashed worker past the write boundary")
	}
}

func TestExpiredProvisioningInputIsNeverUsed(t *testing.T) {
	s, h, j := provisionedJob(t, homeProvisioning())
	path, _ := s.provisioningPath(j.ID)
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadProvisioning(j); !errors.Is(err, ErrProvisioningUnavailable) {
		t.Fatalf("worker read expired input: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired input stayed on disk")
	}
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h); !errors.Is(err, ErrProvisioningUnavailable) {
		t.Fatalf("probed a job whose input expired: %v", err)
	}
}

func TestJobsWithFirstBootInputHaveIDsOlderBuildsReject(t *testing.T) {
	older := regexp.MustCompile(`^[a-f0-9]{32}$`)
	_, _, j := provisionedJob(t, homeProvisioning())
	if older.MatchString(j.ID) || !jobIDPattern.MatchString(j.ID) {
		t.Fatalf("job with first-boot input has ID %q", j.ID)
	}
	_, _, plain := provisionedJob(t, nil)
	if !older.MatchString(plain.ID) {
		t.Fatalf("job without first-boot input changed its ID format: %q", plain.ID)
	}
}

func TestDroppedProvisioningRecordIsRefused(t *testing.T) {
	s, h, j := provisionedJob(t, homeProvisioning())
	// An older build that rewrites the job keeps no record of the input.
	j.Provisioning = nil
	if err := s.save(j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadProvisioning(j); !errors.Is(err, errProvisioningDropped) {
		t.Fatalf("worker would install without the requested input: %v", err)
	}
	h.Launch = func(context.Context, *Job) error { t.Fatal("launched without its first-boot input"); return nil }
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h); !errors.Is(err, errProvisioningDropped) {
		t.Fatalf("probed a job that lost its first-boot input: %v", err)
	}
}
