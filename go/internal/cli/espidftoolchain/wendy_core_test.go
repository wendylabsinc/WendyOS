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

func writeCoreFixture(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareWendyCore(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
	}{
		{
			name:   "C with existing application",
			source: "#include <stdio.h>\nvoid app_main(void)\n{\n    puts(\"hello\");\n}\n",
			want:   "#include \"wendy_core.h\"\n\n#include <stdio.h>\nvoid app_main(void)\n{\n    ESP_ERROR_CHECK(wendy_core_init());\n\n    puts(\"hello\");\n}\n",
		},
		{
			name:   "C++ with C linkage and comments",
			source: "// void app_main(void) { }\nextern \"C\" void app_main() { /* wendy_core_init(); */ work(); }\n",
			want:   "#include \"wendy_core.h\"\n\n// void app_main(void) { }\nextern \"C\" void app_main() {\n    ESP_ERROR_CHECK(wendy_core_init());\n /* wendy_core_init(); */ work(); }\n",
		},
		{
			name:   "CRLF and commented include",
			source: "/*\r\n#include \"wendy_core.h\"\r\n*/\r\nvoid app_main(void) {}\r\n",
			want:   "#include \"wendy_core.h\"\r\n\r\n/*\r\n#include \"wendy_core.h\"\r\n*/\r\nvoid app_main(void) {\r\n    ESP_ERROR_CHECK(wendy_core_init());\r\n}\r\n",
		},
		{
			name:   "existing initialization",
			source: "#include \"wendy_core.h\"\nvoid app_main(void) { /* first */ ESP_ERROR_CHECK(wendy_core_init()); work(); }\n",
			want:   "#include \"wendy_core.h\"\nvoid app_main(void) { /* first */ ESP_ERROR_CHECK(wendy_core_init()); work(); }\n",
		},
		{
			name:   "existing include",
			source: "#include <wendy_core.h>\nvoid app_main(void) {}\n",
			want:   "#include <wendy_core.h>\nvoid app_main(void) {\n    ESP_ERROR_CHECK(wendy_core_init());\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCoreFixture(t, dir, map[string]string{"main/application.cpp": tc.source})
			setup, err := PrepareWendyCore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if string(setup.updated) != tc.want {
				t.Fatalf("prepared source:\n%s\nwant:\n%s", setup.updated, tc.want)
			}
			original, err := os.ReadFile(filepath.Join(dir, "main/application.cpp"))
			if err != nil || string(original) != tc.source {
				t.Fatalf("preparation changed source: %q, %v", original, err)
			}
			writeCoreFixture(t, dir, map[string]string{"main/application.cpp": tc.want})
			again, err := PrepareWendyCore(dir)
			if err != nil || string(again.updated) != tc.want {
				t.Fatalf("setup is not idempotent: %v", err)
			}
		})
	}
}

func TestPrepareWendyCoreRejectsAmbiguousSource(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		files      map[string]string
	}{
		{"multiple files", "multiple app_main", map[string]string{"main/a.c": "void app_main(void) {}", "main/b.c": "void app_main(void) {}"}},
		{"conditional definitions", "multiple app_main", map[string]string{"main/a.c": "#if X\nvoid app_main(void) {}\n#else\nvoid app_main(void) {}\n#endif"}},
		{"no definition", "could not find app_main", map[string]string{"main/a.c": "void app_main(void); /* void app_main(void) {} */"}},
		{"late initialization", "move the existing", map[string]string{"main/a.c": "void app_main(void) { work(); wendy_core_init(); }"}},
		{"invalid manifest", "reading", map[string]string{"main/a.c": "void app_main(void) {}", "main/idf_component.yml": "dependencies: ["}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCoreFixture(t, dir, tc.files)
			_, err := PrepareWendyCore(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			for name, want := range tc.files {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != want {
					t.Fatalf("preparation changed %s", name)
				}
			}
		})
	}
}

func TestApplyWendyCore(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new dependency", true: "preserves existing dependency"}[existing], func(t *testing.T) {
			dir := t.TempDir()
			manifest := "# Existing dependencies\ndependencies:\n  espressif/led_strip: '^3.0.3'\n"
			if existing {
				manifest += "  wendy_core:\n    path: ../../my-core\n"
			}
			writeCoreFixture(t, dir, map[string]string{"main/main.c": "void app_main(void) {}\n", "main/idf_component.yml": manifest})
			setup, err := PrepareWendyCore(dir)
			if err != nil {
				t.Fatal(err)
			}
			calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if args[1] == "idf.py reconfigure" {
					source, _ := os.ReadFile(filepath.Join(dir, "main/main.c"))
					if !strings.Contains(string(source), "ESP_ERROR_CHECK(wendy_core_init());") {
						t.Fatal("reconfigured before initializing source")
					}
				}
				return exec.CommandContext(ctx, "true")
			})
			if err := setup.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := [][]string{{"eim", "run", "idf.py reconfigure", DefaultVersion}}
			if !existing {
				want = append([][]string{{"eim", "run", "idf.py add-dependency --component main --git " + wendyCoreGitURL + " --git-path components/wendy_core wendy_core", DefaultVersion}}, want...)
			}
			if !reflect.DeepEqual(*calls, want) {
				t.Fatalf("commands = %v, want %v", *calls, want)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "main/idf_component.yml"))
			if string(data) != manifest {
				t.Fatal("setup overwrote manifest instead of using component manager")
			}
		})
	}
}

func TestApplyWendyCoreFailures(t *testing.T) {
	for _, stage := range []string{"add-dependency", "reconfigure", "source changed", "source changed during dependency", "cancelled"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			const source = "void app_main(void) {}\n"
			writeCoreFixture(t, dir, map[string]string{"main/main.c": source})
			setup, err := PrepareWendyCore(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantSource := source
			if stage == "source changed" {
				wantSource += "// edited while prompt was open\n"
				writeCoreFixture(t, dir, map[string]string{"main/main.c": wantSource})
			}
			if stage == "cancelled" {
				cancel()
			}
			calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if stage == "source changed during dependency" && strings.Contains(args[1], "add-dependency") {
					wantSource += "// edited during dependency installation\n"
					writeCoreFixture(t, dir, map[string]string{"main/main.c": wantSource})
				}
				if strings.Contains(args[1], stage) {
					return exec.CommandContext(ctx, "false")
				}
				return exec.CommandContext(ctx, "true")
			})
			if err := setup.Apply(ctx); err == nil {
				t.Fatal("expected failure")
			}
			if stage == "source changed" || stage == "cancelled" {
				if len(*calls) != 0 {
					t.Fatalf("ran commands after %s", stage)
				}
			}
			if stage != "reconfigure" {
				data, _ := os.ReadFile(filepath.Join(dir, "main/main.c"))
				if string(data) != wantSource {
					t.Fatalf("modified source after %s", stage)
				}
			}
		})
	}
}

func TestHasBuildComponent(t *testing.T) {
	for _, tc := range []struct {
		name, description string
		want, wantErr     bool
	}{
		{"missing description", "", false, false},
		{"bare project", `{"build_components":["main","freertos"]}`, false, false},
		{"core without Kconfig flag", `{"build_components":["main","wendy_core"]}`, true, false},
		{"namespaced component", `{"build_components":["main","wendy__wendy_core"]}`, true, false},
		{"other component", `{"build_components":["main","wendy_core_utils"]}`, false, false},
		{"invalid description", `{`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.description != "" {
				writeCoreFixture(t, dir, map[string]string{"build/project_description.json": tc.description})
			}
			got, err := HasBuildComponent(dir, "wendy_core")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
