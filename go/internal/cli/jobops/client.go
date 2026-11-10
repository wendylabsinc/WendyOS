// Package jobops provides bounded, read-only inspection of existing host jobs.
// CLI and MCP use the same fixed host program and authenticated transport.
package jobops

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

//go:embed probe.py
var program string

// InspectOptions bounds how long an agent waits for a changed job snapshot.
type InspectOptions struct {
	Since         string
	WaitSeconds   int
	Target        string
	UntilTerminal bool
}

// Inspect returns recorded progress plus independently checked process identity.
// It never starts, resumes, stops, or rewrites a job. A missing/stale status file
// is an error or unknown observation, never proof of successful completion.
func Inspect(ctx context.Context, conn *grpcclient.AgentConnection, jobPath string, opts InspectOptions) (map[string]any, error) {
	if err := ValidateInspect(jobPath, opts); err != nil {
		return nil, err
	}
	target := opts.Target
	if target == "" && conn != nil {
		target = conn.MeshHost
		if target == "" {
			target = conn.Addr
		}
		if target == "" {
			target = conn.Host
		}
	}
	args, _ := json.Marshal(map[string]any{"path": jobPath, "since": opts.Since, "wait_seconds": opts.WaitSeconds, "target": target, "until_terminal": opts.UntilTerminal})
	return execute(ctx, conn, program, args, 32768, time.Duration(opts.WaitSeconds+10)*time.Second)
}

func execute(ctx context.Context, conn *grpcclient.AgentConnection, script string, args []byte, limit int, timeout time.Duration) (map[string]any, error) {
	if conn == nil || conn.ShellService == nil {
		return nil, fmt.Errorf("connect to a device with host-job inspection support")
	}
	if !conn.IsMTLS {
		return nil, fmt.Errorf("host-job inspection requires an authenticated mTLS connection")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stream, err := conn.ShellService.HostShell(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Start_{Start: &agentpb.HostShellRequest_Start{Command: []string{"python3", "-c", script, string(args)}}}})
	if err != nil {
		return nil, err
	}
	if err = stream.CloseSend(); err != nil {
		return nil, err
	}
	var output strings.Builder
	exited := false
	exitCode := int32(0)
	for {
		msg, e := stream.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if data := msg.GetStdoutData(); len(data) > 0 {
			if output.Len()+len(data) > limit {
				return nil, fmt.Errorf("host-job response exceeds %d bytes", limit)
			}
			output.Write(data)
		}
		if _, ok := msg.GetResponseType().(*agentpb.HostShellResponse_ExitCode); ok {
			exitCode = msg.GetExitCode()
			exited = true
			break
		}
	}
	if !exited {
		return nil, fmt.Errorf("host-job inspection ended without an exit status")
	}
	var result map[string]any
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(line, "WENDY_JOB ") {
			if result != nil {
				return nil, fmt.Errorf("multiple host-job response frames")
			}
			if e := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "WENDY_JOB "))), &result); e != nil {
				return nil, fmt.Errorf("invalid host-job response: %w", e)
			}
		}
	}
	if result == nil {
		return nil, fmt.Errorf("host-job inspection returned no structured result (python3 and a readable status.json are required)")
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("host-job inspection: %v", result["error"])
	}
	return result, nil
}

// ValidateInspect checks arguments without opening a connection or reading files.
func ValidateInspect(jobPath string, opts InspectOptions) error {
	if !path.IsAbs(jobPath) || path.Clean(jobPath) == "/" || len(jobPath) > 4096 || strings.ContainsRune(jobPath, 0) {
		return fmt.Errorf("job_path must be an absolute job directory")
	}
	if opts.WaitSeconds < 0 || opts.WaitSeconds > 30 || len(opts.Since) > 128 || (opts.WaitSeconds > 0 && opts.Since == "" && !opts.UntilTerminal) {
		return fmt.Errorf("wait_seconds must be 0..30 and requires a since cursor")
	}

	return nil
}
