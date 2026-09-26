package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func projectTestCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "wendy", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "JSON output")
	root.AddCommand(newProjectCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"project"}, args...))
	err := root.Execute()
	return output.String(), err
}

func projectTestSetup(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := writeWendyJSON(t, body)
	originalJSON, originalInteractive := jsonOutput, isInteractiveTerminalFn
	jsonOutput = false
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { jsonOutput, isInteractiveTerminalFn = originalJSON, originalInteractive })
	return dir, filepath.Join(dir, "wendy.json")
}

func readProjectTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProjectDirectWorkflowPreservesManifest(t *testing.T) {
	_, path := projectTestSetup(t, `{
  "$schema":"https://wendy.dev/schemas/wendy.json",
  "appId":"robot", "version":"1.0", "future":{"counter":9007199254740993},
  "frameworks":{"future":{"enabled":true},"ros2":{"domainId":42,"futureField":"keep"}},
  "entitlements":[{"type":"persist","name":"recordings","path":"/data","futureFlag":true}],
  "services":{"vision":{"context":"vision","futureSetting":"keep"}}
}`)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "http", "--port", "8080"},
		{"edit", "ros2", "--domain-id", "0"},
		{"edit", "persist", "--path", "/recordings"},
		{"add", "camera", "--service", "vision"},
		{"remove", "http"},
	} {
		if _, err := projectTestCommand(t, append(args, "--file", path)...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	data := readProjectTestFile(t, path)
	for _, want := range []string{`9007199254740993`, `"$schema"`, `"futureFlag": true`, `"futureField": "keep"`, `"futureSetting": "keep"`, `"domainId": 0`, `"path": "/recordings"`} {
		if !strings.Contains(data, want) {
			t.Errorf("lost %s in %s", want, data)
		}
	}
	if strings.Contains(data, `"http"`) {
		t.Errorf("HTTP was not removed: %s", data)
	}
	doc, err := loadProjectManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := doc.scope("vision")
	if !projectHasFeature(scope, "camera") || projectHasFeature(doc.root, "camera") {
		t.Fatal("camera was not scoped to vision")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions changed to %v", info.Mode())
	}
}

func TestProjectRejectsInvalidEditsWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"missing port", []string{"add", "http"}, "--port"},
		{"bad port", []string{"add", "http", "--port", "70000"}, "65535"},
		{"missing storage path", []string{"add", "persist", "--name", "data"}, "--path"},
		{"invalid storage path", []string{"add", "persist", "--name", "data", "--path", "relative"}, "absolute"},
		{"bad domain", []string{"add", "ros2", "--domain-id", "233"}, "232"},
		{"bad middleware", []string{"add", "ros2", "--rmw", "typo"}, "rmw"},
		{"bad device", []string{"add", "serial", "--serial-device", "../../ttyUSB0"}, "serial device"},
		{"mesh CIDR", []string{"add", "network", "--mode", "mesh"}, "--service-cidr"},
		{"wrong flag", []string{"add", "camera", "--domain-id", "42"}, "does not use --domain-id"},
		{"unknown service", []string{"add", "camera", "--service", "typo"}, "unknown service"},
		{"no name", []string{"add"}, "specify what to add"},
		{"headless edit", []string{"edit", "ros2"}, "specify fields"},
		{"headless editor", []string{"edit", "--raw"}, "interactive terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, path := projectTestSetup(t, `{"appId":"robot","frameworks":{"ros2":{}}}`)
			before := readProjectTestFile(t, path)
			if test.name == "bad domain" || test.name == "bad middleware" {
				test.args[0] = "edit"
			}
			_, err := projectTestCommand(t, append(test.args, "--file", path)...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
			if after := readProjectTestFile(t, path); after != before {
				t.Fatal("invalid edit changed the file")
			}
		})
	}
}

