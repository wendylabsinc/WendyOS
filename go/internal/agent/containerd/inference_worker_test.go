package containerd

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

var testInferenceWorker = inference.WorkerContainer{
	Image:       "ghcr.io/wendylabsinc/wendy-inference-worker@sha256:0000",
	ImageLabels: map[string]string{inference.WorkerImageDepsLabel: "62dfd59926b6a643"},
	Args:        []string{"/opt/wendy-inference/venv/bin/python", "-u", "/data/inference/runtime-1/worker.py"},
	Env:         []string{"HF_HOME=/data/inference/models"},
	Dir:         "/data/inference/runtime-1",
	Mounts:      []inference.WorkerMount{{Path: "/data/inference/runtime-1", ReadOnly: true}, {Path: "/data/inference/models"}},
}

func TestInferenceWorkerIsConfinedToItsOwnMounts(t *testing.T) {
	spec, err := inferenceWorkerSpec(testInferenceWorker, "/fixed/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Root.Readonly || !spec.Process.NoNewPrivileges {
		t.Fatal("the worker needs a read-only root and no new privileges")
	}
	if caps := spec.Process.Capabilities; len(caps.Bounding)+len(caps.Permitted)+len(caps.Effective)+len(caps.Inheritable)+len(caps.Ambient) != 0 {
		t.Fatalf("the worker has capabilities: %+v", caps)
	}
	// Deny-all is the only device rule: no GPU, no camera.
	if devices := spec.Linux.Resources.Devices; len(devices) != 1 || devices[0].Allow {
		t.Fatalf("device rules %+v", devices)
	}
	private := map[string]bool{}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Path != "" {
			t.Fatalf("unexpected namespace join: %+v", ns)
		}
		private[ns.Type] = true
	}
	// The host's network is how it downloads a model; everything else is its own.
	if !private["pid"] || !private["ipc"] || !private["mount"] || !private["uts"] || private["network"] {
		t.Fatalf("namespaces %+v", spec.Linux.Namespaces)
	}
	binds := map[string]string{}
	for _, mount := range spec.Mounts {
		if mount.Type == "bind" {
			binds[mount.Destination] = mount.Source + " " + strings.Join(mount.Options, ",")
		}
	}
	want := map[string]string{
		"/data/inference/runtime-1": "/data/inference/runtime-1 bind,ro,nosuid,nodev,noexec",
		"/data/inference/models":    "/data/inference/models bind,rw,nosuid,nodev,noexec",
		// What host networking brings, as for a host-network app.
		"/sys":             "/sys rbind,nosuid,noexec,nodev,ro",
		"/etc/resolv.conf": "/fixed/resolv.conf rbind,ro",
	}
	if len(binds) != len(want) {
		t.Fatalf("host mounts %v, want %v", binds, want)
	}
	for destination, mount := range want {
		if binds[destination] != mount {
			t.Errorf("mount %s is %q, want %q", destination, binds[destination], mount)
		}
	}
	if !slices.Equal(spec.Process.Args, testInferenceWorker.Args) || spec.Process.Cwd != testInferenceWorker.Dir || !envContains(spec.Process.Env, "HF_HOME=/data/inference/models") {
		t.Fatalf("process %+v", spec.Process)
	}
}

// Every app listing, stats call and restart selects on the app version label.
func TestInferenceWorkerIsNotAnApp(t *testing.T) {
	labels := inferenceWorkerLabels(testInferenceWorker)
	if labels[labelKeyInferenceWorker] != testInferenceWorker.Image {
		t.Fatalf("labels %v", labels)
	}
	for _, key := range []string{labelKeyAppVersion, labelKeyAppID, labelKeyROS2Sidecar, labelKeyROS2HostSidecar, labelKeyROS2SystemSidecar} {
		if _, ok := labels[key]; ok {
			t.Errorf("a model worker carries %s", key)
		}
	}
	// A host shutdown stops every task with a Wendy label.
	if !hasWendyLabel(labels) {
		t.Fatal("a host shutdown would leave the worker running")
	}
}

func TestInferenceImageBuiltForAnotherLockIsRefused(t *testing.T) {
	if err := inferenceImageMismatch(testInferenceWorker, map[string]string{inference.WorkerImageDepsLabel: "62dfd59926b6a643", "other": "label"}); err != nil {
		t.Fatal(err)
	}
	for _, labels := range []map[string]string{nil, {inference.WorkerImageDepsLabel: "0000000000000000"}} {
		// The message is the campaign's reason for running as a process.
		message := fmt.Sprint(inferenceImageMismatch(testInferenceWorker, labels))
		if !strings.Contains(message, `and this agent needs "62dfd59926b6a643"`) {
			t.Fatalf("labels %v: %s", labels, message)
		}
	}
}
