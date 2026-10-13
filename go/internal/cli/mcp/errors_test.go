package mcp

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrResult_StructuredAndText(t *testing.T) {
	r := errResult(errCodeNotConnected, "no device connected")
	if !r.IsError {
		t.Fatal("expected IsError=true")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map structured content, got %T", r.StructuredContent)
	}
	if sc["error_code"] != "NOT_CONNECTED" {
		t.Errorf("error_code = %v, want NOT_CONNECTED", sc["error_code"])
	}
	text := toolResultText(t, r)
	if text != "[NOT_CONNECTED] no device connected" {
		t.Errorf("text = %q", text)
	}
}

func TestCodeFromGRPC(t *testing.T) {
	cases := map[codes.Code]errorCode{
		codes.Unavailable:      errCodeDeviceUnreachable,
		codes.PermissionDenied: errCodeEntitlementDenied,
		codes.NotFound:         errCodeNotFound,
		codes.InvalidArgument:  errCodeInvalidArgument,
		codes.Unimplemented:    errCodeUnsupported,
		codes.Internal:         errCodeInternal,
	}
	for c, want := range cases {
		if got := codeFromGRPC(status.Error(c, "x")); got != want {
			t.Errorf("codeFromGRPC(%v) = %v, want %v", c, got, want)
		}
	}
}

// fakeHintRewrite stands in for a plugin-managed CLI that is not on PATH.
func fakeHintRewrite(t *testing.T) {
	t.Helper()
	old := rewriteCLIHints
	rewriteCLIHints = func(msg string) string {
		return strings.NewReplacer("'wendy ", "'/home/u/.wendy/bin/wendy ", "`wendy ", "`/home/u/.wendy/bin/wendy ").Replace(msg)
	}
	t.Cleanup(func() { rewriteCLIHints = old })
}

func TestErrResult_RewritesCLIHints(t *testing.T) {
	fakeHintRewrite(t)
	r := errResult(errCodeInternal, "auth entry has no certificates; re-run 'wendy auth login'")
	want := "auth entry has no certificates; re-run '/home/u/.wendy/bin/wendy auth login'"
	if got := structuredMap(t, r)["message"]; got != want {
		t.Errorf("structured message = %q, want %q", got, want)
	}
	if text := toolResultText(t, r); !strings.Contains(text, want) {
		t.Errorf("text fallback = %q, want it to contain %q", text, want)
	}
}

func TestROS2Error_RewritesCLIHints(t *testing.T) {
	fakeHintRewrite(t)
	r := ros2Error(status.Error(codes.Unimplemented, "unknown service"))
	if text := toolResultText(t, r); !strings.Contains(text, "`/home/u/.wendy/bin/wendy device update`") {
		t.Errorf("ros2Error text = %q, want the rewritten update hint", text)
	}
}
