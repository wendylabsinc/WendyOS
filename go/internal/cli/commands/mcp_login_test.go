package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// auth_login in `wendy mcp serve` signs in to the default Wendy Cloud and
// never writes to stdout, the MCP protocol stream.
func TestMCPLoginStarter_DefaultCloudAndCleanStdout(t *testing.T) {
	isolateLoginConfig(t)
	stubIssueLegacyCertificate(t, nil, errors.New("must not be called"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var url string
	var err error
	out := captureStdout(t, func() {
		session, startErr := mcpLoginStarter(ctx)
		err = startErr
		if startErr != nil {
			return
		}
		url = session.URL()
		cancel()
		select {
		case <-session.Done():
		case <-time.After(5 * time.Second):
			t.Error("session did not end after cancel")
		}
	})
	if err != nil {
		t.Fatalf("mcpLoginStarter: %v", err)
	}
	if out != "" {
		t.Fatalf("mcpLoginStarter wrote to stdout: %q", out)
	}
	if !strings.HasPrefix(url, defaultCloudDashboard+"/cli-auth?redirect_uri=") {
		t.Fatalf("URL = %q, want the default dashboard", url)
	}
}
