package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
)

// docsBaseURL is the public docs site's stable "latest" alias (see
// development/docs-site.md). Doc topics map onto it 1:1 by relative path, so
// it doubles as a fallback for anyone who'd rather read in a browser.
const docsBaseURL = "https://docs.wendy.dev/latest"

// newDocsCmd builds `wendy docs [topic]`. Before this command, the CLI's
// embedded documentation (go/internal/cli/assets/docs, wired up in
// go/internal/cli/assets/assets.go) was reachable only through the MCP
// server's wendy://docs/ resources — nothing surfaced it for a plain
// terminal, scripted, or headless-agent invocation of `wendy`. This exposes
// the same embedded content as printable text, with no network access
// required.
func newDocsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs [topic]",
		Short: "Show Wendy documentation from the terminal",
		Long: `Print Wendy's embedded documentation to the terminal — no browser required.

Run with no arguments to list every available topic. A topic may be given by
its short name (e.g. "ros2") or its full path under the docs tree (e.g.
"integrations/ros2"). This is the same content published at ` + docsBaseURL + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			topics, err := docsTopics()
			if err != nil {
				return fmt.Errorf("listing docs: %w", err)
			}
			// cmd.OutOrStdout() (not cobra's cmd.Print*, which despite the
			// name writes to OutOrStderr) keeps `wendy docs --json | jq`
			// working while still honoring cmd.SetOut in tests.
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				printDocsTopicList(out, topics)
				return nil
			}
			return printDocsTopic(out, topics, args[0])
		},
	}
	return cmd
}

// docsTopic describes one renderable page under the embedded docs tree.
type docsTopic struct {
	// Path is the file path relative to the embedded docs root, e.g.
	// "integrations/ros2.mdx".
	Path string
	// Slug is the short lookup key, e.g. "ros2" for "integrations/ros2.mdx"
	// or "integrations" for "integrations/index.mdx". Not necessarily unique
	// across topics (e.g. every tutorials/<language>/hello-world.mdx shares
	// the slug "hello-world") — see printDocsTopic for how collisions are
	// handled.
	Slug string
	// Title comes from the page's frontmatter when present, falling back to
	// a title-cased form of the path.
	Title string
}

// urlPath is the topic's path with its extension (and any trailing "/index")
// stripped, matching the public docs site's URL scheme.
func (t docsTopic) urlPath() string {
	p := strings.TrimSuffix(t.Path, path.Ext(t.Path))
	p = strings.TrimSuffix(p, "/index")
	if p == "index" {
		p = ""
	}
	return p
}

// publicURL is the full public docs-site URL for this topic.
func (t docsTopic) publicURL() string {
	up := t.urlPath()
	if up == "" {
		return docsBaseURL + "/"
	}
	return docsBaseURL + "/" + up + "/"
}

