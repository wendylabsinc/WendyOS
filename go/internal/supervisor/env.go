package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// The supervisor tells the agent it is supervised through these variables.
const (
	EnvSupervisor = "WENDY_AGENT_SUPERVISOR"
	EnvStore      = "WENDY_AGENT_STORE"
)

// InboxFromEnv is the upload inbox when the supervisor started this agent.
func InboxFromEnv() (string, bool) {
	sup, store := os.Getenv(EnvSupervisor), os.Getenv(EnvStore)
	if sup == "" || store == "" {
		return "", false
	}
	return Store{Dir: store}.InboxDir(), true
}

// WriteIncoming marks an upload in the inbox as complete; write the binary first.
func WriteIncoming(inbox string, in Incoming) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(inbox, in.SHA256+".json"), data, 0o644)
}

// cloudEnrolledFile holds the pid of an agent that connects to the cloud relay.
const cloudEnrolledFile = "cloud-enrolled"

// MarkCloudEnrolled tells the supervisor this agent connects to the cloud, so
// an automatic update waits until it is connected rather than skip that check.
func MarkCloudEnrolled(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cloudEnrolledFile), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// CloudEnrolledFunc reports whether the agent with pid marked itself enrolled.
func CloudEnrolledFunc(dir string) func(pid int) bool {
	return func(pid int) bool {
		data, err := os.ReadFile(filepath.Join(dir, cloudEnrolledFile))
		return err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(pid)
	}
}
