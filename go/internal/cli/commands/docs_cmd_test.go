package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestNewDocsCmd_Wiring(t *testing.T) {
	cmd := newDocsCmd()
	if cmd.Use != "docs [topic]" {
		t.Errorf("Use = %q; want %q", cmd.Use, "docs [topic]")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}
	if cmd.Args == nil {
		t.Error("Args validator should be set (accepts at most one positional topic)")
	}
}

// TestDocsTopics_IncludesROS2 is the direct regression test for the concrete
// friction this PR fixes: `wendy device ros2 --help` now points at `wendy
// docs ros2`, so that topic must actually resolve to the embedded
// integrations/ros2.mdx page (previously not embedded in the CLI binary at
// all — see go/internal/cli/assets/assets.go).
func TestDocsTopics_IncludesROS2(t *testing.T) {
	topics, err := docsTopics()
	if err != nil {
		t.Fatalf("docsTopics: %v", err)
	}
	if len(topics) == 0 {
		t.Fatal("docsTopics returned no topics")
	}

	var found *docsTopic
	for i := range topics {
		if topics[i].Path == "integrations/ros2.mdx" {
			found = &topics[i]
		}
	}
	if found == nil {
		t.Fatal(`expected a topic for "integrations/ros2.mdx"`)
	}
	if found.Slug != "ros2" {
		t.Errorf("slug = %q; want %q", found.Slug, "ros2")
	}
	if found.Title == "" {
		t.Error("title should be populated from frontmatter")
	}
}

