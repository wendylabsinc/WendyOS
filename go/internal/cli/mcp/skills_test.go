package mcp

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type skillTestReply struct {
	ID     json.RawMessage            `json:"id"`
	Result json.RawMessage            `json:"result"`
	Error  *mcpgo.JSONRPCErrorDetails `json:"error"`
}

type skillExchange func([]byte) []byte

func stdioSkillExchange(t *testing.T, srv *server.MCPServer, ctx context.Context) skillExchange {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	in, clientIn := io.Pipe()
	clientOut, out := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveStdioStreams(ctx, srv, in, out) }()
	t.Cleanup(func() {
		cancel()
		_ = clientIn.Close()
		_ = clientOut.Close()
		_ = in.Close()
		_ = out.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("stdio did not shut down")
		}
	})
	reader := bufio.NewReader(clientOut)
	return func(raw []byte) []byte {
		t.Helper()
		if _, err := clientIn.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
}

func skillCall(t *testing.T, exchange skillExchange, method string, params any) skillTestReply {
	t.Helper()
	// Preserve numeric IDs beyond JavaScript's integer precision as well.
	const id = "9007199254740993"
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	var reply skillTestReply
	response := exchange(raw)
	if err := json.Unmarshal(response, &reply); err != nil {
		t.Fatalf("decode %s: %v", response, err)
	}
	// The SDK handles IDs for standard methods; the extension must preserve them.
	if strings.HasPrefix(method, "skills/") && string(reply.ID) != id {
		t.Fatalf("changed request ID: %s", reply.ID)
	}
	return reply
}

// Exercise the same import flow used by Scan Tools over the actual transports,
// with no device connector or external skill installation available.
func TestSkillsImportOverTransports(t *testing.T) {
	for _, transport := range []string{"cli-stdio", "gateway-stdio", "gateway-http"} {
		t.Run(transport, func(t *testing.T) {
			var exchange skillExchange
			if transport == "cli-stdio" {
				srv, err := New(&config.Config{}, nil).newProtocolServer()
				if err != nil {
					t.Fatal(err)
				}
				exchange = stdioSkillExchange(t, srv, context.Background())
			} else {
				g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
					t.Error("skill import connected to a device")
					return nil, fmt.Errorf("unexpected connection")
				})
				if err != nil {
					t.Fatal(err)
				}
				if transport == "gateway-stdio" {
					ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
					exchange = stdioSkillExchange(t, g.protocol, ctx)
				} else {
					h := gatewayTestHTTP(t, g)
					exchange = func(raw []byte) []byte {
						t.Helper()
						req, _ := http.NewRequest(http.MethodPost, h.URL+"/mcp", bytes.NewReader(raw))
						req.Header.Set("Authorization", "Bearer "+gatewayTestEnv("ALICE_TOKEN"))
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Accept", "application/json, text/event-stream")
						resp, err := h.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						data, err := io.ReadAll(resp.Body)
						if err != nil || resp.StatusCode != http.StatusOK {
							t.Fatalf("HTTP %d: %s, %v", resp.StatusCode, data, err)
						}
						return data
					}
				}
			}
			verifySkillImport(t, exchange)
		})
	}
}