func TestProjectDryRunAndJSONNeverPrompt(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot","entitlements":[{"type":"camera","password":"camera-secret-123"}]}`)
	oldPick, oldPrompt, oldConfirm := projectPick, projectPrompt, projectConfirm
	t.Cleanup(func() { projectPick, projectPrompt, projectConfirm = oldPick, oldPrompt, oldConfirm })
	projectPick = func(string, []tui.PickerItem) (string, error) { t.Fatal("unexpected picker"); return "", nil }
	projectPrompt = func(string, string, string, tui.ValidateFunc) (string, error) {
		t.Fatal("unexpected prompt")
		return "", nil
	}
	projectConfirm = func(string) (bool, error) { t.Fatal("unexpected confirmation"); return false, nil }
	// --json must suppress prompts even when a TTY is available.
	isInteractiveTerminalFn = func() bool { return true }
	before := readProjectTestFile(t, path)
	output, err := projectTestCommand(t, "edit", "camera", "--allowlist", "camera-1", "--dry-run", "--json", "--file", path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("not JSON: %s: %v", output, err)
	}
	if result["saved"] != false || result["dryRun"] != true {
		t.Fatalf("bad result: %s", output)
	}
	if strings.Contains(output, "camera-secret-123") {
		t.Fatal("preview exposed the camera password")
	}
	if readProjectTestFile(t, path) != before {
		t.Fatal("dry run wrote the file")
	}
	output, err = projectTestCommand(t, "show", "--json", "--file", path)
	if err != nil || !json.Valid([]byte(output)) {
		t.Fatalf("show JSON: %s, %v", output, err)
	}
	output, err = projectTestCommand(t, "--json", "--file", path)
	if err != nil || !json.Valid([]byte(output)) {
		t.Fatalf("bare project JSON: %s, %v", output, err)
	}
}

func TestProjectRepeatedEntriesRequireSelection(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot","entitlements":[{"type":"persist","name":"a","path":"/a"},{"type":"persist","name":"b","path":"/b"}]}`)
	if _, err := projectTestCommand(t, "remove", "persist", "--file", path); err == nil || !strings.Contains(err.Error(), "--entry") {
		t.Fatalf("error = %v", err)
	}
	if _, err := projectTestCommand(t, "edit", "persist", "--entry", "1", "--path", "/new-b", "--file", path); err != nil {
		t.Fatal(err)
	}
	if _, err := projectTestCommand(t, "remove", "persist", "--entry", "0", "--file", path); err != nil {
		t.Fatal(err)
	}
	data := readProjectTestFile(t, path)
	if strings.Contains(data, `"/a"`) || !strings.Contains(data, `"/new-b"`) {
		t.Fatal(data)
	}
}

