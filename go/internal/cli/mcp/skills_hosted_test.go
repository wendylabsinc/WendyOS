package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestHostedSkillCatalogKeepsLocalWorkflowsSeparate(t *testing.T) {
	s := New(&config.Config{}, nil)
	local, err := s.skillCatalog()
	if err != nil {
		t.Fatal(err)
	}
	s.relay = true
	hosted, err := s.skillCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if local == hosted || bytes.Equal(local.files[skillBaseURI+"SKILL.md"], hosted.files[skillBaseURI+"SKILL.md"]) {
		t.Fatal("hosted and local skills share the same content")
	}
	names, err := assets.EndUserSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		uri := skillBaseURI + "references/skills/" + name + "/SKILL.md"
		if local.files[uri] == nil {
			t.Errorf("local skill bundle lost %s", name)
		}
		if hosted.files[uri] != nil {
			t.Errorf("hosted skill bundle contains local workflow %s", name)
		}
	}
}

// Use Serve, the real relay entry point, so extension dispatch and registered
// resources must select the same catalog without contacting Cloud or a device.
func TestHostedSkillsImportOverRelayTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	in, clientIn := io.Pipe()
	clientOut, out := io.Pipe()
	done := make(chan error, 1)
	s := New(&config.Config{}, nil)
	go func() { done <- s.Serve(ctx, &BindingPolicy{}, in, out) }()
	t.Cleanup(func() {
		cancel()
		_ = clientIn.Close()
		_ = clientOut.Close()
		_ = in.Close()
		_ = out.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("relay transport did not shut down")
		}
	})
	reader := bufio.NewReader(clientOut)
	exchange := func(raw []byte) []byte {
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
	call := func(method string, params, into any) {
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
	call("initialize", map[string]any{"protocolVersion": mcpgo.LATEST_PROTOCOL_VERSION, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "hosted-skill-import", "version": "1"}}, &initialized)
	if _, ok := initialized.Capabilities.Extensions[skillsExtension]; !ok {
		t.Fatal("missing hosted skills extension")
	}
	var listed struct {
		Skills []skillEntry `json:"skills"`
	}
	call("skills/list", map[string]any{}, &listed)
	if len(listed.Skills) != 1 || listed.Skills[0].URI != skillBaseURI+"SKILL.md" {
		t.Fatalf("unexpected hosted catalog: %+v", listed)
	}
	entry := listed.Skills[0]
	var got struct {
		Skill skillEntry `json:"skill"`
	}
	call("skills/get", map[string]any{"uri": entry.URI}, &got)
	if !reflect.DeepEqual(entry, got.Skill) {
		t.Fatal("hosted skills/list and skills/get disagree")
	}

	expected := map[string][]byte{}
	const root = "chatgpt-plugin/skills/wendy/"
	if err := fs.WalkDir(assets.FS, strings.TrimSuffix(root, "/"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := assets.FS.ReadFile(p)
		if err == nil {
			expected[skillBaseURI+strings.TrimPrefix(p, root)] = data
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(entry.Resources) != len(expected) {
		t.Fatalf("hosted catalog has %d resources; public skill has %d", len(entry.Resources), len(expected))
	}
	for _, resource := range entry.Resources {
		want, ok := expected[resource.URI]
		if !ok {
			t.Fatalf("hosted catalog includes a nonpublic resource: %s", resource.URI)
		}
		var read struct {
			Contents []struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"contents"`
		}
		call("resources/read", map[string]any{"uri": resource.URI}, &read)
		if len(read.Contents) != 1 || read.Contents[0].URI != resource.URI || read.Contents[0].Text != string(want) {
			t.Fatalf("hosted resource differs from public skill: %s", resource.URI)
		}
		if resource.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256(want)) {
			t.Fatalf("hosted digest differs from public skill: %s", resource.URI)
		}
	}
	fm, _, err := skillFrontmatter(expected[skillBaseURI+"SKILL.md"])
	if err != nil || !reflect.DeepEqual(fm, entry.Frontmatter) {
		t.Fatalf("hosted frontmatter differs from public skill: %v", err)
	}
	var resources struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
	}
	call("resources/list", map[string]any{}, &resources)
	registered := map[string]bool{}
	for _, resource := range resources.Resources {
		if strings.HasPrefix(resource.URI, skillBaseURI) {
			if _, ok := expected[resource.URI]; !ok {
				t.Fatalf("registered nonpublic skill resource: %s", resource.URI)
			}
			registered[resource.URI] = true
		}
	}
	if len(registered) != len(expected) {
		t.Fatal("registered hosted resources differ from the import catalog")
	}
	for _, method := range []string{"resources/read", "skills/get"} {
		uri := skillBaseURI + "references/skills/wendy-install/SKILL.md"
		if reply := skillCall(t, exchange, method, map[string]any{"uri": uri}); reply.Error == nil {
			t.Fatalf("%s exposed local installer guidance", method)
		}
	}
}
