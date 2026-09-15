package espidftoolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIsEspIdfProject(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "empty directory",
			files: nil,
			want:  false,
		},
		{
			name:  "sdkconfig present",
			files: map[string]string{"sdkconfig": "CONFIG_IDF_TARGET=\"esp32c6\"\n"},
			want:  true,
		},
		{
			name:  "sdkconfig.defaults present",
			files: map[string]string{"sdkconfig.defaults": "CONFIG_LOG_DEFAULT_LEVEL_INFO=y\n"},
			want:  true,
		},
		{
			name: "CMakeLists.txt with IDF project include",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.16)\ninclude($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(blink)\n",
			},
			want: true,
		},
		{
			name: "CMakeLists.txt with esp-idf path include",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.16)\ninclude(~/esp/esp-idf/tools/cmake/project.cmake)\nproject(blink)\n",
			},
			want: true,
		},
		{
			name: "CMakeLists.txt with non-IDF project.cmake include",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.16)\ninclude(cmake/project.cmake)\nproject(hello)\n",
			},
			want: false,
		},
		{
			name: "plain CMakeLists.txt",
			files: map[string]string{
				"CMakeLists.txt": "cmake_minimum_required(VERSION 3.16)\nproject(hello)\nadd_executable(hello main.c)\n",
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := IsEspIdfProject(dir); got != tt.want {
				t.Errorf("IsEspIdfProject() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProjectName(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "typical ESP-IDF project",
			content: "cmake_minimum_required(VERSION 3.16)\ninclude($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(blink)\n",
			want:    "blink",
		},
		{
			name:    "project with extra arguments",
			content: "cmake_minimum_required(VERSION 3.16)\nproject(blink VERSION 1.0 LANGUAGES C)\n",
			want:    "blink",
		},
		{
			name:    "uppercase command",
			content: "PROJECT(Blink)\n",
			want:    "Blink",
		},
		{
			name:    "indented with spaces around name",
			content: "  project ( my-app )\n",
			want:    "my-app",
		},
		{
			name:    "commented-out project before real one",
			content: "# project(old)\nproject(new)\n",
			want:    "new",
		},
		{
			name:    "no project declaration",
			content: "cmake_minimum_required(VERSION 3.16)\n",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := ProjectName(dir); got != tt.want {
				t.Errorf("ProjectName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectNameMissingFile(t *testing.T) {
	if got := ProjectName(t.TempDir()); got != "" {
		t.Errorf("ProjectName() = %q, want \"\"", got)
	}
}

func TestProjectTarget(t *testing.T) {
	tests := []struct {
		name    string
		content string // build/config/sdkconfig.json content; "" means no file
		want    string
	}{
		{
			name:    "configured project",
			content: `{"IDF_TARGET": "esp32c6", "IDF_TOOLCHAIN": "gcc"}`,
			want:    "esp32c6",
		},
		{
			name:    "no IDF_TARGET property",
			content: `{"IDF_TOOLCHAIN": "gcc"}`,
			want:    "",
		},
		{
			name:    "invalid JSON",
			content: "not json",
			want:    "",
		},
		{
			name:    "missing file",
			content: "",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" {
				configDir := filepath.Join(dir, "build", "config")
				if err := os.MkdirAll(configDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configDir, "sdkconfig.json"), []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := ProjectTarget(dir); got != tt.want {
				t.Errorf("ProjectTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadSdkconfig(t *testing.T) {
	tests := []struct {
		name    string
		content string // sdkconfig content; "" means no file
		keys    []string
		want    map[string]any
		wantErr bool
	}{
		{
			name: "boolean option, unrequested option left out",
			content: "CONFIG_ESP_CONSOLE_UART_DEFAULT=y\n" +
				"CONFIG_ESP_CONSOLE_SECONDARY_NONE=y\n",
			keys: []string{"CONFIG_ESP_CONSOLE_UART_DEFAULT"},
			want: map[string]any{"CONFIG_ESP_CONSOLE_UART_DEFAULT": true},
		},
		{
			name:    "option that is not set is absent",
			content: "# CONFIG_ESP_CONSOLE_UART_DEFAULT is not set\n",
			keys:    []string{"CONFIG_ESP_CONSOLE_UART_DEFAULT"},
			want:    map[string]any{},
		},
		{
			name:    "string values, an empty one distinct from unset",
			content: "CONFIG_IDF_TARGET=\"esp32c6\"\nCONFIG_WENDY_WIFI_SSID=\"\"\n",
			keys:    []string{"CONFIG_IDF_TARGET", "CONFIG_WENDY_WIFI_SSID"},
			want: map[string]any{
				"CONFIG_IDF_TARGET":      "esp32c6",
				"CONFIG_WENDY_WIFI_SSID": "",
			},
		},
		{
			name:    "unbalanced quote is kept as-is, not stripped",
			content: "CONFIG_IDF_TARGET=\"esp32c6\n",
			keys:    []string{"CONFIG_IDF_TARGET"},
			want: map[string]any{
				"CONFIG_IDF_TARGET": "\"esp32c6",
			},
		},
		{
			name:    "integer values, decimal and hex",
			content: "CONFIG_ESP_CONSOLE_UART_BAUDRATE=115200\nCONFIG_PARTITION_TABLE_OFFSET=0x8000\n",
			keys:    []string{"CONFIG_ESP_CONSOLE_UART_BAUDRATE", "CONFIG_PARTITION_TABLE_OFFSET"},
			want: map[string]any{
				"CONFIG_ESP_CONSOLE_UART_BAUDRATE": 115200,
				"CONFIG_PARTITION_TABLE_OFFSET":    32768,
			},
		},
		{
			name:    "trailing comment after a value is not stripped",
			content: "CONFIG_ESP_CONSOLE_UART_BAUDRATE=115200 # default baud rate\n",
			keys:    []string{"CONFIG_ESP_CONSOLE_UART_BAUDRATE"},
			want: map[string]any{
				"CONFIG_ESP_CONSOLE_UART_BAUDRATE": "115200 # default baud rate",
			},
		},
		{
			name: "deprecated alias is not the requested option",
			content: "# CONFIG_ESP_CONSOLE_UART_DEFAULT is not set\n" +
				"#\n# Deprecated options for backward compatibility\n#\n" +
				"CONFIG_CONSOLE_UART_DEFAULT=y\n# End of deprecated options\n",
			keys: []string{"CONFIG_ESP_CONSOLE_UART_DEFAULT"},
			want: map[string]any{},
		},
		{
			name:    "no keys requested",
			content: "CONFIG_ESP_CONSOLE_UART_DEFAULT=y\n",
			keys:    nil,
			want:    map[string]any{},
		},
		{
			name:    "missing sdkconfig",
			content: "",
			keys:    []string{"CONFIG_ESP_CONSOLE_UART_DEFAULT"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" {
				if err := os.WriteFile(filepath.Join(dir, "sdkconfig"), []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ReadSdkconfig(dir, tt.keys)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ReadSdkconfig() = %#v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadSdkconfig() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReadSdkconfig() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// stubExecCommandContext replaces execCommandContext for the duration of the
// test, recording each invocation and delegating to fake to pick the command
// actually run.
func stubExecCommandContext(t *testing.T, fake func(ctx context.Context, name string, args ...string) *exec.Cmd) *[][]string {
	t.Helper()
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return fake(ctx, name, args...)
	}
	return &calls
}

func TestParseInstalledVersions(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{
			name: "typical eim list output",
			output: "2026-07-08 17:15:30 -  5 - 07 - INFO - Listing installed versions...\n" +
				"Installed versions:\n" +
				"- v6.0.1 [/Users/me/.espressif/v6.0.1/esp-idf]\n" +
				"- v5.5.4 (selected) [/Users/me/.espressif/v5.5.4/esp-idf]\n",
			want: []string{"v6.0.1", "v5.5.4"},
		},
		{
			name:   "no installed versions",
			output: "Installed versions:\n",
			want:   nil,
		},
		{
			name:   "empty output",
			output: "",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseInstalledVersions(tt.output); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseInstalledVersions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureVersion_AlreadyInstalled(t *testing.T) {
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "list" {
			return exec.CommandContext(ctx, "echo", "- "+DefaultVersion+" (selected) [/x]")
		}
		return exec.CommandContext(ctx, "true")
	})

	if err := EnsureVersion(context.Background()); err != nil {
		t.Fatalf("EnsureVersion() unexpected error: %v", err)
	}

	want := [][]string{{"eim", "--version"}, {"eim", "list"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestEnsureVersion_InstallsWhenMissing(t *testing.T) {
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "list" {
			return exec.CommandContext(ctx, "echo", "- v6.0.1 [/x]")
		}
		return exec.CommandContext(ctx, "true")
	})

	if err := EnsureVersion(context.Background()); err != nil {
		t.Fatalf("EnsureVersion() unexpected error: %v", err)
	}

	if len(*calls) != 3 {
		t.Fatalf("expected 3 calls (--version, list, install), got %d: %v", len(*calls), *calls)
	}
	wantInstall := []string{"eim", "install", "-i", DefaultVersion, "-n", "true"}
	if !reflect.DeepEqual((*calls)[2], wantInstall) {
		t.Errorf("install call = %v, want %v", (*calls)[2], wantInstall)
	}
}

func TestEnsureVersion_EimNotFound(t *testing.T) {
	stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
	})

	err := EnsureVersion(context.Background())
	if err == nil {
		t.Fatal("EnsureVersion() expected error when eim is not installed, got nil")
	}
	if !strings.Contains(err.Error(), "eim (ESP-IDF Installation Manager) is not installed") {
		t.Errorf("expected actionable error message, got: %v", err)
	}
}

func TestIdfCommandContext(t *testing.T) {
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})

	IdfCommandContext(context.Background(), "build")
	IdfCommandContext(context.Background(), "-DFOO=a b", "it's", "")

	want := [][]string{
		{"eim", "run", "idf.py build", DefaultVersion},
		{"eim", "run", `idf.py '-DFOO=a b' 'it'"'"'s' ''`, DefaultVersion},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}
