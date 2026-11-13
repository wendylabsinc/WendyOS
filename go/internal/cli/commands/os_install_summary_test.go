//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

func TestProvisioningSummary(t *testing.T) {
	creds := []wendyconf.WifiCredential{
		{SSID: "Home", Password: "hunter2"},
		{SSID: "Cafe"},
		{SSID: "Off\x1b[31mice", Password: "corp-secret"},
	}
	var b bytes.Buffer
	printProvisioningSummary(&b, "nano-lab", creds, cloudPreEnrolled)
	got := b.String()
	for _, want := range []string{"First boot will apply:", "Device name", "nano-lab", "Home, Cafe (open), Off[31mice", "Cloud", "pre-enrolled"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not contain %q", got, want)
		}
	}
	for _, secret := range []string{"hunter2", "corp-secret", "\x1b[31m"} {
		if strings.Contains(got, secret) {
			t.Errorf("summary %q leaks %q", got, secret)
		}
	}

	b.Reset()
	printProvisioningSummary(&b, "", nil, "")
	got = b.String()
	for _, want := range []string{"auto-generated", "none — reach the board over Ethernet or USB"} {
		if !strings.Contains(got, want) {
			t.Errorf("empty summary %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "Cloud") {
		t.Errorf("summary %q has a Cloud line although enrollment is decided later", got)
	}

	for status, want := range map[cloudStatus]string{cloudNotEnrolled: "not enrolled", cloudWillPreEnroll: "will pre-enroll"} {
		b.Reset()
		printProvisioningSummary(&b, "", nil, status)
		if !strings.Contains(b.String(), want) {
			t.Errorf("summary %q does not say %q", b.String(), want)
		}
	}
}

func TestLogProvisioningSummaryIsPlain(t *testing.T) {
	var b bytes.Buffer
	logProvisioningSummary(&b, "nano-lab", []wendyconf.WifiCredential{{SSID: "Home", Password: "pw"}}, nil)
	got := b.String()
	if !strings.HasPrefix(got, "First boot will apply:\n") || !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("log summary %q: want it to start with the heading and end with a blank line", got)
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "pw\n") {
		t.Fatalf("log summary %q has styling or a password", got)
	}
}

func TestProvisioningWrittenLine(t *testing.T) {
	got := provisioningWrittenLine("nano-lab", []wendyconf.WifiCredential{{SSID: "Home", Password: "hunter2"}}, true)
	want := "Config partition written: name nano-lab · Wi-Fi Home · agent seed · Cloud enrollment"
	if got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
	got = provisioningWrittenLine("", nil, false)
	want = "Config partition written: auto-generated name · no Wi-Fi · agent seed"
	if got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestProvisionConfigWithRetryReportsWhatLanded(t *testing.T) {
	creds := []wendyconf.WifiCredential{{SSID: "Home", Password: "hunter2"}}
	withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error { return nil }, false, nil)
	out, _ := captureCommandStdout(t, func() error {
		_, _, err := provisionConfigWithRetry(context.Background(), drive{}, creds, "nano-lab", nil, true, nil)
		return err
	})
	if !strings.Contains(out, "Config partition written: name nano-lab · Wi-Fi Home · agent seed") || strings.Contains(out, "hunter2") {
		t.Fatalf("output %q; want the written line without the password", out)
	}

	withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return errors.New("no config partition")
	}, false, nil)
	out, _ = captureCommandStdout(t, func() error {
		_, _, err := provisionConfigWithRetry(context.Background(), drive{}, creds, "nano-lab", nil, true, nil)
		return err
	})
	if strings.Contains(out, "Config partition written") {
		t.Fatalf("output %q claims a write that failed", out)
	}
}