// docsTopics walks the embedded docs tree for renderable pages (.md/.mdx).
// Everything else embedded under "docs" (meta.json navigation files,
// package.json, etc.) is skipped by the extension check.
func docsTopics() ([]docsTopic, error) {
	var topics []docsTopic
	err := fs.WalkDir(assets.FS, "docs", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := path.Ext(p)
		if ext != ".md" && ext != ".mdx" {
			return nil
		}
		rel := strings.TrimPrefix(p, "docs/")
		// WalkDir already found this entry in the embedded FS, so a read
		// failure here means the embed itself is broken. Fail loudly rather
		// than dropping the topic — a silently missing page looks identical
		// to one that was never embedded.
		data, rerr := assets.FS.ReadFile(p)
		if rerr != nil {
			return fmt.Errorf("reading embedded doc %q: %w", p, rerr)
		}
		title, _, _ := parseDocFrontmatter(data)
		if title == "" {
			title = docTitleFromPath(rel)
		}
		topics = append(topics, docsTopic{
			Path:  rel,
			Slug:  docSlug(rel),
			Title: title,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Path < topics[j].Path })
	return topics, nil
}

// docSlug derives the short lookup key for a doc path: the file's base name,
// or its parent directory's name when the file is that directory's index
// page (e.g. "integrations/index.mdx" -> "integrations"). The root
// "index.mdx" has no parent directory to borrow a name from, so it keeps the
// literal slug "index".
func docSlug(relPath string) string {
	noExt := strings.TrimSuffix(relPath, path.Ext(relPath))
	base := path.Base(noExt)
	if base != "index" {
		return base
	}
	dir := path.Dir(noExt)
	if dir == "." {
		return "index"
	}
	return path.Base(dir)
}

// docTitleFromPath produces a readable fallback title for a doc that has no
// frontmatter title, e.g. "guides/tutorials/python/hello-world.mdx" ->
// "hello world".
func docTitleFromPath(relPath string) string {
	base := path.Base(strings.TrimSuffix(relPath, path.Ext(relPath)))
	base = strings.ReplaceAll(base, "-", " ")
	return strings.ReplaceAll(base, "_", " ")
}

// docFrontmatterPattern matches a leading YAML frontmatter block delimited by
// "---" lines, as used by the fumadocs MDX content (title/description keys).
var docFrontmatterPattern = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?`)

// parseDocFrontmatter extracts "title" and "description" from a leading YAML
// frontmatter block, if present, and returns the remaining body. It only
// understands simple "key: value" lines — everything these docs actually use
// — not general YAML.
func parseDocFrontmatter(data []byte) (title, description string, body []byte) {
	loc := docFrontmatterPattern.FindSubmatchIndex(data)
	if loc == nil {
		return "", "", data
	}
	block := string(data[loc[2]:loc[3]])
	body = data[loc[1]:]
	for _, line := range strings.Split(block, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "title":
			title = val
		case "description":
			description = val
		}
	}
	return title, description, body
}

// mdxComponentPattern strips fumadocs/JSX components (e.g. <Callout
// type="info">…</Callout>, self-closing <ImageZoom .../>) from MDX bodies for
// plain-terminal rendering. JSX convention capitalizes component names, which
// is what distinguishes them here from any incidental "<" in prose. This is
// lossy — a component's own visual treatment (callout icon, tabs, image) is
// dropped — but its text content, which is what these docs actually convey,
// reads fine as plain Markdown afterward.
var mdxComponentPattern = regexp.MustCompile(`</?[A-Z][A-Za-z0-9]*(?:\s[^<>]*)?/?>`)

func renderDocsMDX(body []byte) string {
	return mdxComponentPattern.ReplaceAllString(string(body), "")
}

// printDocsTopicList prints every available doc topic grouped by directory,
// or a JSON array of {path,slug,title} with --json.
func printDocsTopicList(w io.Writer, topics []docsTopic) {
	if jsonOutput {
		type jsonTopic struct {
			Path  string `json:"path"`
			Slug  string `json:"slug"`
			Title string `json:"title"`
		}
		out := make([]jsonTopic, 0, len(topics))
		for _, t := range topics {
			out = append(out, jsonTopic{Path: t.urlPath(), Slug: t.Slug, Title: t.Title})
		}
		data, err := json.Marshal(out)
		if err != nil {
			return
		}
		fmt.Fprintln(w, string(data))
		return
	}

	groups := map[string][]docsTopic{}
	var dirs []string
	for _, t := range topics {
		dir := path.Dir(t.Path)
		if _, ok := groups[dir]; !ok {
			dirs = append(dirs, dir)
		}
		groups[dir] = append(groups[dir], t)
	}
	sort.Strings(dirs)

	fmt.Fprintf(w, "Available doc topics (%d) — run `wendy docs <topic>` to read one:\n", len(topics))
	for _, dir := range dirs {
		if dir == "." {
			fmt.Fprintln(w)
		} else {
			fmt.Fprintf(w, "\n%s/\n", dir)
		}
		items := groups[dir]
		sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
		for _, t := range items {
			// The site-root index has no non-empty urlPath ("" maps to
			// docsBaseURL itself); show its slug ("index") instead so the
			// listed value is always something `wendy docs <topic>` accepts.
			label := t.urlPath()
			if label == "" {
				label = t.Slug
			}
			fmt.Fprintf(w, "  %-40s %s\n", label, t.Title)
		}
	}
}

// printDocsTopic resolves arg against topics and prints the match. arg may be
// a short slug or the full path (with or without a Markdown extension). An
// unresolvable or ambiguous arg returns an error that lists the way out, matching the
// quality of feedback `wendy project entitlements add <bad-type>` gives.
func printDocsTopic(w io.Writer, topics []docsTopic, arg string) error {
	needle := strings.Trim(strings.TrimSpace(arg), "/")
	// Dotted topic names such as wendy.json are listed without a Markdown extension.
	if ext := path.Ext(needle); ext == ".md" || ext == ".mdx" {
		needle = strings.TrimSuffix(needle, ext)
	}

	for _, t := range topics {
		if t.urlPath() == needle {
			return renderDocsTopic(w, t)
		}
	}

	var matches []docsTopic
	for _, t := range topics {
		if t.Slug == needle {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 1:
		return renderDocsTopic(w, matches[0])
	case 0:
		names := make([]string, 0, len(topics))
		for _, t := range topics {
			names = append(names, t.urlPath())
		}
		sort.Strings(names)
		return fmt.Errorf("unknown doc topic %q\nRun `wendy docs` with no arguments to list all %d available topics", arg, len(names))
	default:
		paths := make([]string, 0, len(matches))
		for _, m := range matches {
			paths = append(paths, m.urlPath())
		}
		sort.Strings(paths)
		return fmt.Errorf("%q matches more than one doc topic; use the full path:\n  %s", arg, strings.Join(paths, "\n  "))
	}
}

func renderDocsTopic(w io.Writer, t docsTopic) error {
	data, err := assets.FS.ReadFile("docs/" + t.Path)
	if err != nil {
		return err
	}
	title, description, body := parseDocFrontmatter(data)
	if title == "" {
		title = t.Title
	}

	if jsonOutput {
		out := map[string]string{
			"path":        t.urlPath(),
			"title":       title,
			"description": description,
			"body":        renderDocsMDX(body),
			"url":         t.publicURL(),
		}
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(data))
		return nil
	}

	if title != "" {
		fmt.Fprintln(w, title)
		fmt.Fprintln(w, strings.Repeat("=", len(title)))
		fmt.Fprintln(w)
	}
	if description != "" {
		fmt.Fprintln(w, description)
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, renderDocsMDX(body))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "View online: %s\n", t.publicURL())
	return nil
}