func TestProjectLegacyCommandsUseSafeWriter(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot","future":9007199254740993}`)
	for _, args := range [][]string{
		{"entitlements", "add", "http", "--port", "8080"},
		{"entitlements", "edit", "http", "--port", "8081"},
		{"frameworks", "add", "ros2"},
		{"frameworks", "edit", "ros2", "--domain-id", "42"},
		{"frameworks", "remove", "ros2"},
	} {
		if _, err := projectTestCommand(t, append(args, "--file", path)...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	data := readProjectTestFile(t, path)
	if !strings.Contains(data, "9007199254740993") || !strings.Contains(data, "8081") {
		t.Fatal(data)
	}
	if _, err := projectTestCommand(t, "entitlements", "list", "--file", path); err != nil {
		t.Fatal(err)
	}
}

func TestProjectComposeServiceAndValidation(t *testing.T) {
	dir, path := projectTestSetup(t, `{"appId":"robot"}`)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services:\n  vision:\n    image: python\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "http", "--port", "8080", "--service", "vision"},
		{"add", "ros2", "--domain-id", "42", "--service", "vision"},
		{"validate"},
	} {
		if _, err := projectTestCommand(t, append(args, "--file", path)...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	before := readProjectTestFile(t, path)
	if strings.Contains(before, `"context"`) {
		t.Fatal("invented a Compose build context")
	}
	if _, err := projectTestCommand(t, "edit", "ros2", "--rmw", "typo", "--service", "vision", "--file", path); err == nil {
		t.Fatal("accepted invalid service framework")
	}
	if readProjectTestFile(t, path) != before {
		t.Fatal("invalid Compose edit wrote the file")
	}
	output, err := projectTestCommand(t, "show", "--file", path)
	if err != nil || !strings.Contains(output, "Service vision") || !strings.Contains(output, "8080") {
		t.Fatalf("%s: %v", output, err)
	}
}

func TestProjectDetectsConcurrentEditAndPreservesSymlink(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot"}`)
	doc, err := loadProjectManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.root["version"] = "2"
	if err := os.WriteFile(path, []byte(`{"appId":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := doc.save(); err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("error = %v", err)
	}
	if got := readProjectTestFile(t, path); got != `{"appId":"other"}` {
		t.Fatal(got)
	}
	link := filepath.Join(filepath.Dir(path), "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skip(err)
	}
	if _, err := projectTestCommand(t, "edit", "app", "--version", "3", "--file", link); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(link)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replaced the symlink")
	}
	if !strings.Contains(readProjectTestFile(t, path), `"version": "3"`) {
		t.Fatal("did not update symlink target")
	}
}

func TestProjectGuidedWorkflowStagesAndSaves(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot"}`)
	before := readProjectTestFile(t, path)
	isInteractiveTerminalFn = func() bool { return true }
	oldPick, oldPrompt, oldConfirm := projectPick, projectPrompt, projectConfirm
	t.Cleanup(func() { projectPick, projectPrompt, projectConfirm = oldPick, oldPrompt, oldConfirm })
	choices := []string{"add", "http", "add", "ros2", "save"}
	projectPick = func(_ string, items []tui.PickerItem) (string, error) {
		if readProjectTestFile(t, path) != before {
			t.Fatal("saved before review")
		}
		if len(choices) == 0 {
			t.Fatal("unexpected picker")
		}
		choice := choices[0]
		choices = choices[1:]
		for _, item := range items {
			if item.Value == choice {
				return choice, nil
			}
		}
		t.Fatalf("choice %s was missing", choice)
		return "", nil
	}
	projectPrompt = func(label, hint, value string, validate tui.ValidateFunc) (string, error) {
		if label == "Port" {
			value = "8080"
		}
		if label == "Domain ID" {
			value = "42"
		}
		if err := validate(value); err != nil {
			t.Fatal(err)
		}
		return value, nil
	}
	projectConfirm = func(string) (bool, error) {
		if readProjectTestFile(t, path) != before {
			t.Fatal("saved before confirmation")
		}
		return true, nil
	}
	output, err := projectTestCommand(t, "--file", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Changes to") {
		t.Fatal("missing preview")
	}
	data := readProjectTestFile(t, path)
	if !strings.Contains(data, `"port": 8080`) || !strings.Contains(data, `"domainId": 42`) {
		t.Fatal(data)
	}
}

func TestProjectGuidedCancellationWritesNothing(t *testing.T) {
	for _, cancel := range []string{"prompt", "review"} {
		t.Run(cancel, func(t *testing.T) {
			_, path := projectTestSetup(t, `{"appId":"robot"}`)
			before := readProjectTestFile(t, path)
			isInteractiveTerminalFn = func() bool { return true }
			oldPrompt, oldConfirm := projectPrompt, projectConfirm
			t.Cleanup(func() { projectPrompt, projectConfirm = oldPrompt, oldConfirm })
			projectPrompt = func(string, string, string, tui.ValidateFunc) (string, error) {
				if cancel == "prompt" {
					return "", tui.ErrCancelled
				}
				return "8080", nil
			}
			projectConfirm = func(string) (bool, error) { return false, nil }
			_, err := projectTestCommand(t, "add", "http", "--file", path)
			if cancel == "prompt" && !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("error = %v", err)
			}
			if readProjectTestFile(t, path) != before {
				t.Fatal("cancelled edit changed the file")
			}
		})
	}
}

func TestProjectValidationJSONReportsFailure(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot","entitlements":[{"type":"http"}]}`)
	output, err := projectTestCommand(t, "validate", "--json", "--file", path)
	if err == nil {
		t.Fatal("expected invalid manifest")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("%s: %v", output, err)
	}
	if result["valid"] != false || !strings.Contains(result["error"].(string), "port") {
		t.Fatal(output)
	}
}

func TestProjectMissingManifestAndComposeOverview(t *testing.T) {
	dir := t.TempDir()
	oldJSON, oldInteractive := jsonOutput, isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { jsonOutput, isInteractiveTerminalFn = oldJSON, oldInteractive })
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := projectTestCommand(t, "--file", dir)
	if err != nil || !strings.Contains(output, "optional") || !strings.Contains(output, "web") {
		t.Fatalf("%s: %v", output, err)
	}
	if _, err := projectTestCommand(t, "add", "camera", "--file", dir); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wendy.json")); !os.IsNotExist(err) {
		t.Fatal("read-only command created a manifest")
	}
}