func TestConfirmDiskInstallSettlesWiFiBeforeErasing(t *testing.T) {
	d := drive{Name: "SD Card", DevicePath: "/dev/disk4", IsRemovable: true}
	skip := preEnrollOptions{mode: preEnrollSkip}

	t.Run("undecided Wi-Fi fails before the erase question", func(t *testing.T) {
		stubWifiChoice(t, false, "")
		asked := stubInstallConfirms(t, true)
		_, _, _, err := confirmDiskInstall(d, wifiCLIOptions{RequireChoice: true}, "", skip, false, false)
		if !errors.Is(err, errWiFiUndecided) || len(*asked) != 0 {
			t.Fatalf("got %v, asked %v; want errWiFiUndecided before any question", err, *asked)
		}
	})

	t.Run("bad name fails before the erase question", func(t *testing.T) {
		stubWifiChoice(t, false, "")
		asked := stubInstallConfirms(t, true)
		_, _, _, err := confirmDiskInstall(d, wifiCLIOptions{NoWifi: true, RequireChoice: true}, "Lab_1", skip, true, false)
		if err == nil || !strings.Contains(err.Error(), "--device-name") || len(*asked) != 0 {
			t.Fatalf("got %v, asked %v", err, *asked)
		}
	})

	t.Run("summary is shown before the erase question", func(t *testing.T) {
		stubWifiChoice(t, true, "")
		origPrompt := installConfirmPrompt
		t.Cleanup(func() { installConfirmPrompt = origPrompt })
		installConfirmPrompt = func(q string) (bool, error) {
			_, _ = os.Stdout.WriteString("<erase question>\n")
			return true, nil
		}
		var creds []wendyconf.WifiCredential
		var name string
		out, err := captureCommandStdout(t, func() error {
			var err error
			creds, name, _, err = confirmDiskInstall(d, wifiCLIOptions{SSID: "Home", Password: "hunter2", RequireChoice: true}, "nano-lab", skip, false, false)
			return err
		})
		if err != nil || name != "nano-lab" || len(creds) != 1 {
			t.Fatalf("got %+v, %q, %v", creds, name, err)
		}
		summary, question := strings.Index(out, "First boot will apply:"), strings.Index(out, "<erase question>")
		if summary < 0 || question < 0 || summary > question {
			t.Fatalf("output %q: want the summary before the erase question", out)
		}
		if !strings.Contains(out, "not enrolled") {
			t.Fatalf("output %q: want the Cloud line, settled before the summary", out)
		}
		if strings.Contains(out, "hunter2") {
			t.Fatalf("output %q leaks the password", out)
		}
	})

	t.Run("force prints the summary and skips the question", func(t *testing.T) {
		stubWifiChoice(t, false, "")
		asked := stubInstallConfirms(t, false)
		out, err := captureCommandStdout(t, func() error {
			_, _, _, err := confirmDiskInstall(d, wifiCLIOptions{NoWifi: true, RequireChoice: true}, "", skip, true, false)
			return err
		})
		if err != nil || len(*asked) != 0 || !strings.Contains(out, "auto-generated") {
			t.Fatalf("got %v, asked %v, output %q", err, *asked, out)
		}
	})

	t.Run("no terminal and no --force fails before pre-enrollment", func(t *testing.T) {
		stubWifiChoice(t, false, "")
		stubEnrollPrompts(t) // fails the test on any enrollment call
		asked := stubInstallConfirms(t, true)
		out, err := captureCommandStdout(t, func() error {
			_, _, _, err := confirmDiskInstall(d, wifiCLIOptions{NoWifi: true, RequireChoice: true}, "lab-pi", preEnrollOptions{mode: preEnrollForced}, false, false)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "needs confirmation") || len(*asked) != 0 {
			t.Fatalf("got %v, asked %v", err, *asked)
		}
		if strings.Contains(out, "First boot will apply:") {
			t.Fatalf("output %q: printed a summary for an install that cannot go ahead", out)
		}
	})

	t.Run("internal drive refusal comes before pre-enrollment", func(t *testing.T) {
		stubWifiChoice(t, true, "")
		stubEnrollPrompts(t)
		asked := stubInstallConfirms(t, true)
		internal := drive{Name: "Data SSD", DevicePath: "/dev/disk2"}
		_, _, _, err := confirmDiskInstall(internal, wifiCLIOptions{NoWifi: true, RequireChoice: true}, "lab-pi", preEnrollOptions{mode: preEnrollForced}, true, false)
		if err == nil || !strings.Contains(err.Error(), "--yes-overwrite-internal") || len(*asked) != 0 {
			t.Fatalf("got %v, asked %v", err, *asked)
		}
	})
}

// stubDiskPreEnroll logs into one Cloud session and answers pre-enrollment:
// yes, a blank optional name, then lab-box once a name is required. events
// records each question and mint in the order they happen.
func stubDiskPreEnroll(t *testing.T, events *[]string) {
	t.Helper()
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{{CloudGRPC: "prod.example.com:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}}}); err != nil {
		t.Fatal(err)
	}
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) {
		*events = append(*events, "pre-enroll?")
		return true, nil
	}
	promptDeviceName = func(_, _ string, validate tui.ValidateFunc) (string, error) {
		if validate("") == nil {
			*events = append(*events, "name?")
			return "", nil
		}
		*events = append(*events, "required name?")
		return "lab-box", nil
	}
	preEnrollACMEFn = func(_ context.Context, _ *config.AuthConfig, name string) (*acmeEnrollmentBake, error) {
		*events = append(*events, "mint "+name)
		return &acmeEnrollmentBake{DeviceID: "11111111-1111-4111-8111-111111111111", EABKeyID: "eab-id"}, nil
	}
}

// stubEraseQuestion answers the erase question and records it in events and on stdout.
func stubEraseQuestion(t *testing.T, events *[]string, answer bool) {
	t.Helper()
	orig := installConfirmPrompt
	t.Cleanup(func() { installConfirmPrompt = orig })
	installConfirmPrompt = func(string) (bool, error) {
		*events = append(*events, "erase?")
		_, _ = os.Stdout.WriteString("<erase question>\n")
		return answer, nil
	}
}

// A blank name is asked for by the pre-enrollment plan; the summary must show
// that name before the erase question, and nothing may be reserved yet.
func TestConfirmDiskInstallSummaryShowsTheNamePreEnrollmentChose(t *testing.T) {
	var events []string
	stubDiskPreEnroll(t, &events)
	stubWifiChoice(t, true, "")
	stubEraseQuestion(t, &events, true)

	var name string
	var plan *acmePreEnrollPlan
	out, err := captureCommandStdout(t, func() error {
		var err error
		_, name, plan, err = confirmDiskInstall(drive{Name: "SD Card", DevicePath: "/dev/disk4", IsRemovable: true},
			wifiCLIOptions{RequireChoice: true}, "", preEnrollOptions{mode: preEnrollAuto}, false, false)
		return err
	})
	if err != nil || name != "lab-box" || plan == nil {
		t.Fatalf("got name %q, plan %v, %v", name, plan, err)
	}
	if want := []string{"name?", "pre-enroll?", "required name?", "erase?"}; !slices.Equal(events, want) {
		t.Fatalf("events %v; want %v, with nothing minted", events, want)
	}
	summary, erase := strings.Index(out, "First boot will apply:"), strings.Index(out, "<erase question>")
	if summary < 0 || erase < summary {
		t.Fatalf("output %q: want the summary before the erase question", out)
	}
	block := out[summary:erase]
	if !strings.Contains(block, "lab-box") || strings.Contains(block, "auto-generated") || !strings.Contains(block, "will pre-enroll") {
		t.Fatalf("summary %q: want the planned name and enrollment", block)
	}
}

// The plan asks its questions and settles the name without contacting Cloud.
func TestPlanProvisioningReservesNothing(t *testing.T) {
	var events []string
	stubDiskPreEnroll(t, &events)
	setInteractive(t, true)
	serveManifests(t, nil) // any HTTP request fails the test
	preEnrollACMEFn = func(context.Context, *config.AuthConfig, string) (*acmeEnrollmentBake, error) {
		t.Fatal("the plan reserved the device in Cloud")
		return nil, nil
	}
	name := ""
	plan, err := planProvisioning(preEnrollOptions{mode: preEnrollAuto}, &name)
	if err != nil || plan == nil || name != "lab-box" {
		t.Fatalf("got plan %v, name %q, %v", plan, name, err)
	}
}

// settleDiskFixture publishes one image whose block map needs 16 GiB.
func settleDiskFixture(t *testing.T) pickerDevice {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LocalAppData", t.TempDir())
	bmap := fmt.Sprintf(`<bmap version="2.0"><ImageSize>%d</ImageSize><BlockSize>4096</BlockSize>`+
		`<ChecksumType>sha256</ChecksumType><BlockMap><Range chksum="ab">0</Range></BlockMap></bmap>`, int64(16)<<30)
	serveManifests(t, map[string]string{"rpi5/image.img.bmap": bmap})
	return pickerDevice{Name: "Raspberry Pi 5", RawVersion: "1.0.0", Manifest: &deviceManifest{Versions: map[string]deviceVersion{
		"1.0.0": {Path: "rpi5/image.img", BmapPath: "rpi5/image.img.bmap"},
	}}}
}

func settleTestDrive(size int64) drive {
	return drive{Name: "SD Card", DevicePath: "/dev/disk4", IsRemovable: true, SizeBytes: size}
}

func TestSettleDiskInstallRefusesASmallDriveBeforeAnyQuestion(t *testing.T) {
	device := settleDiskFixture(t)
	var events []string
	stubDiskPreEnroll(t, &events)
	_, wifiPrompts := stubWifiChoice(t, true, "")
	stubEraseQuestion(t, &events, true)

	_, err := captureCommandStdout(t, func() error {
		_, err := settleDiskInstall(context.Background(), "raspberry-pi-5", device, "1.0.0", settleTestDrive(8<<30), false, false, "",
			wifiCLIOptions{RequireChoice: true}, "", preEnrollOptions{mode: preEnrollAuto}, false, false)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("err %v; want the drive refused as too small", err)
	}
	if *wifiPrompts != 0 || len(events) != 0 {
		t.Fatalf("asked %d Wi-Fi questions and %v before refusing the drive", *wifiPrompts, events)
	}
}

func TestSettleDiskInstallReservesTheNameOnlyAfterTheEraseConfirm(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(fmt.Sprintf("erase confirmed=%v", confirm), func(t *testing.T) {
			device := settleDiskFixture(t)
			var events []string
			stubDiskPreEnroll(t, &events)
			stubWifiChoice(t, true, "")
			stubEraseQuestion(t, &events, confirm)

			var settled diskInstallSettled
			out, err := captureCommandStdout(t, func() error {
				var err error
				settled, err = settleDiskInstall(context.Background(), "raspberry-pi-5", device, "1.0.0", settleTestDrive(32<<30), false, false, "",
					wifiCLIOptions{RequireChoice: true}, "", preEnrollOptions{mode: preEnrollAuto}, false, false)
				return err
			})
			if !confirm {
				if !errors.Is(err, ErrUserCancelled) || slices.Contains(events, "mint lab-box") {
					t.Fatalf("declined erase: %v, events %v; want a cancel with no name reserved", err, events)
				}
				return
			}
			if want := []string{"name?", "pre-enroll?", "required name?", "erase?", "mint lab-box"}; err != nil || !slices.Equal(events, want) {
				t.Fatalf("err %v, events %v; want %v", err, events, want)
			}
			if settled.name != "lab-box" || !bytes.Contains(settled.provisioningJSON, []byte("11111111-1111-4111-8111-111111111111")) || settled.storage == "" || settled.imgInfo == nil {
				t.Fatalf("settled %+v", settled)
			}
			if !strings.Contains(out, "will pre-enroll") {
				t.Fatalf("output %q: want the planned enrollment in the summary", out)
			}
		})
	}
}

// A mint that fails after the erase confirm ends the same way pre-enrollment
// failures always have: an error without a terminal, a choice with one.
func TestSettleDiskInstallMintFailure(t *testing.T) {
	failMint := func(events *[]string) {
		preEnrollACMEFn = func(context.Context, *config.AuthConfig, string) (*acmeEnrollmentBake, error) {
			*events = append(*events, "mint")
			return nil, errors.New("cloud unavailable")
		}
	}
	settle := func(device pickerDevice, name string, mode preEnrollMode, force bool) (diskInstallSettled, error) {
		var settled diskInstallSettled
		_, err := captureCommandStdout(t, func() error {
			var err error
			settled, err = settleDiskInstall(context.Background(), "raspberry-pi-5", device, "1.0.0", settleTestDrive(32<<30), false, false, "",
				wifiCLIOptions{NoWifi: true, RequireChoice: true}, name, preEnrollOptions{mode: mode}, force, false)
			return err
		})
		return settled, err
	}

	t.Run("forced without a terminal", func(t *testing.T) {
		device := settleDiskFixture(t)
		var events []string
		stubDiskPreEnroll(t, &events)
		stubWifiChoice(t, false, "")
		failMint(&events)
		_, err := settle(device, "lab-pi", preEnrollForced, true)
		if err == nil || !strings.Contains(err.Error(), "--pre-enroll: pre-enrollment failed: cloud unavailable") || !slices.Equal(events, []string{"mint"}) {
			t.Fatalf("err %v, events %v", err, events)
		}
	})
	for _, keepGoing := range []bool{true, false} {
		t.Run(fmt.Sprintf("at a terminal, continue unenrolled=%v", keepGoing), func(t *testing.T) {
			device := settleDiskFixture(t)
			var events []string
			stubDiskPreEnroll(t, &events)
			stubWifiChoice(t, true, "")
			stubEraseQuestion(t, &events, true)
			failMint(&events)
			confirmContinueUnenrolled = func() (bool, error) {
				events = append(events, "continue?")
				return keepGoing, nil
			}
			settled, err := settle(device, "lab-pi", preEnrollAuto, false)
			if want := []string{"pre-enroll?", "erase?", "mint", "continue?"}; !slices.Equal(events, want) {
				t.Fatalf("events %v; want %v", events, want)
			}
			if keepGoing && (err != nil || settled.provisioningJSON != nil || settled.name != "lab-pi") {
				t.Fatalf("continued: %+v, %v; want an unenrolled install", settled, err)
			}
			if !keepGoing && !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("declined: %v; want ErrUserCancelled", err)
			}
		})
	}
}

