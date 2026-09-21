package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func testSnapshot(t *testing.T) Image {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	picture.Set(0, 0, color.RGBA{R: 255, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	return Image{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(data.Bytes())}
}

type snapshotMCP struct {
	image Image
	calls int
}

func (m *snapshotMCP) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{Tools: []mcpgo.Tool{mcpgo.NewTool("camera_snapshot")}}, nil
}
func (m *snapshotMCP) CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m.calls++
	return &mcpgo.CallToolResult{Content: []mcpgo.Content{
		mcpgo.TextContent{Type: "text", Text: "Camera 0 snapshot, 2×2"},
		mcpgo.ImageContent{Type: "image", MIMEType: m.image.MIMEType, Data: m.image.Data},
	}}, nil
}
func (*snapshotMCP) Close() error { return nil }

func TestCameraImageReachesModelAfterApprovalWithoutEnteringTranscript(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow", false: "deny"}[allow], func(t *testing.T) {
			client := &snapshotMCP{image: testSnapshot(t)}
			executor, err := newWorkspaceTools(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer executor.Close()
			executor.mcp = client
			round := 0
			provider := uiProviderFunc(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
				round++
				if round == 1 {
					return Message{ToolCalls: []ToolCall{{ID: "snapshot", Name: "camera_snapshot", Arguments: json.RawMessage(`{}`)}}}, nil
				}
				result := messages[len(messages)-1]
				if result.Role != "tool" || result.ToolCallID != "snapshot" {
					t.Fatalf("lost tool pairing: %+v", result)
				}
				if allow {
					if len(result.Images) != 1 || result.Images[0] != client.image {
						t.Fatal("image was not preserved for the model")
					}
				} else if len(result.Images) != 0 || !strings.Contains(result.Content, "denied") {
					t.Fatal("denied capture returned media")
				}
				return Message{Content: "Finished inspection."}, nil
			})
			engine := NewEngine(provider, executor, "test")
			if err := engine.Turn(context.Background(), "What do you see?", func(event Event) {
				if strings.Contains(event.Text, client.image.Data) {
					t.Fatal("base64 leaked into terminal event")
				}
			}, func(context.Context, ToolCall) (bool, error) { return allow, nil }); err != nil {
				t.Fatal(err)
			}
			if client.calls != map[bool]int{true: 1, false: 0}[allow] {
				t.Fatalf("captures = %d", client.calls)
			}
			if allow {
				copy := engine.Messages()
				copy[len(copy)-2].Images[0].Data = "changed"
				if engine.Messages()[len(copy)-2].Images[0] != client.image {
					t.Fatal("Messages shared image slice")
				}
			}
		})
	}
}

func TestImageValidationRejectsMalformedOrOversizedResults(t *testing.T) {
	valid := testSnapshot(t)
	for name, images := range map[string][]Image{
		"unsupported":    {{MIMEType: "audio/wav", Data: valid.Data}},
		"invalid-base64": {{MIMEType: "image/png", Data: "%%%"}},
		"invalid-image":  {{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("not an image"))}},
		"mismatch":       {{MIMEType: "image/jpeg", Data: valid.Data}},
		"too-large":      {{MIMEType: "image/png", Data: strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)+4)}},
		"too-many":       {valid, valid, valid, valid, valid},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := validateImages(images)
			if err == nil || len(result) != 0 {
				t.Fatalf("invalid image accepted: %v", err)
			}
		})
	}
}

func TestTextLimitDoesNotDropFollowingImageBlock(t *testing.T) {
	valid := testSnapshot(t)
	result, err := mediaToolResult(&mcpgo.CallToolResult{Content: []mcpgo.Content{
		mcpgo.TextContent{Type: "text", Text: strings.Repeat("x", maxToolOutputBytes+100)},
		mcpgo.ImageContent{Type: "image", MIMEType: valid.MIMEType, Data: valid.Data},
	}})
	if err != nil || len(result.Text) > maxToolOutputBytes || len(result.Images) != 1 {
		t.Fatalf("result text=%d, images=%d, err=%v", len(result.Text), len(result.Images), err)
	}
}

func TestImageHistoryExpiresOldAttachmentsButKeepsMetadata(t *testing.T) {
	data := strings.Repeat("A", maxHistoryImageBytes/2)
	engine := &Engine{messages: []Message{
		{Role: "tool", Content: "first snapshot", Images: []Image{{Data: data}}},
		{Role: "tool", Content: "second snapshot", Images: []Image{{Data: data}}},
		{Role: "tool", Content: "third snapshot", Images: []Image{{Data: data}}},
	}}
	engine.trimImageHistory()
	if len(engine.messages[0].Images) != 0 || !strings.Contains(engine.messages[0].Content, "expired") || len(engine.messages[1].Images) != 1 || len(engine.messages[2].Images) != 1 {
		t.Fatal("wrong images expired")
	}
	before := engine.messages[0].Content
	engine.trimImageHistory()
	if engine.messages[0].Content != before {
		t.Fatal("repeated expiry notice")
	}
}

func TestImageOnlyStructuredMirrorCannotLeakBase64IntoText(t *testing.T) {
	valid := testSnapshot(t)
	result, err := mediaToolResult(&mcpgo.CallToolResult{
		Content:           []mcpgo.Content{mcpgo.ImageContent{Type: "image", MIMEType: valid.MIMEType, Data: valid.Data}},
		StructuredContent: map[string]any{"image": map[string]string{"mimeType": valid.MIMEType, "data": valid.Data}},
	})
	if err != nil || len(result.Images) != 1 || strings.Contains(result.Text, valid.Data) {
		t.Fatalf("image result leaked mirrored content: %v", err)
	}
}

func TestCompactSnapshotResultShowsImageInsteadOfJSON(t *testing.T) {
	got := compactToolEntry(chatEntry{kind: "result", text: "[1 image(s) attached for visual inspection.]\n{\"camera_id\":\"0\",\"width\":1280}"})
	if got != "↳ 1 image captured" {
		t.Fatalf("summary=%q", got)
	}
}

func TestImageHistoryAlsoLimitsSmallImageCount(t *testing.T) {
	engine := &Engine{}
	for i := 0; i < maxHistoryImages+2; i++ {
		engine.messages = append(engine.messages, Message{Content: "snapshot", Images: []Image{{Data: "tiny"}}})
	}
	engine.trimImageHistory()
	retained := 0
	for _, message := range engine.messages {
		retained += len(message.Images)
	}
	if retained != maxHistoryImages || len(engine.messages[0].Images) != 0 || len(engine.messages[len(engine.messages)-1].Images) != 1 {
		t.Fatalf("retained=%d", retained)
	}
}
