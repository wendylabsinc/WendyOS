package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestPromptTunnelMapping(t *testing.T) {
	for _, tt := range []struct {
		name          string
		input         string
		cancelCtx     bool
		local, remote uint32
		udp           bool
		cancelled     bool
	}{
		{name: "TCP", input: "8080:3000\r", local: 8080, remote: 3000},
		{name: "UDP", input: "9000:9001/udp\r", local: 9000, remote: 9001, udp: true},
		{name: "single port", input: "8080\r", local: 8080, remote: 8080},
		{name: "retry empty input", input: "\r8080\r", local: 8080, remote: 8080},
		{name: "retry invalid port", input: "65536\r\x158080:3000\r", local: 8080, remote: 3000},
		{name: "retry invalid protocol", input: "8080/sctp\r\x158080/udp\r", local: 8080, remote: 8080, udp: true},
		{name: "ctrl+c", input: "\x03", cancelled: true},
		{name: "signal", cancelCtx: true, cancelled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tt.cancelCtx {
				cancel()
			}
			cmd := newCloudTunnelCmd()
			cmd.SetContext(ctx)
			cmd.SetIn(strings.NewReader(tt.input))
			cmd.SetOut(io.Discard)
			gotLocal, gotRemote, gotUDP, err := promptTunnelMapping(cmd)
			if tt.cancelled {
				// ErrUserCancelled exits 0 without printing, like the device picker.
				if !errors.Is(err, ErrUserCancelled) {
					t.Fatalf("got %v, want ErrUserCancelled", err)
				}
				return
			}
			if err != nil || gotLocal != tt.local || gotRemote != tt.remote || gotUDP != tt.udp {
				t.Fatalf("got (%d, %d, %v, %v), want (%d, %d, %v, nil)", gotLocal, gotRemote, gotUDP, err, tt.local, tt.remote, tt.udp)
			}
		})
	}
}

func TestValidateTunnelMappingAsksForEmptyInput(t *testing.T) {
	if err := validateTunnelMapping(""); err == nil || !strings.Contains(err.Error(), "enter a port mapping") {
		t.Fatalf("got %v, want a request for a mapping", err)
	}
	if err := validateTunnelMapping("8080/udp"); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
}

func TestCloudTunnelCommandMissingAndExtraArgs(t *testing.T) {
	oldInteractive, oldJSON := isInteractiveTerminalFn, jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn, jsonOutput = oldInteractive, oldJSON })
	for _, tt := range []struct {
		name        string
		args        []string
		interactive bool
		json        bool
		want        string
		usage       bool
	}{
		{name: "missing", args: []string{}, want: "provide a port mapping", usage: true},
		{name: "missing JSON", args: []string{}, interactive: true, json: true, want: "provide a port mapping", usage: true},
		{name: "invalid", args: []string{"0"}, want: "invalid port"},
		{name: "extra", args: []string{"8080", "9000"}, want: "accepts at most 1 arg(s)", usage: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isInteractiveTerminalFn = func() bool { return tt.interactive }
			jsonOutput = tt.json
			cmd := newCloudTunnelCmd()
			markUsageErrors(cmd) // as the root command does for the whole tree
			cmd.SetArgs(tt.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
			// A usage error exits 2 with code cli_usage.
			if tt.usage && !IsUsageError(err) {
				t.Fatalf("%v is not a usage error", err)
			}
		})
	}
}

func TestCloudTunnelResolvesSessionBeforePrompting(t *testing.T) {
	isolateLoginConfig(t)
	oldInteractive, oldJSON := isInteractiveTerminalFn, jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn, jsonOutput = oldInteractive, oldJSON })
	isInteractiveTerminalFn = func() bool { return true }
	jsonOutput = false

	var out bytes.Buffer
	cmd := newCloudTunnelCmd()
	cmd.SetArgs(nil)
	cmd.SetIn(strings.NewReader("8080:3000\r"))
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); !errors.Is(err, config.ErrNotLoggedIn) {
		t.Fatalf("got %v, want ErrNotLoggedIn", err)
	}
	if strings.Contains(out.String(), "Port mapping") {
		t.Fatal("prompted for a mapping before reporting the missing login")
	}
}