func TestDocSlug(t *testing.T) {
	cases := map[string]string{
		"integrations/ros2.mdx":      "ros2",
		"integrations/index.mdx":     "integrations",
		"index.mdx":                  "index",
		"guides/tutorials/foo.mdx":   "foo",
		"device/entitlements.md":     "entitlements",
		"clients/wendy-cli/init.mdx": "init",
	}
	for in, want := range cases {
		if got := docSlug(in); got != want {
			t.Errorf("docSlug(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestDocsTopic_URLPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"integrations/ros2.mdx", "integrations/ros2"},
		{"integrations/index.mdx", "integrations"},
		{"index.mdx", ""},
		{"device/entitlements.md", "device/entitlements"},
	}
	for _, tc := range cases {
		topic := docsTopic{Path: tc.path}
		if got := topic.urlPath(); got != tc.want {
			t.Errorf("urlPath(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

func TestDocsTopic_PublicURL(t *testing.T) {
	topic := docsTopic{Path: "integrations/ros2.mdx"}
	want := "https://docs.wendy.dev/latest/integrations/ros2/"
	if got := topic.publicURL(); got != want {
		t.Errorf("publicURL() = %q; want %q", got, want)
	}
}

func TestParseDocFrontmatter(t *testing.T) {
	data := []byte("---\ntitle: Wendy for ROS 2\ndescription: Some description here\n---\n\nBody text follows.\n")
	title, description, body := parseDocFrontmatter(data)
	if title != "Wendy for ROS 2" {
		t.Errorf("title = %q; want %q", title, "Wendy for ROS 2")
	}
	if description != "Some description here" {
		t.Errorf("description = %q; want %q", description, "Some description here")
	}
	if strings.Contains(string(body), "---") {
		t.Errorf("body should not contain the frontmatter delimiters: %q", body)
	}
	if !strings.Contains(string(body), "Body text follows.") {
		t.Errorf("body missing content: %q", body)
	}
}

func TestParseDocFrontmatter_NoFrontmatter(t *testing.T) {
	data := []byte("# Just a heading\n\nNo frontmatter here.\n")
	title, description, body := parseDocFrontmatter(data)
	if title != "" || description != "" {
		t.Errorf("title/description = %q/%q; want both empty", title, description)
	}
	if string(body) != string(data) {
		t.Errorf("body = %q; want unchanged input", body)
	}
}

func TestRenderDocsMDX_StripsComponentsKeepsText(t *testing.T) {
	body := []byte(`<Callout type="info">
Important note text.
</Callout>

Regular paragraph with <CliShot flow="x" alt="y" />.
`)
	got := renderDocsMDX(body)
	if strings.Contains(got, "Callout") || strings.Contains(got, "CliShot") {
		t.Errorf("component tags should be stripped, got: %q", got)
	}
	if !strings.Contains(got, "Important note text.") {
		t.Errorf("inner text should be preserved, got: %q", got)
	}
	if !strings.Contains(got, "Regular paragraph with") {
		t.Errorf("surrounding prose should be preserved, got: %q", got)
	}
}

// TestRenderDocsMDX_PreservesPlaceholderSyntax guards against a regression
// where the JSX-stripping regex is broadened to match lowercase tag names: a
// robotics CLI doc set uses lowercase `<placeholder>` syntax in usage text
// (e.g. "wendy device ros2 call <service> <type>"), which must not be
// confused with an MDX/JSX component (PascalCase by convention).
func TestRenderDocsMDX_PreservesPlaceholderSyntax(t *testing.T) {
	body := []byte("wendy device ros2 call <service> <type> [request]\n")
	got := renderDocsMDX(body)
	if got != string(body) {
		t.Errorf("renderDocsMDX(%q) = %q; want unchanged (lowercase placeholders are not JSX)", body, got)
	}
}

func TestPrintDocsTopic_UnknownTopic(t *testing.T) {
	topics := []docsTopic{{Path: "integrations/ros2.mdx", Slug: "ros2", Title: "Wendy for ROS 2"}}
	err := printDocsTopic(io.Discard, topics, "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for an unknown topic")
	}
	if !strings.Contains(err.Error(), `unknown doc topic "does-not-exist"`) {
		t.Errorf("error = %q; want it to name the bad topic", err.Error())
	}
}

// TestPrintDocsTopic_AmbiguousSlug covers the real collision in this docs
// tree: every guides/tutorials/<language>/hello-world.mdx shares the slug
// "hello-world". The error should list the full paths rather than silently
// picking one.
func TestPrintDocsTopic_AmbiguousSlug(t *testing.T) {
	topics := []docsTopic{
		{Path: "guides/tutorials/python/hello-world.mdx", Slug: "hello-world", Title: "Hello World in Python"},
		{Path: "guides/tutorials/rust/hello-world.mdx", Slug: "hello-world", Title: "Hello World in Rust"},
	}
	err := printDocsTopic(io.Discard, topics, "hello-world")
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	for _, want := range []string{"guides/tutorials/python/hello-world", "guides/tutorials/rust/hello-world"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q; want it to list %q", err.Error(), want)
		}
	}
}

// TestPrintDocsTopic_RendersROS2ViaSlug exercises `wendy docs ros2` end to
// end against the real embedded content: the short slug must resolve to
// integrations/ros2.mdx, render its frontmatter title, and print the public
// docs URL as a fallback — this is exactly what ros2.go's Long help text now
// tells users to run.
func TestPrintDocsTopic_RendersROS2ViaSlug(t *testing.T) {
	topics, err := docsTopics()
	if err != nil {
		t.Fatalf("docsTopics: %v", err)
	}

	var buf bytes.Buffer
	if err := printDocsTopic(&buf, topics, "ros2"); err != nil {
		t.Errorf("printDocsTopic(ros2): %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Wendy for ROS 2") {
		t.Errorf("output missing title, got: %q", out)
	}
	if !strings.Contains(out, "View online: https://docs.wendy.dev/latest/integrations/ros2/") {
		t.Errorf("output missing the public docs URL fallback, got: %q", out)
	}
}

func TestNewDocsCmd_DottedTopic(t *testing.T) {
	prevJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = prevJSON })

	for _, mode := range []struct {
		name string
		json bool
	}{
		{"text", false},
		{"json", true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			jsonOutput = mode.json
			for _, arg := range []string{
				"apps/wendy.json",
				"wendy.json",
				"apps/wendy.json.md",
				"wendy.json.md",
				"apps/wendy.json.mdx",
				" /apps/wendy.json/ ",
			} {
				t.Run(arg, func(t *testing.T) {
					cmd := newDocsCmd()
					var buf bytes.Buffer
					cmd.SetOut(&buf)
					cmd.SetErr(&buf)
					cmd.SetArgs([]string{arg})
					if err := cmd.Execute(); err != nil {
						t.Fatalf("wendy docs %q: %v", arg, err)
					}
					if !mode.json {
						if !strings.Contains(buf.String(), "View online: https://docs.wendy.dev/latest/apps/wendy.json/") {
							t.Fatal("text output does not identify the wendy.json documentation")
						}
						return
					}
					var rendered struct {
						Path string `json:"path"`
						Body string `json:"body"`
					}
					if err := json.Unmarshal(buf.Bytes(), &rendered); err != nil {
						t.Fatalf("decode rendered topic: %v", err)
					}
					if rendered.Path != "apps/wendy.json" || rendered.Body == "" {
						t.Fatalf("expected the wendy.json documentation, got path %q and %d body bytes", rendered.Path, len(rendered.Body))
					}
				})
			}
		})
	}
}

func TestPrintDocsTopic_UnknownExtension(t *testing.T) {
	topics, err := docsTopics()
	if err != nil {
		t.Fatalf("docsTopics: %v", err)
	}
	for _, arg := range []string{"ros2.json", "integrations/ros2.txt", "apps/wendy.json.bak"} {
		t.Run(arg, func(t *testing.T) {
			var buf bytes.Buffer
			err := printDocsTopic(&buf, topics, arg)
			if err == nil || !strings.Contains(err.Error(), "unknown doc topic") {
				t.Fatalf("expected an unknown-topic error for %q, got %v", arg, err)
			}
			if buf.Len() != 0 {
				t.Fatalf("unknown topic %q rendered a different document", arg)
			}
		})
	}
}

func TestPrintDocsTopicList_TopicsResolve(t *testing.T) {
	prevJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = prevJSON })

	topics, err := docsTopics()
	if err != nil {
		t.Fatalf("docsTopics: %v", err)
	}
	var buf bytes.Buffer
	printDocsTopicList(&buf, topics)
	var listed []struct {
		Path string `json:"path"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(buf.Bytes(), &listed); err != nil {
		t.Fatalf("decode topic list: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("topic list is empty")
	}
	for _, topic := range listed {
		arg := topic.Path
		if arg == "" {
			arg = topic.Slug
		}
		t.Run(arg, func(t *testing.T) {
			if err := printDocsTopic(io.Discard, topics, arg); err != nil {
				t.Fatalf("listed topic %q cannot be opened: %v", arg, err)
			}
		})
	}
}

func TestPrintDocsTopicList_NonJSON(t *testing.T) {
	prevJSON := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = prevJSON }()

	topics := []docsTopic{
		{Path: "integrations/ros2.mdx", Slug: "ros2", Title: "Wendy for ROS 2"},
		{Path: "hardware/camera.mdx", Slug: "camera", Title: "Camera Access"},
	}

	var buf bytes.Buffer
	printDocsTopicList(&buf, topics)
	out := buf.String()

	for _, want := range []string{"integrations/ros2", "Wendy for ROS 2", "hardware/camera", "Camera Access"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q, got: %q", want, out)
		}
	}
}

func TestPrintDocsTopicList_JSON(t *testing.T) {
	prevJSON := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = prevJSON }()

	topics := []docsTopic{{Path: "integrations/ros2.mdx", Slug: "ros2", Title: "Wendy for ROS 2"}}

	var buf bytes.Buffer
	printDocsTopicList(&buf, topics)
	out := buf.String()

	if !strings.Contains(out, `"slug":"ros2"`) {
		t.Errorf("expected JSON output to contain the slug field, got: %q", out)
	}
	if buf.Len() == 0 {
		t.Error("expected non-empty JSON output")
	}
}

// TestNewDocsCmd_RespectsSetOut is the regression test for the output
// plumbing: `wendy docs` must render into cmd.SetOut's writer rather than
// unconditionally into the process's stdout.
func TestNewDocsCmd_RespectsSetOut(t *testing.T) {
	prevJSON := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = prevJSON }()

	cmd := newDocsCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"ros2"})

	stdout := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})

	if !strings.Contains(buf.String(), "Wendy for ROS 2") {
		t.Errorf("redirected output missing the topic title, got: %q", buf.String())
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("nothing should have leaked to the process stdout, got: %q", stdout)
	}
}
