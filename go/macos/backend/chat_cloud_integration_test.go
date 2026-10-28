package backend

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

// Explicit opt-in: this test sends a short inference request to a real device.
func TestCloudChatIntegration(t *testing.T) {
	selector := os.Getenv("WENDY_TEST_CHAT_DEVICE")
	model := os.Getenv("WENDY_TEST_CHAT_MODEL")
	if selector == "" || model == "" {
		t.Skip("set WENDY_TEST_CHAT_DEVICE and WENDY_TEST_CHAT_MODEL for a live Cloud check")
	}
	if !strings.HasPrefix(selector, "cloud://") {
		t.Fatal("integration check requires an explicit Cloud selector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := commands.DesktopConnect(ctx, selector)
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = conn
	b.session = 1
	b.deviceSelector = selector
	b.life, b.stop = context.WithCancel(ctx)
	defer b.Shutdown()
	models, err := b.ChatModels(1, 11434)
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(0)
	for _, item := range models {
		if item.ID == model && item.UnavailableReason == "" {
			port = item.Port
		}
	}
	if port == 0 {
		t.Fatalf("selected model was not reachable through ChatModels: %+v", models)
	}
	transport, err := deviceChatTransport(conn, port)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	provider, err := chat.DeviceProvider(model, transport)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	reply, err := provider.Stream(ctx, []chat.Message{{Role: "user", Content: "Reply with just the word hello."}}, nil, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(reply.Content) == "" || streamed.Len() == 0 {
		t.Fatal("model returned no streamed reply")
	}
	t.Logf("Discovered %d models and received a streamed reply: %s", len(models), reply.Content)
	if binary := os.Getenv("WENDY_TEST_CHAT_CLI"); binary != "" {
		b.chatToolsFactory = func(ctx context.Context, selector string) (desktopChatTools, error) {
			return chat.NewMCPTools(ctx, binary, selector)
		}
		if err := b.StartChat(1, 1, port, model, []chat.Message{{Role: "user", Content: "Reply with just the word hello. No device actions are needed."}}); err != nil {
			t.Fatal(err)
		}
		for {
			snapshot, err := b.PollChat(1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Done {
				if snapshot.Error != "" || strings.TrimSpace(snapshot.Text) == "" {
					t.Fatalf("desktop chat failed: %+v", snapshot)
				}
				t.Logf("Desktop chat with Wendy MCP tools completed: %s", snapshot.Text)
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}
