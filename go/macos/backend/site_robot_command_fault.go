package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type siteCommandFault struct {
	At         time.Time         `json:"at"`
	Command    pb.UnitreeCommand `json:"command"`
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	DurationMS int64             `json:"durationMS"`
}

func siteCommandError(err error) (codes.Code, string) {
	code, message := status.Code(err), status.Convert(err).Message()
	if errors.Is(err, context.DeadlineExceeded) {
		code = codes.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		code = codes.Canceled
	}
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 512 {
		message = message[:512]
	}
	return code, message
}

func siteCommandFailure(operation string, err error) string {
	code, message := siteCommandError(err)
	if code == codes.DeadlineExceeded {
		return operation + " timed out. " + message
	}
	return fmt.Sprintf("%s failed (%s): %s", operation, code, message)
}

// Keep the latest failure for each command locally. A failed recovery STOP
// must not overwrite the MOVE failure that triggered it. No sensor data or
// robot identity is stored, and successful commands do not write to disk.
func recordSiteCommandFault(fault siteCommandFault) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "Wendy")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	raw, err := json.Marshal(fault)
	if err != nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("go2-command-fault-%d.json", fault.Command))
	// Unique temporary files avoid two active sessions sharing a writer.
	file, err := os.CreateTemp(dir, ".go2-command-fault-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr == nil && closeErr == nil {
		_ = os.Rename(file.Name(), path)
	}
}

func (r *SiteRobot) flushCommandFaults() {
	for _, fault := range r.pendingCommandFaults {
		r.commandFaultSink(fault)
	}
	r.pendingCommandFaults = nil
}
