package espidftoolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cmakeHead = "cmake_minimum_required(VERSION 3.16)\n"
const cmakeTail = "include($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(blink-rgb)\n"

func TestPrepareSdkconfigDefaultsList(t *testing.T) {
	inserted := sdkconfigDefaultsComment + "\n" + SdkconfigDefaultsLine + "\n\n"
	for _, tc := range []struct {
		name, cmake  string
		noDefaults   bool
		want         string // CMakeLists.txt after Apply, "" for no setup
		wantErr      string
		wantCreation bool
	}{
		{name: "present", cmake: cmakeHead + SdkconfigDefaultsLine + "\n" + cmakeTail},
		{name: "present with other case and spacing", cmake: cmakeHead + "SET( SDKCONFIG_DEFAULTS   \"sdkconfig.defaults.wendy;sdkconfig.defaults\" )\n" + cmakeTail},
		{name: "missing", cmake: cmakeHead + cmakeTail, want: cmakeHead + inserted + cmakeTail},
		{name: "commented out", cmake: cmakeHead + "# " + SdkconfigDefaultsLine + "\n" + cmakeTail, want: cmakeHead + "# " + SdkconfigDefaultsLine + "\n" + inserted + cmakeTail},
		{name: "bracket comment", cmake: cmakeHead + "#[[\n" + SdkconfigDefaultsLine + "\n]]\n" + cmakeTail, want: cmakeHead + "#[[\n" + SdkconfigDefaultsLine + "\n]]\n" + inserted + cmakeTail},
		{name: "indented include", cmake: cmakeHead + "  include( \"$ENV{IDF_PATH}/tools/cmake/project.cmake\" )\nproject(x)\n", want: cmakeHead + inserted + "  include( \"$ENV{IDF_PATH}/tools/cmake/project.cmake\" )\nproject(x)\n"},
		{
			name:  "CRLF",
			cmake: strings.ReplaceAll(cmakeHead+cmakeTail, "\n", "\r\n"),
			want:  strings.ReplaceAll(cmakeHead+inserted+cmakeTail, "\n", "\r\n"),
		},
		{name: "no sdkconfig.defaults", cmake: cmakeHead + SdkconfigDefaultsLine + "\n" + cmakeTail, noDefaults: true, want: cmakeHead + SdkconfigDefaultsLine + "\n" + cmakeTail, wantCreation: true},
		{name: "missing, no sdkconfig.defaults", cmake: cmakeHead + cmakeTail, noDefaults: true, want: cmakeHead + inserted + cmakeTail, wantCreation: true},
		{name: "after the include", cmake: cmakeHead + cmakeTail + SdkconfigDefaultsLine + "\n", wantErr: "after the include"},
		{name: "other value", cmake: cmakeHead + "set(SDKCONFIG_DEFAULTS \"sdkconfig.defaults;my.defaults\")\n" + cmakeTail, wantErr: "another value"},
		{name: "other use", cmake: cmakeHead + SdkconfigDefaultsLine + "\nlist(APPEND SDKCONFIG_DEFAULTS my.defaults)\n" + cmakeTail, wantErr: "another way"},
		{name: "no include", cmake: cmakeHead + "project(x)\n", wantErr: "has no include"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{"CMakeLists.txt": tc.cmake}
			if !tc.noDefaults {
				files["sdkconfig.defaults"] = "CONFIG_X=y\n"
			}
			writeCoreFixture(t, dir, files)
			setup, err := PrepareSdkconfigDefaultsList(dir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), SdkconfigDefaultsLine) {
					t.Fatalf("error = %v, want %q and the line to add", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if setup != nil {
					t.Fatalf("setup = %+v, want none", setup)
				}
				return
			}
			if setup == nil {
				t.Fatal("no setup")
			}
			if setup.CreateDefaults != tc.wantCreation || setup.AddsLine() != (tc.want != tc.cmake) {
				t.Fatalf("CreateDefaults = %v, AddsLine = %v", setup.CreateDefaults, setup.AddsLine())
			}
			if data, _ := os.ReadFile(filepath.Join(dir, "CMakeLists.txt")); string(data) != tc.cmake {
				t.Fatal("preparation wrote CMakeLists.txt")
			}
			if err := setup.Apply(); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(filepath.Join(dir, "CMakeLists.txt")); string(data) != tc.want {
				t.Errorf("CMakeLists.txt =\n%q\nwant\n%q", data, tc.want)
			}
			data, err := os.ReadFile(filepath.Join(dir, "sdkconfig.defaults"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantCreation && string(data) != appDefaultsContent {
				t.Errorf("created sdkconfig.defaults = %q", data)
			}
			// Once applied, the project needs no setup.
			if again, err := PrepareSdkconfigDefaultsList(dir); err != nil || again != nil {
				t.Errorf("after Apply: %+v, %v; want no setup", again, err)
			}
		})
	}
}

func TestSdkconfigDefaultsApplyKeepsConcurrentChanges(t *testing.T) {
	dir := t.TempDir()
	writeCoreFixture(t, dir, map[string]string{"CMakeLists.txt": cmakeHead + cmakeTail})
	setup, err := PrepareSdkconfigDefaultsList(dir)
	if err != nil {
		t.Fatal(err)
	}
	// sdkconfig.defaults appears while the prompt is open: it is kept.
	writeCoreFixture(t, dir, map[string]string{"sdkconfig.defaults": "CONFIG_MINE=y\n"})
	if err := setup.Apply(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "sdkconfig.defaults")); string(data) != "CONFIG_MINE=y\n" {
		t.Errorf("sdkconfig.defaults replaced: %q", data)
	}

	// CMakeLists.txt changes while the prompt is open: no write.
	dir = t.TempDir()
	writeCoreFixture(t, dir, map[string]string{"CMakeLists.txt": cmakeHead + cmakeTail, "sdkconfig.defaults": ""})
	if setup, err = PrepareSdkconfigDefaultsList(dir); err != nil {
		t.Fatal(err)
	}
	edited := "# edited\n" + cmakeHead + cmakeTail
	writeCoreFixture(t, dir, map[string]string{"CMakeLists.txt": edited})
	if err := setup.Apply(); err == nil || !strings.Contains(err.Error(), "changed during setup") {
		t.Fatalf("Apply = %v, want a changed-file error", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "CMakeLists.txt")); string(data) != edited {
		t.Error("Apply overwrote the edited CMakeLists.txt")
	}
}