func verifySkillImport(t *testing.T, exchange skillExchange) {
	t.Helper()
	call := func(method string, params any, into any) {
		t.Helper()
		reply := skillCall(t, exchange, method, params)
		if reply.Error != nil {
			t.Fatalf("%s failed: %+v", method, reply.Error)
		}
		if err := json.Unmarshal(reply.Result, into); err != nil {
			t.Fatal(err)
		}
	}
	var initialized mcpgo.InitializeResult
	call("initialize", map[string]any{"protocolVersion": mcpgo.LATEST_PROTOCOL_VERSION, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "skill-import", "version": "1"}}, &initialized)
	if _, ok := initialized.Capabilities.Extensions[skillsExtension]; !ok {
		t.Fatal("missing capabilities.extensions skill declaration")
	}
	if _, ok := initialized.Capabilities.Experimental[skillsExtension]; ok {
		t.Fatal("used the obsolete experimental declaration")
	}
	var listed struct {
		Skills     []skillEntry `json:"skills"`
		NextCursor string       `json:"nextCursor"`
	}
	call("skills/list", map[string]any{}, &listed)
	if len(listed.Skills) != 1 || listed.NextCursor != "" || listed.Skills[0].Frontmatter["name"] != "wendy" {
		t.Fatalf("unexpected catalog: %+v", listed)
	}
	entry := listed.Skills[0]
	var got struct {
		Skill skillEntry `json:"skill"`
	}
	call("skills/get", map[string]any{"uri": entry.URI}, &got)
	if !reflect.DeepEqual(got.Skill, entry) {
		t.Fatal("skills/get disagrees with skills/list")
	}
	files := map[string][]byte{}
	for _, resource := range entry.Resources {
		var read struct {
			Contents []struct {
				URI  string  `json:"uri"`
				Text *string `json:"text"`
				Blob *string `json:"blob"`
			} `json:"contents"`
		}
		call("resources/read", map[string]any{"uri": resource.URI}, &read)
		if len(read.Contents) != 1 || read.Contents[0].URI != resource.URI {
			t.Fatalf("wrong resource identity: %+v", read)
		}
		item := read.Contents[0]
		if (item.Text == nil) == (item.Blob == nil) {
			t.Fatal("resource must contain text or a blob")
		}
		var data []byte
		if item.Text != nil {
			data = []byte(*item.Text)
		} else {
			var err error
			data, err = base64.StdEncoding.DecodeString(*item.Blob)
			if err != nil {
				t.Fatal(err)
			}
		}
		if resource.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256(data)) {
			t.Fatalf("digest mismatch for %s", resource.URI)
		}
		p := strings.TrimPrefix(resource.URI, skillBaseURI)
		if !strings.HasPrefix(resource.URI, skillBaseURI) || !fs.ValidPath(p) || files[p] != nil {
			t.Fatalf("unsafe or duplicate path %s", resource.URI)
		}
		files[p] = data
	}
	fm, _, err := skillFrontmatter(files["SKILL.md"])
	if err != nil || !reflect.DeepEqual(fm, entry.Frontmatter) {
		t.Fatalf("frontmatter differs: %v", err)
	}
	names, err := assets.EndUserSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := map[string]bool{"SKILL.md": true}
	for _, name := range names {
		if !bytes.Contains(files["SKILL.md"], []byte("(references/skills/"+name+"/SKILL.md)")) {
			t.Fatalf("workflow missing from index: %s", name)
		}
		err := fs.WalkDir(assets.FS, "skills/"+name, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			want, err := assets.FS.ReadFile(p)
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(p, "skills/")
			key := "references/skills/" + rel
			wantFiles[key] = true
			if !bytes.Equal(files[key], want) {
				t.Errorf("lost source bytes: %s", p)
			}
			if name == "wendy" && path.Base(p) != "SKILL.md" {
				key := strings.TrimPrefix(rel, "wendy/")
				wantFiles[key] = true
				if !bytes.Equal(files[key], want) {
					t.Errorf("lost root reference: %s", p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) != len(wantFiles) {
		t.Fatal("catalog includes files outside the end-user group")
	}
	// Existing sibling links must resolve inside the imported package.
	linked := path.Join("references/skills/wendy-device-debug", "../wendy-device-ops/SKILL.md")
	if files[linked] == nil {
		t.Fatal("broken sibling skill link")
	}
	var archive bytes.Buffer
	zipper := zip.NewWriter(&archive)
	total := 0
	for p, data := range files {
		limit := 1 << 20
		if p == "SKILL.md" {
			limit = 256 << 10
		}
		if len(data) > limit {
			t.Fatalf("resource exceeds import limit: %s", p)
		}
		total += len(data)
		w, err := zipper.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	if len(files) > 100 || total > 5<<20 || archive.Len() > 8<<20 {
		t.Fatal("bundle exceeds import limits")
	}
	t.Logf("import verified: %d files, %d bytes, %d-byte ZIP", len(files), total, archive.Len())

	for _, tc := range []struct {
		method string
		params any
	}{
		{"skills/list", map[string]any{"cursor": "stale"}},
		{"skills/list", map[string]any{"cursor": 1}},
		{"skills/list", map[string]any{"cursor": nil}},
		{"skills/list", []string{}},
		{"skills/get", map[string]any{}},
		{"skills/get", map[string]any{"uri": 1}},
		{"skills/get", map[string]any{"uri": skillBaseURI + "references/skills/wendy/SKILL.md"}},
		{"skills/get", map[string]any{"uri": "file:///etc/passwd"}},
	} {
		reply := skillCall(t, exchange, tc.method, tc.params)
		if reply.Error == nil || reply.Error.Code != mcpgo.INVALID_PARAMS {
			t.Fatalf("accepted invalid request: %+v", tc)
		}
	}
	for _, uri := range []string{skillBaseURI + "../wendy-contributing/SKILL.md", skillBaseURI + "%2e%2e/SKILL.md", "file:///etc/passwd"} {
		if reply := skillCall(t, exchange, "resources/read", map[string]any{"uri": uri}); reply.Error == nil {
			t.Fatalf("read unknown resource %s", uri)
		}
	}
	var pong map[string]any
	call("ping", map[string]any{}, &pong)
	var tools struct {
		Tools []mcpgo.Tool `json:"tools"`
	}
	call("tools/list", map[string]any{}, &tools)
	if len(tools.Tools) == 0 {
		t.Fatal("skill adapter broke tool discovery")
	}
}

func TestSkillFrontmatterPreservesAllFields(t *testing.T) {
	data := []byte("---\nname: example\ndescription: |\n  Multiple lines\n  preserved here.\nlicense: MIT\ndisable-model-invocation: true\nmetadata:\n  version: 2\nallowed-tools:\n  - resources/read\n---\n\nBody\n")
	fm, offset, err := skillFrontmatter(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(fm) != 6 || fm["license"] != "MIT" || fm["disable-model-invocation"] != true || fm["description"] != "Multiple lines\npreserved here.\n" || string(data[offset:]) != "\nBody\n" {
		t.Fatalf("lost frontmatter: %+v", fm)
	}
	if fm["metadata"].(map[string]any)["version"] != float64(2) {
		t.Fatal("lost nested metadata")
	}
}

func TestGatewaySkillsRequireAuthentication(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, fmt.Errorf("offline") })
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	for _, method := range []string{"skills/list", "skills/get", "resources/read"} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"uri": skillBaseURI + "SKILL.md"}})
		req, _ := http.NewRequest(http.MethodPost, h.URL+"/mcp", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s returned %d", method, resp.StatusCode)
		}
	}
}
