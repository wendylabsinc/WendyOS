package jobops

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

//go:embed submit.py
var submitProgram string

//go:embed worker.py
var workerProgram string

// Step is one bounded argv execution, with output kept in its owned job directory.
type Step struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	MetricsFile    string   `json:"metrics_file,omitempty"`
}

// SourceManifest references a hash-pinned remote source contract. File payloads
// and large inventories stay outside tool arguments and model context.
type SourceManifest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Spec names a durable job and the source checks to perform before launch.
type Spec struct {
	Name             string            `json:"name"`
	WorkingDirectory string            `json:"working_directory"`
	Steps            []Step            `json:"steps"`
	SourceHashes     map[string]string `json:"source_hashes,omitempty"`
	SourceManifest   *SourceManifest   `json:"source_manifest,omitempty"`
}

var jobName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateSpec rejects invalid jobs before opening a connection or writing files.
func ValidateSpec(spec Spec) error {
	if !jobName.MatchString(spec.Name) || !path.IsAbs(spec.WorkingDirectory) || path.Clean(spec.WorkingDirectory) == "/" || strings.ContainsRune(spec.WorkingDirectory, 0) || len(spec.WorkingDirectory) > 4096 || len(spec.Steps) < 1 || len(spec.Steps) > 32 {
		return fmt.Errorf("spec requires a valid name, absolute working_directory and 1..32 steps")
	}
	names := map[string]bool{}
	size := 0
	for _, step := range spec.Steps {
		if step.Name == "runner" {
			return fmt.Errorf("step name %q is reserved for the job supervisor", step.Name)
		}
		if !jobName.MatchString(step.Name) || names[step.Name] || len(step.Argv) == 0 || step.Argv[0] == "" || step.TimeoutSeconds < 1 || step.TimeoutSeconds > 86400 {
			return fmt.Errorf("each step needs a unique name, argv and timeout_seconds in 1..86400")
		}
		if name := step.MetricsFile; name != "" && (path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || len(name) > 1024 || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "../")) {
			return fmt.Errorf("metrics_file must be a safe path relative to its owned step directory")
		}
		names[step.Name] = true
		for _, arg := range step.Argv {
			size += len(arg)
			if strings.ContainsRune(arg, 0) || len(arg) > 8192 {
				return fmt.Errorf("invalid argv")
			}
		}
	}
	if size > 65536 || len(spec.SourceHashes) > 64 {
		return fmt.Errorf("job specification exceeds its argument or source-file limit")
	}
	if spec.SourceManifest != nil {
		name := spec.SourceManifest.Path
		raw, err := hex.DecodeString(spec.SourceManifest.SHA256)
		if err != nil || len(raw) != 32 || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || len(name) > 1024 || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "../") {
			return fmt.Errorf("source_manifest needs a safe relative path and SHA-256 digest")
		}
	}
	for name, digest := range spec.SourceHashes {
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != 32 || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || len(name) > 1024 || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "../") {
			return fmt.Errorf("source_hashes needs relative paths and SHA-256 digests")
		}
	}
	return nil
}

// Submit creates a named, detached job. Repeating the same name/spec returns the
// original receipt; a different spec never overwrites or restarts that job.
func Submit(ctx context.Context, conn *grpcclient.AgentConnection, spec Spec) (map[string]any, error) {
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	args, _ := json.Marshal(map[string]any{"spec": spec, "worker_source": workerProgram})
	if len(args) > 120000 {
		return nil, fmt.Errorf("serialized job specification exceeds 120000 bytes")
	}
	receipt, err := execute(ctx, conn, submitProgram, args, 32768, 120*time.Second)
	if err != nil {
		return nil, fmt.Errorf("no confirmed job receipt; inspect .wendy/jobs/%s or retry the identical name/spec before starting another job: %w", spec.Name, err)
	}
	return receipt, nil
}