func TestAlsoOnPromptTerminal(t *testing.T) {
	var log bytes.Buffer
	if w := alsoOnPromptTerminal(&log); w != io.Writer(&log) {
		t.Fatalf("without a prompt output: %v; want the writer itself", w)
	}

	var terminal bytes.Buffer
	t.Cleanup(tui.SetPromptOutput(&terminal))
	_, _ = io.WriteString(alsoOnPromptTerminal(&log), "Target: board\n")
	if log.String() != "Target: board\n" || terminal.String() != "Target: board\n" {
		t.Fatalf("log %q, terminal %q; want the line in both", log.String(), terminal.String())
	}

	// Prompts drawn on stderr: a stderr line must not appear twice.
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Cleanup(tui.SetPromptOutput(f))
	if w := alsoOnPromptTerminal(f); w != io.Writer(f) {
		t.Fatalf("writer that is the prompt output: %v; want it once", w)
	}
}

// With stdout redirected, the summary and erase warning must also be on the
// terminal where the question is answered; stdout keeps them for the log.
func TestDestructiveConfirmsShowTheSummaryWherePromptsAre(t *testing.T) {
	creds := []wendyconf.WifiCredential{{SSID: "Home", Password: "hunter2"}}
	recovery := recoveryFlashConfirm{target: "Jetson AGX Thor (1-2)", warning: []string{"This erases QSPI + internal NVMe on the Thor."}, question: "Flash Jetson AGX Thor (1-2)?"}
	disk := drive{Name: "SD Card", DevicePath: "/dev/disk4", IsRemovable: true}
	installers := map[string]func() error{
		"disk": func() error {
			_, _, _, err := confirmDiskInstall(disk, wifiCLIOptions{SSID: "Home", Password: "hunter2", RequireChoice: true}, "lab-pi", preEnrollOptions{mode: preEnrollSkip}, false, false)
			return err
		},
		"recovery": func() error { return confirmRecoveryFlash(recovery, "lab-pi", creds, false, false) },
	}
	for name, install := range installers {
		for _, redirected := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/redirected=%v", name, redirected), func(t *testing.T) {
				stubWifiChoice(t, true, "")
				asked := stubInstallConfirms(t, true)
				var terminal bytes.Buffer
				if redirected {
					t.Cleanup(tui.SetPromptOutput(&terminal))
				}
				out, err := captureCommandStdout(t, install)
				if err != nil || len(*asked) != 1 {
					t.Fatalf("%v, asked %v", err, *asked)
				}
				want := []string{"First boot will apply:", "lab-pi", "Home"}
				if name == "recovery" {
					want = append(want, "Target:", recovery.target, recovery.warning[0])
				}
				for _, w := range want {
					if n := strings.Count(out, w); n != 1 {
						t.Errorf("stdout has %q %d times; want once:\n%s", w, n, out)
					}
					if redirected && !strings.Contains(terminal.String(), w) {
						t.Errorf("the terminal never shows %q:\n%s", w, terminal.String())
					}
				}
				if strings.Contains(out+terminal.String(), "hunter2") {
					t.Error("the password was printed")
				}
			})
		}
	}
}

