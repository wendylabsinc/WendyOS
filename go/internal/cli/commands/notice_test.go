package commands

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

// swapNoticeOut sends plainNotice output to w for the rest of the test.
func swapNoticeOut(t *testing.T, w io.Writer) {
	t.Helper()
	prev := noticeOut
	noticeOut = w
	t.Cleanup(func() { noticeOut = prev })
}

func TestPlainNoticeIsOneUnstyledLine(t *testing.T) {
	var out strings.Builder
	swapNoticeOut(t, &out)
	plainNotice("%s", "\x1b[1mUsing\x1b[0m  default\n  device x.")
	if got, want := out.String(), "Using default device x.\n"; got != want {
		t.Errorf("plainNotice wrote %q, want %q", got, want)
	}
	out.Reset()
	plainNotice("%s", "  ")
	if out.Len() != 0 {
		t.Errorf("blank notice wrote %q, want nothing", out.String())
	}
}

func TestReportStaleCertificateInJSONMode(t *testing.T) {
	prev := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = prev })
	var out strings.Builder
	swapNoticeOut(t, &out)

	reportStaleCertificate(errCertExpired)
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "warning: ") || !strings.Contains(got, "wendy auth login") {
		t.Errorf("stale-certificate notice = %q, want one warning line naming 'wendy auth login'", got)
	}
}

func TestMaybeShowNextStep(t *testing.T) {
	newInfo := func() (*cobra.Command, *strings.Builder) {
		root := &cobra.Command{Use: "wendy"}
		device := &cobra.Command{Use: "device"}
		info := &cobra.Command{Use: "info", Run: func(*cobra.Command, []string) {}}
		root.AddCommand(device)
		device.AddCommand(info)
		var human strings.Builder
		info.SetErr(&human)
		return info, &human
	}
	const hint = "Next: run `wendy run` to build and deploy an app to this device.\n"
	clearCI := func(t *testing.T) {
		for _, key := range env.CIEnvVars {
			t.Setenv(key, "")
		}
	}
	prev := jsonOutput
	t.Cleanup(func() { jsonOutput = prev })

	t.Run("JSON mode writes the hint to stderr as a plain line", func(t *testing.T) {
		clearCI(t)
		stubNonInteractive(t)
		jsonOutput = true
		var out strings.Builder
		swapNoticeOut(t, &out)
		cmd, human := newInfo()
		maybeShowNextStep(cmd)
		if out.String() != hint || human.Len() != 0 {
			t.Errorf("notice = %q, human = %q; want the hint once, on the notice stream", out.String(), human.String())
		}
	})
	t.Run("CI stays quiet", func(t *testing.T) {
		clearCI(t)
		t.Setenv("CI", "true")
		jsonOutput = true
		var out strings.Builder
		swapNoticeOut(t, &out)
		cmd, human := newInfo()
		maybeShowNextStep(cmd)
		if out.Len() != 0 || human.Len() != 0 {
			t.Errorf("CI printed %q / %q", out.String(), human.String())
		}
	})
	t.Run("text mode at a terminal is unchanged", func(t *testing.T) {
		clearCI(t)
		stubInteractive(t)
		jsonOutput = false
		var out strings.Builder
		swapNoticeOut(t, &out)
		cmd, human := newInfo()
		maybeShowNextStep(cmd)
		if human.String() != hint || out.Len() != 0 {
			t.Errorf("human = %q, notice = %q; want the hint on the command's stderr", human.String(), out.String())
		}
	})
}
