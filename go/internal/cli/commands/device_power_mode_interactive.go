package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// powerModeWaitTimeout bounds how long the interactive screen waits for a
// rebooted device to report its new mode.
const powerModeWaitTimeout = 5 * time.Minute

// powerModeWaitInterval is the pause between probes while waiting. A var so
// tests can shrink it.
var powerModeWaitInterval = 3 * time.Second

// Hooks so tests can drive the screen without a terminal or a device.
var (
	pickPowerModeFn = func(title string, items []tui.PickerItem) (string, error) {
		return pickFromItemsWithColumns(title, items, powerModePickerColumns())
	}
	waitPowerModeAppliedFn = waitForPowerModeApplied
)

func powerModePickerColumns() []tui.PickerColumn {
	return []tui.PickerColumn{
		{Title: "ID", MinWidth: 4, Required: true, Value: func(i tui.PickerItem) string { return i.Size }},
		{Title: "Mode", MinWidth: 10, Required: true, Value: func(i tui.PickerItem) string { return i.Name }},
		{Title: "Status", MinWidth: 12, Value: func(i tui.PickerItem) string { return i.Type }},
	}
}

// powerModeItems turns the device's modes into picker rows, in ID order, with
// the current and boot-default modes marked. The row value is the mode name.
func powerModeItems(resp *agentpb.GetPowerModeResponse) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(resp.GetAvailable()))
	for _, m := range resp.GetAvailable() {
		var status string
		isCurrent := m.GetId() == resp.GetCurrent().GetId()
		isBoot := resp.GetBootDefault() != nil && m.GetId() == resp.GetBootDefault().GetId()
		switch {
		case isCurrent && isBoot:
			status = "current, boot default"
		case isCurrent:
			status = "current"
		case isBoot:
			status = "boot default"
		}
		id := strconv.Itoa(int(m.GetId()))
		items = append(items, tui.PickerItem{
			Name:     m.GetName(),
			Type:     status,
			Size:     id,
			SortKey:  fmt.Sprintf("%010d", m.GetId()),
			DedupKey: id,
			Value:    m.GetName(),
		})
	}
	return items
}

func runPowerModeInteractive(cmd *cobra.Command) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	addr := conn.Addr
	if addr == "" {
		addr = conn.Host
	}
	return runPowerModeScreen(ctx, conn.AgentService, addr, conn.DevicePinKey, os.Stdout)
}

// runPowerModeScreen shows the modes, lets the user pick one and sets it,
// asking before a reboot and waiting for the device to return in the new mode.
func runPowerModeScreen(ctx context.Context, client agentpb.WendyAgentServiceClient, host, pinKey string, out io.Writer) error {
	resp, err := client.GetPowerMode(ctx, &agentpb.GetPowerModeRequest{})
	if err != nil {
		return powerModeRPCError("reading the power mode", err)
	}
	current := resp.GetCurrent()

	picked, err := pickPowerModeFn("Power mode: "+powerModeLabel(current)+" — select a mode to set", powerModeItems(resp))
	if err != nil {
		return err
	}
	if picked == current.GetName() {
		fmt.Fprintf(out, "Already in %s.\n", tui.Value(powerModeLabel(current)))
		return nil
	}

	o, err := setPowerMode(ctx, client, picked, false, true, out)
	if err != nil {
		return err
	}
	if !o.rebooting() {
		return printPowerModeSet(out, o, false)
	}

	label := powerModeLabel(o.resp.GetMode())
	fmt.Fprintf(out, "Power mode %s set; the device is rebooting now.\n", tui.Value(label))
	applied, err := waitPowerModeAppliedFn(ctx, host, pinKey, o.resp.GetMode().GetId())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Back online. Power mode is now %s.\n", tui.Value(powerModeLabel(applied)))
	return nil
}

type powerModeProbe func(ctx context.Context) (*agentpb.PowerMode, error)

// pollPowerModeApplied probes until the device reports mode want. It tolerates
// probe errors (the device is rebooting) and a stale reading from before the
// reboot; only the new mode counts.
func pollPowerModeApplied(ctx context.Context, probe powerModeProbe, want int32, interval time.Duration) (*agentpb.PowerMode, error) {
	var last *agentpb.PowerMode
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		cur, err := probe(probeCtx)
		cancel()
		if errors.Is(err, errDeviceIdentityRefused) || errors.Is(err, certs.ErrExpectedDeviceIdentity) {
			return nil, err
		}
		if err == nil {
			last = cur
			if cur.GetId() == want {
				return cur, nil
			}
		}
		select {
		case <-ctx.Done():
			seen := "the device never answered"
			if last != nil {
				seen = "last seen " + powerModeLabel(last)
			}
			return nil, fmt.Errorf("the device did not report the new power mode in time (%s); check with 'wendy device power-mode get'", seen)
		case <-time.After(interval):
		}
	}
}

// waitForPowerModeApplied waits for the device at host to come back reporting
// mode want, behind a spinner on an interactive terminal.
func waitForPowerModeApplied(ctx context.Context, host, pinKey string, want int32) (*agentpb.PowerMode, error) {
	addr := host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = hostPort(host, defaultAgentPort)
	}
	ctx, cancel := context.WithTimeout(ctx, powerModeWaitTimeout)
	defer cancel()

	probe := func(ctx context.Context) (*agentpb.PowerMode, error) {
		conn, _, err := connectWithAutoTLSDiagnosticsKeyed(ctx, addr, pinKey)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		resp, err := conn.AgentService.GetPowerMode(ctx, &agentpb.GetPowerModeRequest{})
		if err != nil {
			return nil, err
		}
		return resp.GetCurrent(), nil
	}

	if !isInteractiveTerminal() {
		return pollPowerModeApplied(ctx, probe, want, powerModeWaitInterval)
	}

	p := tui.NewProgressProgram(tui.NewSpinner("Waiting for the device to come back online..."))
	go func() {
		applied, err := pollPowerModeApplied(ctx, probe, want, powerModeWaitInterval)
		p.Send(tui.SpinnerDoneMsg{Result: applied, Err: err})
	}()
	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("TUI error: %w", err)
	}
	sm, ok := finalModel.(tui.SpinnerModel)
	if !ok || !sm.Done() {
		return nil, ErrUserCancelled
	}
	result, spinErr := sm.Result()
	if spinErr != nil {
		return nil, spinErr
	}
	applied, _ := result.(*agentpb.PowerMode)
	return applied, nil
}
