package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestConnectToAgentNonInteractiveWithoutDevice(t *testing.T) {
	stubInteractive(t)
	t.Setenv("WENDY_AGENT_SOCKET", "")
	setTempConfig(t, &config.Config{})
	originalDevice, originalJSON := deviceFlag, jsonOutput
	deviceFlag, jsonOutput = "", false
	t.Cleanup(func() { deviceFlag, jsonOutput = originalDevice, originalJSON })

	conn, err := connectToAgent(context.Background(), NonInteractive())
	if conn != nil {
		conn.Close()
		t.Fatal("expected no connection without a specified device")
	}
	if err == nil || !strings.Contains(err.Error(), "no device specified; use --device") {
		t.Fatalf("error = %v, want an explicit target error without opening a picker", err)
	}
}
