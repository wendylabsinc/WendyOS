package commands

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