func TestConfirmRecoveryFlash(t *testing.T) {
	c := recoveryFlashConfirm{target: "Orin (1-3)", warning: []string{"This erases QSPI."}, question: "Flash Orin (1-3)?"}

	t.Run("declined", func(t *testing.T) {
		setInteractive(t, true)
		asked := stubInstallConfirms(t, false)
		_, err := captureCommandStdout(t, func() error { return confirmRecoveryFlash(c, "", nil, true, false) })
		if !errors.Is(err, ErrUserCancelled) || len(*asked) != 1 || (*asked)[0] != "danger:Flash Orin (1-3)?" {
			t.Fatalf("got %v, asked %v", err, *asked)
		}
	})
	t.Run("force shows the summary without the warning or question", func(t *testing.T) {
		setInteractive(t, false)
		asked := stubInstallConfirms(t, false)
		out, err := captureCommandStdout(t, func() error { return confirmRecoveryFlash(c, "", nil, true, true) })
		if err != nil || len(*asked) != 0 || !strings.Contains(out, "pre-enrolled") || strings.Contains(out, "This erases QSPI.") {
			t.Fatalf("got %v, asked %v, output %q", err, *asked, out)
		}
	})
	t.Run("no terminal and no --force", func(t *testing.T) {
		setInteractive(t, false)
		asked := stubInstallConfirms(t, true)
		_, err := captureCommandStdout(t, func() error { return confirmRecoveryFlash(c, "", nil, false, false) })
		if err == nil || !strings.Contains(err.Error(), "flashing Orin (1-3) needs confirmation") || len(*asked) != 0 {
			t.Fatalf("got %v, asked %v", err, *asked)
		}
	})
}