func TestProjectHelpFocusesOnSelectedFeature(t *testing.T) {
	projectTestSetup(t, `{"appId":"robot"}`)
	output, err := projectTestCommand(t, "add", "http", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "--port string") || strings.Contains(output, "--domain-id string") || strings.Contains(output, "--app-id string") {
		t.Fatalf("help does not focus on HTTP fields:\n%s", output)
	}
	output, err = projectTestCommand(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"show", "add", "edit", "remove", "validate"} {
		if !strings.Contains(output, name) {
			t.Errorf("missing %s", name)
		}
	}
	if strings.Contains(output, "Manage project entitlements") {
		t.Fatal("compatibility group crowded the main help")
	}
}

func TestProjectGuidedCreationOnlyWritesManifest(t *testing.T) {
	dir, path := projectTestSetup(t, `{"appId":"placeholder"}`)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	isInteractiveTerminalFn = func() bool { return true }
	oldPick, oldPrompt, oldConfirm := projectPick, projectPrompt, projectConfirm
	t.Cleanup(func() { projectPick, projectPrompt, projectConfirm = oldPick, oldPrompt, oldConfirm })
	choices := []string{"create", "save"}
	projectPick = func(string, []tui.PickerItem) (string, error) {
		if len(choices) == 0 {
			t.Fatal("unexpected picker")
		}
		choice := choices[0]
		choices = choices[1:]
		return choice, nil
	}
	projectPrompt = func(label, hint, value string, validate tui.ValidateFunc) (string, error) {
		if label == "App ID" {
			value = "existing-project"
		}
		return value, validate(value)
	}
	projectConfirm = func(string) (bool, error) { return true, nil }
	if _, err := projectTestCommand(t, "--file", dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "wendy.json" {
		t.Fatalf("unexpected files: %v", entries)
	}
	if !strings.Contains(readProjectTestFile(t, path), `"appId": "existing-project"`) {
		t.Fatal("missing manifest")
	}
}

func TestProjectRawRepairAndInvalidResult(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "invalid"}[valid], func(t *testing.T) {
			_, path := projectTestSetup(t, `{"appId":`)
			isInteractiveTerminalFn = func() bool { return true }
			oldEditor, oldConfirm := projectOpenEditor, projectConfirm
			t.Cleanup(func() { projectOpenEditor, projectConfirm = oldEditor, oldConfirm })
			projectOpenEditor = func(_ *cobra.Command, doc *projectManifest) error {
				if string(doc.original) != `{"appId":` {
					t.Fatal("lost original malformed content")
				}
				doc.root = projectObject{"appId": "repaired"}
				if !valid {
					doc.root["appId"] = "invalid ID"
				}
				return nil
			}
			projectConfirm = func(string) (bool, error) { return true, nil }
			_, err := projectTestCommand(t, "edit", "--raw", "--file", path)
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid([]byte(readProjectTestFile(t, path))) {
					t.Fatal("did not repair JSON")
				}
			} else {
				if err == nil {
					t.Fatal("accepted invalid result")
				}
				if readProjectTestFile(t, path) != `{"appId":` {
					t.Fatal("overwrote manifest with invalid result")
				}
			}
		})
	}
}

func TestProjectPreviewReportsOnlyChangedFields(t *testing.T) {
	a, _ := parseProjectObject([]byte(`{"env":{"API_TOKEN":"old"},"entitlements":[{"type":"http","port":8080}]}`))
	b, _ := parseProjectObject([]byte(`{"env":{"API_TOKEN":"new"},"entitlements":[{"type":"http","port":8081}],"future":null}`))
	changes := projectChanges(a, b, "")
	if len(changes) != 3 {
		t.Fatalf("changes = %+v", changes)
	}
	encoded, _ := json.Marshal(changes)
	if !bytes.Contains(encoded, []byte("/entitlements/0/port")) || bytes.Contains(encoded, []byte(`"old"`)) || bytes.Contains(encoded, []byte(`"new"`)) {
		t.Fatalf("bad preview: %s", encoded)
	}
}

func TestProjectNetworkDefaultsAndModeChange(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"robot"}`)
	for _, args := range [][]string{
		{"add", "network"},
		{"edit", "network", "--mode", "mesh", "--service-cidr", "10.42.0.0/16"},
		{"edit", "network", "--mode", "bridge"},
	} {
		if _, err := projectTestCommand(t, append(args, "--file", path)...); err != nil {
			t.Fatal(err)
		}
	}
	data := readProjectTestFile(t, path)
	if strings.Contains(data, "serviceCIDR") || !strings.Contains(data, `"mode": "bridge"`) {
		t.Fatal(data)
	}
}
