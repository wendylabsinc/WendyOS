package commands

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// promptTerminalFn finds where to draw prompts when stdout is not a terminal;
// close releases what it opened. Stubbed in tests.
var promptTerminalFn = findPromptTerminal

func findPromptTerminal() (*os.File, func(), bool) {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		return os.Stderr, func() {}, true
	}
	return openPromptTTY()
}

// startInstallSession decides once whether a person can answer this install's
// prompts, so every gate in the run agrees; end restores what it changed.
func startInstallSession(cmd *cobra.Command) (end func()) {
	prevInteractive := isInteractiveTerminalFn
	prevJSON := jsonOutput
	prevCanPromptForOrg := canPromptForOrgFn

	interactive := prevInteractive()
	restorePrompt := func() {}
	closeTerminal := func() {}
	// `wendy install | tee log` typed at a terminal: stdout is piped but a
	// person is there, so prompts move to a terminal they can see.
	if !interactive && humanPresentFn() {
		if f, closeFn, ok := promptTerminalFn(); ok {
			interactive = true
			restorePrompt = tui.SetPromptOutput(f)
			closeTerminal = closeFn
		}
	}

	isInteractiveTerminalFn = func() bool { return interactive }
	// root turned JSON on only because stdout is piped; an explicit --json stays.
	if interactive && !cmd.Root().PersistentFlags().Changed("json") {
		jsonOutput = false
	}
	canPromptForOrgFn = func() bool { return interactive && prevCanPromptForOrg() }

	return func() {
		isInteractiveTerminalFn = prevInteractive
		jsonOutput = prevJSON
		canPromptForOrgFn = prevCanPromptForOrg
		restorePrompt()
		closeTerminal()
	}
}

// Install confirm prompts; stubbed in tests.
var (
	installConfirmPrompt       = func(q string) (bool, error) { return tui.Confirm(q) }
	installDangerConfirmPrompt = func(q string) (bool, error) { return tui.ConfirmNoDefaultDanger(q) }
)

// confirmInstallStep asks question unless --force skips it; with nobody to answer
// it fails naming what needed the answer. danger asks in red with no default.
func confirmInstallStep(what, question string, danger, force bool) (bool, error) {
	if force {
		return true, nil
	}
	if err := checkInstallConfirmable(what, force); err != nil {
		return false, err
	}
	if danger {
		return installDangerConfirmPrompt(question)
	}
	return installConfirmPrompt(question)
}

// checkInstallConfirmable fails before work that a later unanswerable confirm
// would waste, such as pre-enrollment minting a credential.
func checkInstallConfirmable(what string, force bool) error {
	if force || isInteractiveTerminal() {
		return nil
	}
	return usageErrorf("%s needs confirmation: run this in a terminal, or pass --force", what)
}

// requireInstallChoice fails when a picker would have no one to answer it.
// hint says which flag, or what else, makes the choice unnecessary.
func requireInstallChoice(what, hint string) error {
	if isInteractiveTerminal() {
		return nil
	}
	return usageErrorf("%s needs a terminal: %s", what, hint)
}

// requireRecoveryChoice guards the multi-board recovery pickers: flashing the
// wrong board is unrecoverable, so a run nobody watches never guesses.
func requireRecoveryChoice(count int, label string) error {
	if isInteractiveTerminal() {
		return nil
	}
	return fmt.Errorf("%d %s boards are in recovery mode: connect only the one to flash, or run this in a terminal to choose", count, label)
}

// usbAccessDeniedLine is what a person sees when the board is attached but
// this login may not open it (macOS, or Linux without a udev grant for this
// user). sudo is the one step that works on every distro; the device names
// what the briefing called it.
func usbAccessDeniedLine(device string) string {
	return tui.WarningMessage("Your user account can't open the " + device + " over USB; run the same command again with sudo.")
}

// promptRescan asks to rescan after a fix the user makes outside wendy and
// reports whether they chose to quit. Without a person to answer it quits.
func promptRescan(question string) (quit bool) {
	if !isInteractiveTerminal() {
		return true
	}
	fmt.Fprint(tui.PromptWriter(), question)
	return readQuit()
}

// readLineFn reads one line of typed input; stubbed in tests.
var readLineFn = func() string {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}