func TestSdkconfigStale(t *testing.T) {
	dir := t.TempDir()
	if !SdkconfigStale(dir, "sdkconfig.defaults.wendy") {
		t.Error("no sdkconfig: want stale")
	}
	old := time.Now().Add(-time.Hour)
	writeCoreFixture(t, dir, map[string]string{"sdkconfig": "", "sdkconfig.defaults.wendy": "", "CMakeLists.txt": ""})
	for _, name := range []string{"sdkconfig.defaults.wendy", "CMakeLists.txt"} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if SdkconfigStale(dir, "sdkconfig.defaults.wendy", "CMakeLists.txt", "missing.file") {
		t.Error("sdkconfig newer than its inputs: want not stale")
	}
	if err := os.Chtimes(filepath.Join(dir, "CMakeLists.txt"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !SdkconfigStale(dir, "sdkconfig.defaults.wendy", "CMakeLists.txt") {
		t.Error("CMakeLists.txt newer than sdkconfig: want stale")
	}
}

func TestMaskCMakeComments(t *testing.T) {
	in := "set(A \"x # not a comment\") # comment\n#[==[ a\n]] still ]==] set(B)\n"
	want := "set(A \"x # not a comment\")" + strings.Repeat(" ", 10) + "\n" + strings.Repeat(" ", 7) + "\n" + strings.Repeat(" ", 14) + "set(B)\n"
	if got := string(maskCMakeComments([]byte(in))); got != want {
		t.Errorf("maskCMakeComments =\n%q\nwant\n%q", got, want)
	}
}
