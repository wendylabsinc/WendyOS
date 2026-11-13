package jobops

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

//go:embed checkpoint.py
var checkpointProgram string

// CheckpointOptions selects the remote runtime and a digest-pinned artifact.
// The runtime executes only the fixed inspector, with host-agent permissions.
type CheckpointOptions struct {
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	Python          string `json:"python"`
	SourceDirectory string `json:"source_directory,omitempty"`
}

func ValidateCheckpoint(opts CheckpointOptions) error {
	for _, value := range []string{opts.Path, opts.Python} {
		if !path.IsAbs(value) || path.Clean(value) == "/" || len(value) > 4096 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("checkpoint path and python must be absolute remote paths")
		}
	}
	if p := opts.SourceDirectory; p != "" && (!path.IsAbs(p) || path.Clean(p) == "/" || len(p) > 4096 || strings.ContainsRune(p, 0)) {
		return fmt.Errorf("source_directory must be an absolute remote project directory")
	}
	digest, err := hex.DecodeString(opts.SHA256)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("checkpoint sha256 must identify the expected artifact")
	}
	return nil
}

// InspectCheckpoint reads bounded metadata and compatibility evidence. It never
// launches training or restores a job. It requires an explicit host runtime;
// like job submission, that capability retains host execution authorization.
func InspectCheckpoint(ctx context.Context, conn *grpcclient.AgentConnection, opts CheckpointOptions) (map[string]any, error) {
	if err := ValidateCheckpoint(opts); err != nil {
		return nil, err
	}
	request := map[string]any{"path": opts.Path, "sha256": opts.SHA256, "python": opts.Python, "source_directory": opts.SourceDirectory, "program": checkpointProgram}
	args, _ := json.Marshal(request)
	const bootstrap = `import json,os,sys;r=json.loads(sys.argv[1]);os.execv(r["python"],[r["python"],"-c",r.pop("program"),json.dumps(r)])`
	return execute(ctx, conn, bootstrap, args, 16384, 90*time.Second)
}
