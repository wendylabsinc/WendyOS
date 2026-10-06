package commands

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	toml "github.com/BurntSushi/toml"
)

const testWendyBin = "/opt/wendy/bin/wendy"

var testWendyArgs = []string{"mcp", "serve"}

// The exact lines addMCPToTOMLConfig renders for testWendyBin.
const (
	wendyCommandLine = "command = \"/opt/wendy/bin/wendy\"\n"
	wendyArgsLine    = "args = [\"mcp\", \"serve\"]\n"
	wendyTable       = "[mcp_servers.wendy]\n" + wendyCommandLine + wendyArgsLine
)

// TestUpsertCodexMCPServer_Golden pins the exact output bytes: only the
// command and args keys of [mcp_servers.wendy] are wendy's; every other byte
// — including keys and sub-tables the user added to the wendy entry — must
// survive untouched.
func TestUpsertCodexMCPServer_Golden(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty file",
			in:   "",
			want: wendyTable,
		},
		{
			name: "comments and key order survive an append",
			in: "# Codex settings — managed by hand\n" +
				"model = \"gpt-5-codex\"   # pinned\n" +
				"approval_policy = \"on-request\"\n" +
				"\n" +
				"[profiles.fast]\n" +
				"model = \"gpt-5-mini\"\n",
			want: "# Codex settings — managed by hand\n" +
				"model = \"gpt-5-codex\"   # pinned\n" +
				"approval_policy = \"on-request\"\n" +
				"\n" +
				"[profiles.fast]\n" +
				"model = \"gpt-5-mini\"\n" +
				"\n" +
				wendyTable,
		},
		{
			name: "missing trailing newline",
			in:   "model = \"o3\"",
			want: "model = \"o3\"\n\n" + wendyTable,
		},
		{
			name: "other servers and their sub-tables are untouched; stale wendy updated in place",
			in: "[mcp_servers.github]\n" +
				"command = \"npx\"\n" +
				"args = [\"-y\", \"@modelcontextprotocol/server-github\"]\n" +
				"\n" +
				"[mcp_servers.github.env]\n" +
				"GITHUB_TOKEN = \"ghp_example\"\n" +
				"\n" +
				"# wendy (managed by wendy mcp setup)\n" +
				"[mcp_servers.wendy]\n" +
				"command = \"/old/path/wendy\"\n" +
				"args = [\"mcp\", \"serve\"]\n" +
				"\n" +
				"# trailing comment about the next table\n" +
				"[profiles.fast]\n" +
				"model = \"gpt-5-mini\"\n",
			want: "[mcp_servers.github]\n" +
				"command = \"npx\"\n" +
				"args = [\"-y\", \"@modelcontextprotocol/server-github\"]\n" +
				"\n" +
				"[mcp_servers.github.env]\n" +
				"GITHUB_TOKEN = \"ghp_example\"\n" +
				"\n" +
				"# wendy (managed by wendy mcp setup)\n" +
				wendyTable +
				"\n" +
				"# trailing comment about the next table\n" +
				"[profiles.fast]\n" +
				"model = \"gpt-5-mini\"\n",
		},
		{
			// An env var or timeout the user set must survive every
			// setup run and the silent refresh after each CLI upgrade.
			name: "user keys and sub-tables of the wendy entry are kept; mcp serve args kept as written",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/old/wendy\"\n" +
				"args = [\n" +
				"  \"mcp\",\n" +
				"  # a comment inside the array\n" +
				"  \"serve\",\n" +
				"]\n" +
				"startup_timeout_sec = 30\n" +
				"\n" +
				"[mcp_servers.wendy.env]\n" +
				"HTTPS_PROXY = \"http://proxy.local:3128\"\n" +
				"command = \"an env var, not the server command\"\n" +
				"\n" +
				"[mcp_servers.other]\n" +
				"command = \"other\"\n",
			want: "[mcp_servers.wendy]\n" +
				wendyCommandLine +
				"args = [\n" +
				"  \"mcp\",\n" +
				"  # a comment inside the array\n" +
				"  \"serve\",\n" +
				"]\n" +
				"startup_timeout_sec = 30\n" +
				"\n" +
				"[mcp_servers.wendy.env]\n" +
				"HTTPS_PROXY = \"http://proxy.local:3128\"\n" +
				"command = \"an env var, not the server command\"\n" +
				"\n" +
				"[mcp_servers.other]\n" +
				"command = \"other\"\n",
		},
		{
			name: "user keys around the owned keys stay in place",
			in: "[mcp_servers.wendy]\n" +
				"# pinned by me\n" +
				"env = { HTTPS_PROXY = \"http://proxy.local:3128\" }\n" +
				"command = \"/old/wendy\"  # old\n" +
				"enabled_tools = [\"run\", \"device_list\"]\n" +
				"\"args\" = [\"mcp\", \"serve\", \"--old\"]\n" +
				"tool_timeout_sec = 120\n",
			want: "[mcp_servers.wendy]\n" +
				"# pinned by me\n" +
				"env = { HTTPS_PROXY = \"http://proxy.local:3128\" }\n" +
				"command = \"/opt/wendy/bin/wendy\"  # old\n" +
				"enabled_tools = [\"run\", \"device_list\"]\n" +
				"\"args\" = [\"mcp\", \"serve\", \"--old\"]\n" +
				"tool_timeout_sec = 120\n",
		},
		{
			// The wendy-mcp-setup skill tells users to pin the server to one
			// device this way; setup and every upgrade refresh must keep it.
			name: "a pinned --device survives; only the command is updated",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/old/wendy\"\n" +
				"args = [\"mcp\", \"serve\", \"--device\", \"my-pi.local\"]\n",
			want: "[mcp_servers.wendy]\n" +
				wendyCommandLine +
				"args = [\"mcp\", \"serve\", \"--device\", \"my-pi.local\"]\n",
		},
		{
			name: "stale args that do not start with mcp serve are replaced in place",
			in: "[mcp_servers.wendy]\n" +
				"env = { A = \"1\" }\n" +
				"args = [\"serve\", \"--old\"]\n" +
				"tool_timeout_sec = 120\n" +
				"command = \"/old/wendy\"\n",
			want: "[mcp_servers.wendy]\n" +
				"env = { A = \"1\" }\n" +
				wendyArgsLine +
				"tool_timeout_sec = 120\n" +
				wendyCommandLine,
		},
		{
			name: "multi-line stale args are replaced with all their lines",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = [\n" +
				"  \"mcp\",\n" +
				"]\n" +
				"startup_timeout_sec = 30\n",
			want: wendyTable +
				"startup_timeout_sec = 30\n",
		},
		{
			// TOML lets a multi-line string end with up to two quotes of its
			// own right before the closing delimiter.
			name: "four-quote string ending inside replaced args keeps the next comment",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = [\"\"\"mcp\"\"\"\", \"serve\"]\n" +
				"# keep me\n",
			want: wendyTable +
				"# keep me\n",
		},
		{
			name: "four-quote literal string ending inside replaced args keeps the next comment",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = ['''mcp'''', 'serve']\n" +
				"# keep me\n" +
				"startup_timeout_sec = 30\n",
			want: wendyTable +
				"# keep me\n" +
				"startup_timeout_sec = 30\n",
		},
		{
			name: "five-quote string ending in another key does not hide the wendy table",
			in: "notes = [\"\"\"a\"\"\"\"\", \"b\"]\n" +
				"# keep me\n" +
				"[mcp_servers.wendy]\n" +
				"command = \"/old/wendy\"\n" +
				"args = [\"mcp\", \"serve\"]\n",
			want: "notes = [\"\"\"a\"\"\"\"\", \"b\"]\n" +
				"# keep me\n" +
				wendyTable,
		},
		{
			// Comments are never deleted: those inside a replaced multi-line
			// value move to just after the new line.
			name: "comments inside replaced multi-line args are kept",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = [\n" +
				"  \"stdio\",\n" +
				"  # why: see ticket 123\n" +
				"]\n" +
				"startup_timeout_sec = 30\n",
			want: wendyTable +
				"  # why: see ticket 123\n" +
				"startup_timeout_sec = 30\n",
		},
		{
			// A comment at the end of a replaced key line stays on the new
			// line; those on its continuation lines become comment lines.
			name: "end-of-line comments of a replaced multi-line value are kept",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = [ # why\n" +
				"  \"stdio\", # ticket 1\n" +
				"] # closing\n" +
				"startup_timeout_sec = 30\n",
			want: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = [\"mcp\", \"serve\"] # why\n" +
				"  # ticket 1\n" +
				"# closing\n" +
				"startup_timeout_sec = 30\n",
		},
		{
			name: "end-of-line comments are kept with CRLF line endings",
			in: "[mcp_servers.wendy]\r\n" +
				"command = \"/opt/wendy/bin/wendy\"\r\n" +
				"args = [ # why\r\n" +
				"\t\"stdio\", # t\r\n" +
				"]\r\n",
			want: "[mcp_servers.wendy]\r\n" +
				"command = \"/opt/wendy/bin/wendy\"\r\n" +
				"args = [\"mcp\", \"serve\"] # why\r\n" +
				"\t# t\r\n",
		},
		{
			name: "a comment after the end of a replaced multi-line string is kept",
			in: "[mcp_servers.wendy]\n" +
				"command = \"\"\"/old/wendy\n" +
				"\"\"\"  # set by hand\n" +
				"args = [\"mcp\", \"serve\"]\n",
			want: "[mcp_servers.wendy]\n" +
				wendyCommandLine +
				"# set by hand\n" +
				wendyArgsLine,
		},
		{
			name: "comment-like lines inside a replaced multi-line string are string content",
			in: "[mcp_servers.wendy]\n" +
				"command = \"\"\"\n" +
				"# not a comment\n" +
				"/old/wendy\"\"\"\n" +
				"args = [\"mcp\", \"serve\"]\n",
			want: wendyTable,
		},
		{
			// Some Windows editors save UTF-8 with a byte order mark.
			name: "UTF-8 BOM before the wendy header is kept and the header recognised",
			in: "\ufeff[mcp_servers.wendy]\n" +
				"command = \"/old/wendy\"\n",
			want: "\ufeff[mcp_servers.wendy]\n" +
				wendyCommandLine +
				wendyArgsLine,
		},
		{
			name: "UTF-8 BOM before a comment is kept on append",
			in:   "\ufeff# mine\nmodel = \"o3\"\n",
			want: "\ufeff# mine\nmodel = \"o3\"\n\n" + wendyTable,
		},
		{
			name: "args that are not an array are replaced",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/opt/wendy/bin/wendy\"\n" +
				"args = \"mcp serve\"\n",
			want: wendyTable,
		},
		{
			name: "missing args is added after command",
			in: "[mcp_servers.wendy]\n" +
				"command = \"/old/wendy\"\n" +
				"startup_timeout_sec = 30\n",
			want: wendyTable +
				"startup_timeout_sec = 30\n",
		},
		{
			name: "missing command and args are added after the header",
			in: "[mcp_servers.wendy]\n" +
				"startup_timeout_sec = 30\n" +
				"\n" +
				"[mcp_servers.wendy.env]\n" +
				"HTTPS_PROXY = \"http://proxy.local:3128\"\n",
			want: wendyTable +
				"startup_timeout_sec = 30\n" +
				"\n" +
				"[mcp_servers.wendy.env]\n" +
				"HTTPS_PROXY = \"http://proxy.local:3128\"\n",
		},
		{
			name: "header without a trailing newline",
			in:   "[mcp_servers.wendy]",
			want: wendyTable,
		},
		{
			name: "only a wendy sub-table: the table is appended and the sub-table kept",
			in: "[mcp_servers.wendy.env]\n" +
				"HTTPS_PROXY = \"http://proxy.local:3128\"\n",
			want: "[mcp_servers.wendy.env]\n" +
				"HTTPS_PROXY = \"http://proxy.local:3128\"\n" +
				"\n" +
				wendyTable,
		},
		{
			name: "an up-to-date entry is left exactly as written",
			in: "[mcp_servers.wendy]\n" +
				"args = ['mcp', 'serve']   # mine\n" +
				"command = '/opt/wendy/bin/wendy'\n" +
				"env = { HTTPS_PROXY = \"http://proxy.local:3128\" }\n",
			want: "[mcp_servers.wendy]\n" +
				"args = ['mcp', 'serve']   # mine\n" +
				"command = '/opt/wendy/bin/wendy'\n" +
				"env = { HTTPS_PROXY = \"http://proxy.local:3128\" }\n",
		},
		{
			name: "brackets inside strings and multi-line values are not headers",
			in: "notes = \"\"\"\n" +
				"[mcp_servers.wendy]\n" +
				"command = \"not a key\"\n" +
				"\"\"\"\n" +
				"matrix = [\n" +
				"  [1, 2],\n" +
				"  [3, 4],\n" +
				"]\n",
			want: "notes = \"\"\"\n" +
				"[mcp_servers.wendy]\n" +
				"command = \"not a key\"\n" +
				"\"\"\"\n" +
				"matrix = [\n" +
				"  [1, 2],\n" +
				"  [3, 4],\n" +
				"]\n" +
				"\n" +
				wendyTable,
		},
		{
			name: "quoted and spaced header is recognised and kept as written",
			in: "[ mcp_servers . \"wendy\" ]\n" +
				"command = \"/old/wendy\"\n",
			want: "[ mcp_servers . \"wendy\" ]\n" +
				wendyCommandLine +
				wendyArgsLine,
		},
		{
			name: "similarly named server is not touched",
			in: "[mcp_servers.wendyx]\n" +
				"command = \"x\"\n",
			want: "[mcp_servers.wendyx]\n" +
				"command = \"x\"\n" +
				"\n" +
				wendyTable,
		},
		{
			name: "CRLF line endings are kept",
			in: "# windows\r\n" +
				"[mcp_servers.wendy]\r\n" +
				"command = \"C:\\\\old\\\\wendy.exe\"\r\n" +
				"\r\n" +
				"[other]\r\n" +
				"a = 1\r\n",
			want: "# windows\r\n" +
				"[mcp_servers.wendy]\r\n" +
				"command = \"/opt/wendy/bin/wendy\"\r\n" +
				"args = [\"mcp\", \"serve\"]\r\n" +
				"\r\n" +
				"[other]\r\n" +
				"a = 1\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := upsertCodexMCPServer([]byte(tt.in), "mcp_servers", "wendy", testWendyBin, testWendyArgs)
			if err != nil {
				t.Fatalf("upsertCodexMCPServer: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
			again, err := upsertCodexMCPServer(got, "mcp_servers", "wendy", testWendyBin, testWendyArgs)
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			if string(again) != string(got) {
				t.Fatalf("not idempotent\n--- first ---\n%s\n--- second ---\n%s", got, again)
			}
		})
	}
}

func TestUpsertCodexMCPServer_EscapesCommand(t *testing.T) {
	got, err := upsertCodexMCPServer(nil, "mcp_servers", "wendy", `C:\Program Files\Wendy "CLI"\wendy.exe`, testWendyArgs)
	if err != nil {
		t.Fatal(err)
	}
	if want := `command = "C:\\Program Files\\Wendy \"CLI\"\\wendy.exe"`; !strings.Contains(string(got), want) {
		t.Fatalf("command not escaped as a TOML basic string:\n%s", got)
	}
}

// Entries written with syntax the editor does not rewrite are reported, not
// silently duplicated or reformatted.
func TestUpsertCodexMCPServer_RefusesUnmanagedSyntax(t *testing.T) {
	for name, in := range map[string]string{
		"inline entry":         "[mcp_servers]\nwendy = { command = \"wendy\", args = [\"mcp\", \"serve\"] }\n",
		"dotted entry in root": "mcp_servers.wendy.command = \"wendy\"\n",
		"dotted entry":         "[mcp_servers]\nwendy.command = \"wendy\"\n",
		"inline parent table":  "mcp_servers = { github = { command = \"npx\" } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := upsertCodexMCPServer([]byte(in), "mcp_servers", "wendy", testWendyBin, testWendyArgs)
			if !errors.Is(err, errTOMLUnmanagedEntry) {
				t.Fatalf("err = %v, want errTOMLUnmanagedEntry", err)
			}
		})
	}
}

// The decode-and-compare safety net refuses an edit that would not produce
// exactly the wanted entry — here [mcp_servers.wendy] would land inside the
// last [[mcp_servers]] array element.
func TestUpsertCodexMCPServer_VerificationRejectsUnexpectedShape(t *testing.T) {
	for name, in := range map[string]string{
		"array parent": "[[mcp_servers]]\nname = \"a\"\n",
		"array entry":  "[[mcp_servers.wendy]]\ncommand = \"/old/wendy\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := upsertCodexMCPServer([]byte(in), "mcp_servers", "wendy", testWendyBin, testWendyArgs)
			if err == nil || !strings.Contains(err.Error(), "without touching other settings") {
				t.Fatalf("err = %v, want the verification error", err)
			}
		})
	}
}

// Other servers written with inline tables or dotted keys are fine: only the
// wendy entry itself must be a [table].
func TestUpsertCodexMCPServer_OtherInlineServersAreFine(t *testing.T) {
	in := "[mcp_servers]\ngithub = { command = \"npx\" }\nlinear.command = \"linear\"\n"
	got, err := upsertCodexMCPServer([]byte(in), "mcp_servers", "wendy", testWendyBin, testWendyArgs)
	if err != nil {
		t.Fatal(err)
	}
	if want := in + "\n" + wendyTable; string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpsertCodexMCPServer_InvalidTOML(t *testing.T) {
	if _, err := upsertCodexMCPServer([]byte("[broken\n"), "mcp_servers", "wendy", testWendyBin, testWendyArgs); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestAddMCPToTOMLConfig_MissingFileAndDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "config.toml")
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", testWendyBin, testWendyArgs); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wendyTable {
		t.Fatalf("got:\n%s", got)
	}
}

