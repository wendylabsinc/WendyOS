package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// cloudStatus is the summary's Cloud line; empty leaves the line out, as when
// enrollment happens after flashing.
type cloudStatus string

const (
	cloudPreEnrolled   cloudStatus = "pre-enrolled"
	cloudWillPreEnroll cloudStatus = "will pre-enroll"
	cloudNotEnrolled   cloudStatus = "not enrolled"
)

func enrolledStatus(enrolled bool) cloudStatus {
	if enrolled {
		return cloudPreEnrolled
	}
	return cloudNotEnrolled
}

// printProvisioningSummary shows on one line what first boot will apply,
// before anything is erased. Never prints a password.
func printProvisioningSummary(w io.Writer, name string, creds []wendyconf.WifiCredential, cloud cloudStatus) {
	if name == "" {
		name = "auto-generated"
	}
	wifi := "none (Ethernet or USB)"
	if len(creds) > 0 {
		wifi = strings.Join(wifiSummaryNames(creds), ", ")
	}
	parts := []string{"name " + tui.Value(tui.StripControl(name)), "Wi-Fi " + tui.Value(wifi)}
	if cloud != "" {
		parts = append(parts, "Cloud "+tui.Value(string(cloud)))
	}
	fmt.Fprintln(w, installInfoStyle.Render("›")+tui.Dim(" First boot: ")+strings.Join(parts, tui.Dim(" · ")))
}

// alsoOnPromptTerminal adds the prompt terminal to w while prompts are drawn
// there, so what a confirm asks about reaches both the log and the person.
func alsoOnPromptTerminal(w io.Writer) io.Writer {
	p, ok := tui.PromptOutput()
	if f, isFile := p.(*os.File); !ok || (isFile && w == f) {
		return w
	}
	return io.MultiWriter(w, p)
}

// recoveryFlashConfirm is what a recovery installer shows before it flashes.
type recoveryFlashConfirm struct {
	target   string   // the board the person picked
	warning  []string // what the flash erases, shown before the question
	question string
}

// confirmRecoveryFlash shows the target and what first boot will apply, then
// warns and asks before flashing; --force skips only the warning and question.
func confirmRecoveryFlash(c recoveryFlashConfirm, name string, creds []wendyconf.WifiCredential, cloud cloudStatus, summary, force bool) error {
	w := alsoOnPromptTerminal(os.Stdout)
	fmt.Fprintf(w, "\n%s %s\n", tui.Dim("Target:"), tui.Device(c.target))
	if summary {
		printProvisioningSummary(w, name, creds, cloud)
	}
	if force {
		return nil
	}
	fmt.Fprintln(w)
	for _, line := range c.warning {
		fmt.Fprintln(w, line)
	}
	ok, err := confirmInstallStep("flashing "+c.target, c.question, true, force)
	if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
		return ErrUserCancelled
	}
	if err == nil {
		printAnswer(w, c.question, "yes", true)
	}
	return err
}

// logProvisioningSummary is printProvisioningSummary for a flash log header:
// unstyled, and followed by a blank line; a nil enrolled leaves out the Cloud line.
func logProvisioningSummary(w io.Writer, name string, creds []wendyconf.WifiCredential, enrolled *bool) {
	var cloud cloudStatus
	if enrolled != nil {
		cloud = enrolledStatus(*enrolled)
	}
	plain := provisioningSummary(name, creds, cloud, func(s string) string { return s })
	fmt.Fprint(w, strings.TrimPrefix(plain, "\n")+"\n")
}

func provisioningSummary(name string, creds []wendyconf.WifiCredential, cloud cloudStatus, label func(string) string) string {
	var b strings.Builder
	row := func(key, value string) {
		fmt.Fprintf(&b, "  %s %s\n", label(fmt.Sprintf("%-12s", key)), value)
	}
	b.WriteString("\nFirst boot will apply:\n")
	if name == "" {
		row("Device name", "auto-generated")
	} else {
		row("Device name", tui.StripControl(name))
	}
	if len(creds) == 0 {
		row("Wi-Fi", "none — reach the board over Ethernet or USB")
	} else {
		row("Wi-Fi", strings.Join(wifiSummaryNames(creds), ", "))
	}
	if cloud != "" {
		row("Cloud", string(cloud))
	}
	return b.String()
}

// wifiSummaryNames names each network, marking an open one so a forgotten
// password shows before the erase rather than at first boot.
func wifiSummaryNames(creds []wendyconf.WifiCredential) []string {
	names := make([]string, 0, len(creds))
	for _, c := range creds {
		n := tui.StripControl(c.SSID)
		if c.Password == "" {
			n += " (open)"
		}
		names = append(names, n)
	}
	return names
}

// firstBootWrittenDetail names the agent seeded with the first-boot settings.
func firstBootWrittenDetail(agentVersion string) string {
	if agentVersion == "" {
		return ""
	}
	return "· agent " + agentVersion
}
