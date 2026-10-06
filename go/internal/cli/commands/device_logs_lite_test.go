package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"testing"
)

type testConsoleProvider struct {
	providers.DeviceProvider
	calls int
	err   error
}

func (p *testConsoleProvider) StreamConsole(ctx context.Context, d models.ExternalDevice, f func(liteclient.ConsoleChunk) error) error {
	p.calls++
	if err := f(liteclient.ConsoleChunk{Data: []byte("PKI ready\n")}); err != nil {
		return err
	}
	return p.err
}
func TestLiteDeviceLogs(t *testing.T) {
	old := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = old }()
	p := &testConsoleProvider{}
	target := &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "wendy-lite"}, Provider: p}
	cmd := newDeviceLogsCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := runLiteDeviceLogs(cmd, target, ""); err != nil {
		t.Fatal(err)
	}
	if out.String() != "PKI ready\n" || p.calls != 1 {
		t.Fatal("console not streamed")
	}
	p.err = errors.New("connection lost")
	if err := runLiteDeviceLogs(cmd, target, ""); !errors.Is(err, p.err) {
		t.Fatal("lost connection hidden")
	}
	for _, flag := range []string{"app", "service", "tail", "no-follow", "level", "min-severity"} {
		cmd := newDeviceLogsCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		app := ""
		if flag == "app" {
			app = "camera"
		} else {
			value := "1"
			if flag == "level" {
				value = "error"
			}
			if flag == "no-follow" {
				value = "true"
			}
			if err := cmd.Flags().Set(flag, value); err != nil {
				t.Fatal(err)
			}
		}
		calls := p.calls
		if err := runLiteDeviceLogs(cmd, target, app); err == nil || p.calls != calls {
			t.Fatalf("unsupported %s reached board", flag)
		}
	}
}
func TestLiteConsoleOutput(t *testing.T) {
	cmd := &cobra.Command{}
	var out, diag bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	chunk := liteclient.ConsoleChunk{Data: []byte("partial\nline"), Stderr: true, Gap: true}
	if err := writeLiteConsoleChunk(cmd, chunk, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != string(chunk.Data) || diag.Len() == 0 {
		t.Fatal("lost bytes or gap notice")
	}
	out.Reset()
	diag.Reset()
	if err := writeLiteConsoleChunk(cmd, chunk, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data        string
		Stderr, Gap bool
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data != string(chunk.Data) || !got.Stderr || !got.Gap || diag.Len() != 0 {
		t.Fatal("invalid JSON log")
	}
}