func TestAddMCPToTOMLConfig_UpToDateFileIsNotRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# mine\n\n"+wendyTable+"startup_timeout_sec = 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", testWendyBin, testWendyArgs); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Fatalf("up-to-date config was rewritten (mtime %v, want %v)", fi.ModTime(), old)
	}
}

func TestAddMCPToTOMLConfig_PreservesModeAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions and symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("model = \"o3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := addMCPToTOMLConfig(link, "mcp_servers", "wendy", testWendyBin, testWendyArgs); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced by a regular file (err=%v)", err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
	}
	got, _ := os.ReadFile(target)
	if want := "model = \"o3\"\n\n" + wendyTable; string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAddMCPToTOMLConfig_ErrorLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := "# keep me\nmcp_servers.wendy.command = \"wendy\"\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", testWendyBin, testWendyArgs); err == nil {
		t.Fatal("expected an error for a dotted-key entry")
	}
	if got, _ := os.ReadFile(path); string(got) != in {
		t.Fatalf("file changed on error:\n%s", got)
	}
}

// The scanner is hand-written: whatever the input, the editor must not panic,
// and any output it accepts must be valid, current and stable on a re-run.
func FuzzUpsertCodexMCPServer(f *testing.F) {
	for _, seed := range []string{
		"",
		"model = \"o3\"",
		wendyTable,
		"[mcp_servers.wendy]\ncommand = \"/old\"\nargs = [\n  \"mcp\",\n]\n[mcp_servers.wendy.env]\nA = \"1\"\n",
		"[mcp_servers]\nwendy = { command = \"x\" }\n",
		"notes = '''\n[mcp_servers.wendy]\n'''\n",
		"# c\r\n[mcp_servers.wendy]\r\ncommand = \"x\"\r\n",
		"[mcp_servers.wendy]\nargs = [\"\"\"mcp\"\"\"\", \"serve\"]\n# keep me\n",
		"[mcp_servers.wendy]\nargs = [\n  \"x\",\n  # c\n]\n",
		"\ufeff[mcp_servers.wendy]\ncommand = \"x\"\n",
		"[mcp_servers.wendy]\ncommand = \"/opt/wendy/bin/wendy\"\nargs = [ # why\n  \"stdio\", # ticket 1\n] # closing\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(checkUpsertInvariants)
}

// Structured inputs reach deeper than byte-level mutation: realistic Codex
// config fragments, optionally behind a BOM (bit 7 of the first selector)
// and with CRLF line endings (bit 6).
func FuzzUpsertCodexMCPServer_Structured(f *testing.F) {
	frags := []string{
		"[mcp_servers.wendy]\n",
		"[mcp_servers.wendy.env]\n",
		"[mcp_servers.other]\n",
		"[other]\n",
		"command = \"/old\"\n",
		"command = \"\"\"\n# in string\n/old\"\"\"\"\n",
		"command = '''\n# in lit\n/old''''\n",
		"args = [\n",
		"]\n",
		"] # tail\n",
		"\"mcp\",\n",
		"\"serve\",\n",
		"'--device', \"pi\",\n",
		"# comment A\n",
		"  # comment B\n",
		"\"\"\"x\"\"\"\"\",\n",
		"'''y'''',\n",
		"\"\"\"\n# not comment\n\"\"\",\n",
		"args = [\"mcp\", \"serve\", \"--device\", \"x\"]\n",
		"args = [\"stdio\"] # old\n",
		"args = \"mcp serve\"\n",
		"x = 1\n",
		"\n",
		"notes = \"\"\"a\"\"\"\"\" # c\n",
		"args = [ # open\n",
		"\"stdio\", # item\n",
		"k = [\n",
		"\"#\",\n",
		"'[mcp_servers.wendy]',\n",
	}
	f.Add([]byte{0, 4, 7, 10, 13, 11, 8, 22, 1, 21})
	f.Add([]byte{0, 7, 15, 13, 16, 17, 14, 8, 4})
	f.Add([]byte{0, 4, 24, 25, 12, 9, 13})
	f.Add([]byte{0xc0, 4, 19, 1, 21})
	f.Fuzz(func(t *testing.T, sel []byte) {
		if len(sel) > 40 {
			return
		}
		var b strings.Builder
		for i, s := range sel {
			if i == 0 && s&0x80 != 0 {
				b.WriteString("\ufeff")
			}
			b.WriteString(frags[int(s&0x3f)%len(frags)])
		}
		in := b.String()
		if len(sel) > 0 && sel[0]&0x40 != 0 {
			in = strings.ReplaceAll(in, "\n", "\r\n")
		}
		checkUpsertInvariants(t, in)
	})
}

// checkUpsertInvariants asserts what must hold for any input the editor
// accepts: the output is stable on a re-run, keeps every comment (whole
// comment lines verbatim, end-of-line comments at the end of some line) and a
// BOM, and keeps args that already start with "mcp serve".
func checkUpsertInvariants(t *testing.T, in string) {
	out, err := upsertCodexMCPServer([]byte(in), "mcp_servers", "wendy", testWendyBin, testWendyArgs)
	if err != nil {
		return
	}
	outLines := strings.Split(string(out), "\n")
	have := map[string]int{}
	for i, l := range outLines {
		outLines[i] = strings.TrimRight(l, "\r")
		have[outLines[i]]++
	}
	for _, c := range tomlCommentLines(in) {
		if have[c]--; have[c] < 0 {
			t.Fatalf("comment line %q was deleted\n--- in ---\n%q\n--- out ---\n%q", c, in, out)
		}
	}
	need := map[string]int{}
	for _, c := range tomlComments(in) {
		need[c]++
	}
	for c, n := range need {
		got := 0
		for _, l := range outLines {
			if strings.HasSuffix(l, c) {
				got++
			}
		}
		if got < n {
			t.Fatalf("comment %q was deleted\n--- in ---\n%q\n--- out ---\n%q", c, in, out)
		}
	}
	if strings.HasPrefix(in, "\ufeff") && !strings.HasPrefix(string(out), "\ufeff") {
		t.Fatalf("BOM lost\n--- in ---\n%q\n--- out ---\n%q", in, out)
	}
	var before, after map[string]any
	if _, err := toml.Decode(in, &before); err == nil {
		if _, err := toml.Decode(string(out), &after); err != nil {
			t.Fatalf("accepted output does not decode: %v", err)
		}
		be, _ := tomlEntry(before, "mcp_servers", "wendy")
		ae, _ := tomlEntry(after, "mcp_servers", "wendy")
		if be != nil && mcpArgsStartWith(be["args"], testWendyArgs) {
			b, _ := canonicalTOML(map[string]any{"v": be["args"]})
			a, _ := canonicalTOML(map[string]any{"v": ae["args"]})
			if a != b {
				t.Fatalf("args starting with mcp serve were changed\n--- in ---\n%q\n--- out ---\n%q", in, out)
			}
		}
	}
	again, err := upsertCodexMCPServer(out, "mcp_servers", "wendy", testWendyBin, testWendyArgs)
	if err != nil || string(again) != string(out) {
		t.Fatalf("accepted output is not stable (err=%v)\n--- in ---\n%q\n--- out ---\n%q\n--- again ---\n%q", err, in, out, again)
	}
}

// tomlComments returns every comment in src, from its '#' to the end of the
// line, found without the editor's scanner: a '#' starts a comment when
// inserting a character right after it leaves the decoded document unchanged
// (inside a string or key, the insertion would change it). Inputs with many
// '#' are skipped to keep fuzzing fast.
func tomlComments(src string) []string {
	if strings.Count(src, "#") > 40 {
		return nil
	}
	orig, ok := decodeCanonical(src)
	if !ok {
		return nil
	}
	var comments []string
	for p := 0; p < len(src); p++ {
		if src[p] != '#' {
			continue
		}
		if c, ok := decodeCanonical(src[:p+1] + "x" + src[p+1:]); !ok || c != orig {
			continue
		}
		end := strings.IndexByte(src[p:], '\n')
		if end < 0 {
			end = len(src) - p
		}
		comments = append(comments, strings.TrimRight(src[p:p+end], "\r"))
	}
	return comments
}

// decodeCanonical decodes s and renders it canonically for comparison.
func decodeCanonical(s string) (string, bool) {
	var doc map[string]any
	if _, err := toml.Decode(s, &doc); err != nil {
		return "", false
	}
	c, err := canonicalTOML(doc)
	return c, err == nil
}

// tomlCommentLines returns the comment-only lines of src, found without the
// editor's scanner: a line starting with '#' is a comment when changing it
// leaves the decoded document unchanged (inside a multi-line string, the
// change would alter the string).
func tomlCommentLines(src string) []string {
	orig, ok := decodeCanonical(src)
	if !ok {
		return nil
	}
	lines := strings.SplitAfter(src, "\n")
	var comments []string
	for i, l := range lines {
		text := strings.TrimRight(l, "\r\n")
		if !strings.HasPrefix(strings.TrimLeft(strings.TrimPrefix(text, "\ufeff"), " \t"), "#") {
			continue
		}
		changed := strings.Join(lines[:i], "") + text + "x" + l[len(text):] + strings.Join(lines[i+1:], "")
		if c, ok := decodeCanonical(changed); ok && c == orig {
			comments = append(comments, text)
		}
	}
	return comments
}
