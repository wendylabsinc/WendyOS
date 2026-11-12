//go:build linux

package espidftoolchain

import (
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This is the production Mosaico image, also containing the stable C6 target.
// No device, privileged container, alternate entrypoint or synthetic idf.py is used.
const ownershipIDFImage = "espressif/idf@sha256:30b42a38415e918b1b14825ccf0e4c40e7910c5132618dcd12e3c45361be0943"

func TestProjectToolchainDockerOwnership(t *testing.T) {
	if os.Getenv("WENDY_TEST_IDF_DOCKER_OWNERSHIP") != "1" {
		t.Skip("requires explicit native rootful Docker fixture")
	}
	if os.Getuid() <= 0 {
		t.Fatal("fixture must run as a non-root invoking user")
	}
	for _, outcome := range []string{"success", "failure", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			workspace := t.TempDir()
			project := filepath.Join(workspace, "project with spaces")
			if err := os.MkdirAll(filepath.Join(project, "main"), 0755); err != nil {
				t.Fatal(err)
			}
			// Record actual production Docker invocations, without substituting
			// their execution, so even a failed assertion removes only our containers.
			var containers []string
			original := execCommandContext
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name == "docker" && len(args) > 0 && args[0] == "run" {
					for i := 1; i+1 < len(args); i++ {
						if args[i] == "--name" {
							containers = append(containers, args[i+1])
						}
					}
				}
				return exec.CommandContext(ctx, name, args...)
			}
			t.Cleanup(func() { execCommandContext = original })
			t.Cleanup(func() {
				cleanupSafe := true
				for _, name := range containers {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					data, err := exec.CommandContext(ctx, "docker", "inspect", name).CombinedOutput()
					cancel()
					if err != nil {
						if strings.Contains(strings.ToLower(string(data)), "no such object") || strings.Contains(strings.ToLower(string(data)), "no such container") {
							continue
						}
						t.Errorf("inspect owned container: %v: %s", err, data)
						cleanupSafe = false
						continue
					}
					var records []struct {
						Mounts []struct{ Source string }
					}
					if json.Unmarshal(data, &records) != nil || len(records) != 1 || len(records[0].Mounts) != 1 ||
						(records[0].Mounts[0].Source != workspace && records[0].Mounts[0].Source != project) {
						t.Errorf("refusing cleanup of container with unexpected workspace: %s", name)
						cleanupSafe = false
						continue
					}
					ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
					data, err = exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput()
					cancel()
					if err != nil {
						t.Errorf("remove owned container: %v: %s", err, data)
						cleanupSafe = false
					}
				}
				if !cleanupSafe {
					return
				}
				// The mutant deliberately leaves root-owned outputs. Restore this
				// one private workspace so TempDir cleanup works for the runner.
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				data, err := dockerRunCommand(ctx, "--network=none", "-v", workspace+":/fixture", "--entrypoint", "find",
					ownershipIDFImage, "-P", "/fixture", "-xdev", "-exec", "chown", "-h", "--",
					fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "{}", "+").CombinedOutput()
				if err != nil {
					t.Errorf("restore fixture workspace for removal: %v: %s", err, data)
				}
			})
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(project, path), []byte(value), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write("wendy-idf.json", fmt.Sprintf(`{"image":%q,"workspaceRoot":"..","target":"esp32c6"}`, ownershipIDFImage))
			write("main/main.c", "void app_main(void) {}\n")
			write("main/CMakeLists.txt", "idf_component_register(SRCS main.c INCLUDE_DIRS \".\")\n")
			write("CMakeLists.txt", `cmake_minimum_required(VERSION 3.16)
set(COMPONENTS main)
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(ownership_fixture)
execute_process(COMMAND id -u OUTPUT_VARIABLE BUILD_UID OUTPUT_STRIP_TRAILING_WHITESPACE COMMAND_ERROR_IS_FATAL ANY)
if(NOT BUILD_UID STREQUAL "0")
  message(FATAL_ERROR "ESP-IDF build did not execute as root")
endif()
file(WRITE "${CMAKE_BINARY_DIR}/build-uid" "${BUILD_UID}")
file(WRITE "${CMAKE_SOURCE_DIR}/unrelated-root.txt" "unrelated root source")
file(WRITE "${CMAKE_SOURCE_DIR}/../outside-root.txt" "outside symlink target")
execute_process(COMMAND ${CMAKE_COMMAND} -E create_symlink ../../outside-root.txt "${CMAKE_BINARY_DIR}/outside-link" COMMAND_ERROR_IS_FATAL ANY)
file(WRITE "${CMAKE_BINARY_DIR}/other-owner" "preserve another owner")
execute_process(COMMAND chown 65534:65534 "${CMAKE_BINARY_DIR}/other-owner" COMMAND_ERROR_IS_FATAL ANY)
file(MAKE_DIRECTORY "${CMAKE_SOURCE_DIR}/managed_components")
file(WRITE "${CMAKE_SOURCE_DIR}/managed_components/ownership-sentinel" "generated as root")
if(EXISTS "${CMAKE_SOURCE_DIR}/fail-configure")
  message(FATAL_ERROR "intentional ownership fixture configure failure")
endif()
if(EXISTS "${CMAKE_SOURCE_DIR}/cancel-configure")
  file(WRITE "${CMAKE_BINARY_DIR}/cancel-ready" "ready")
  execute_process(COMMAND ${CMAKE_COMMAND} -E sleep 600)
endif()
`)
			if outcome == "failure" {
				write("fail-configure", "fail")
			} else if outcome == "cancelled" {
				write("cancel-configure", "cancel")
			}
			toolchain, err := LoadProjectToolchain(project, "esp32c6")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := toolchain.Ensure(ctx); err != nil {
				t.Fatal(err)
			}
			if toolchain.outputOwner != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) {
				t.Fatal("requires native Linux rootful Docker without user namespaces")
			}
			watchDone := make(chan struct{})
			if outcome == "cancelled" {
				go func() {
					defer close(watchDone)
					tick := time.NewTicker(20 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-tick.C:
							if _, err := os.Stat(filepath.Join(project, "build/cancel-ready")); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			} else {
				close(watchDone)
			}
			err = toolchain.Run(ctx, project, "-DIDF_TARGET=esp32c6", "gen_project_binary")
			cancel()
			<-watchDone
			if (err == nil) != (outcome == "success") {
				t.Fatalf("unexpected %s build result: %v", outcome, err)
			}
			if data, err := os.ReadFile(filepath.Join(project, "build/build-uid")); err != nil || string(data) != "0" {
				t.Fatalf("actual root ESP-IDF configure did not execute: %q %v", data, err)
			}
			if outcome == "cancelled" {
				if _, err := os.Stat(filepath.Join(project, "build/cancel-ready")); err != nil {
					t.Fatal("cancellation did not reach the live configure process")
				}
			}
			owner := func(path string, uid, gid uint32) {
				t.Helper()
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				stat := info.Sys().(*syscall.Stat_t)
				if stat.Uid != uid || stat.Gid != gid {
					t.Errorf("ESP-IDF output owner mismatch: %s got %d:%d want %d:%d", path, stat.Uid, stat.Gid, uid, gid)
				}
			}
			for _, relative := range []string{"build", "managed_components", "sdkconfig"} {
				if err := filepath.Walk(filepath.Join(project, relative), func(path string, _ os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if path == filepath.Join(project, "build/other-owner") {
						owner(path, 65534, 65534)
					} else {
						owner(path, uint32(os.Getuid()), uint32(os.Getgid()))
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			owner(filepath.Join(project, "unrelated-root.txt"), 0, 0)
			owner(filepath.Join(workspace, "outside-root.txt"), 0, 0)
			owner(filepath.Join(project, "main/main.c"), uint32(os.Getuid()), uint32(os.Getgid()))
			if outcome == "success" {
				firmware, err := elf.Open(filepath.Join(project, "build/ownership_fixture.elf"))
				if err != nil {
					t.Fatal(err)
				}
				defer firmware.Close()
				if firmware.Machine != elf.EM_RISCV || firmware.Entry == 0 {
					t.Fatal("ESP-IDF did not produce executable RISC-V firmware")
				}
				binary, err := os.ReadFile(filepath.Join(project, "build/ownership_fixture.bin"))
				if err != nil || len(binary) < 100 || binary[0] != 0xe9 {
					t.Fatalf("ESP-IDF app image missing or invalid: %v", err)
				}
			}
		})
	}
}
