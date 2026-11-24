package commands

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestInstallOutputPlainWithoutTerminal(t *testing.T) {
	cases := []struct {
		name  string
		write func(*bytes.Buffer)
		want  string
	}{
		{"done", func(b *bytes.Buffer) { installDone(b, "Image written") }, "✓ Image written\n"},
		{"done with detail", func(b *bytes.Buffer) { installDone(b, "Image ready", "(cached)") }, "✓ Image ready (cached)\n"},
		{"result", func(b *bytes.Buffer) { installResult(b, "Installed WendyOS 0.20.2 on Mass-Storage.") }, "✓ Installed WendyOS 0.20.2 on Mass-Storage.\n"},
		{"warn", func(b *bytes.Buffer) { installWarn(b, "x") }, "⚠ x\n"},
		{"note", func(b *bytes.Buffer) { installNote(b, "Fetching available devices...") }, "Fetching available devices...\n"},
		{"header", func(b *bytes.Buffer) {
			installHeader(b, "Jetson Orin Nano", "0.20.2", "/dev/disk4", "500.1 GB, NVMe image")
		},
			"Installing WendyOS 0.20.2 on Jetson Orin Nano → /dev/disk4 (500.1 GB, NVMe image)\n"},
		{"header without detail", func(b *bytes.Buffer) { installHeader(b, "Jetson Orin Nano", "0.20.2", "/dev/disk4", "") },
			"Installing WendyOS 0.20.2 on Jetson Orin Nano → /dev/disk4\n"},
		{"next", func(b *bytes.Buffer) {
			installNext(b, "put the drive in the board, power it on, then run", "wendy discover")
		}, "→ Next: put the drive in the board, power it on, then run wendy discover\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			c.write(&b)
			if got := b.String(); got != c.want {
				t.Fatalf("got %q; want %q", got, c.want)
			}
			if strings.Contains(b.String(), "\x1b") {
				t.Fatalf("output %q has escape codes without a terminal", b.String())
			}
		})
	}
}

func TestInstallImageDetail(t *testing.T) {
	cases := []struct {
		size    int64
		storage string
		want    string
	}{
		{500_107_862_016, "nvme", "500.1 GB, NVMe image"},
		{64_000_000_000, "sd", "64.0 GB, SD image"},
		{0, "sd", "SD image"},
		{64_000_000_000, "", "64.0 GB"},
		{0, "", ""},
	}
	for _, c := range cases {
		if got := installImageDetail(drive{SizeBytes: c.size}, c.storage); got != c.want {
			t.Errorf("installImageDetail(%d, %q) = %q; want %q", c.size, c.storage, got, c.want)
		}
	}
}

func TestFinishLiteInstallWithoutEnrollment(t *testing.T) {
	var b bytes.Buffer
	if err := finishLiteInstall(&b, "1.4.0", nil); err != nil {
		t.Fatal(err)
	}
	want := "\n✓ Flashed Wendy Lite 1.4.0.\n→ Next: the board restarts by itself, then run wendy discover\n"
	if b.String() != want {
		t.Fatalf("got %q; want %q", b.String(), want)
	}
}

// The next step is only true once enrollment, which runs after the flash, is done.
func TestFinishLiteInstallEnrollsBeforeTheNextStep(t *testing.T) {
	var b bytes.Buffer
	err := finishLiteInstall(&b, "1.4.0", func() error { b.WriteString("<enrolling>\n"); return nil })
	flashed, enrolling, next := strings.Index(b.String(), "✓ Flashed"), strings.Index(b.String(), "<enrolling>"), strings.Index(b.String(), "Next:")
	if err != nil || flashed < 0 || enrolling < flashed || next < enrolling {
		t.Fatalf("err %v, output %q; want flashed, then enrollment, then the next step", err, b.String())
	}
}

func TestFinishLiteInstallFailedEnrollmentHasNoNextStep(t *testing.T) {
	var b bytes.Buffer
	failed := errors.New("not enrolled")
	err := finishLiteInstall(&b, "1.4.0", func() error { return failed })
	if !errors.Is(err, failed) || strings.Contains(b.String(), "Next:") {
		t.Fatalf("err %v, output %q; want the enrollment error and no next step", err, b.String())
	}
}

func TestFormatInstallDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		300 * time.Millisecond: "1s",
		45 * time.Second:       "45s",
		134 * time.Second:      "2m 14s",
		63 * time.Minute:       "1h 3m",
	} {
		if got := formatInstallDuration(d); got != want {
			t.Errorf("formatInstallDuration(%v) = %q; want %q", d, got, want)
		}
	}
}

func TestWrittenDetail(t *testing.T) {
	if got, want := writtenDetail(12_500*1024*1024, 134*time.Second), "· 12.2 GiB in 2m 14s"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
	if got, want := writtenDetail(0, 45*time.Second), "· in 45s"; got != want {
		t.Errorf("got %q; want %q without a size when it is unknown", got, want)
	}
}

// Without a terminal (CI, services, a container without -t) there is no bar to
// draw: the work runs directly and the log gets one line saying what it is.
func TestRunProgressWithoutTerminal(t *testing.T) {
	for _, tc := range []struct {
		name              string
		json, interactive bool
	}{{"no terminal", false, false}, {"json with a terminal", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			setInteractive(t, tc.interactive)
			oldJSON := jsonOutput
			t.Cleanup(func() { jsonOutput = oldJSON })
			jsonOutput = tc.json
			ran := false
			var workErr, uiErr error
			out := captureStdout(t, func() {
				workErr, uiErr = runProgress("Writing to /dev/sdz", tui.NewProgress("Writing to /dev/sdz").Transient(),
					func(send func(tui.ProgressUpdateMsg)) error {
						ran = true
						send(tui.ProgressUpdateMsg{Percent: 0.5, Written: 1, Total: 2})
						return errors.New("device gone")
					})
			})
			if !ran || uiErr != nil || workErr == nil || workErr.Error() != "device gone" {
				t.Fatalf("ran %v, work error %v, ui error %v; want the work's own error and no UI", ran, workErr, uiErr)
			}
			if !strings.Contains(out, "Writing to /dev/sdz...") {
				t.Fatalf("output %q; want one line naming the work for the log", out)
			}
		})
	}
}
