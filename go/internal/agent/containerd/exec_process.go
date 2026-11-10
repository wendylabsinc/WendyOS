package containerd

import runtimespec "github.com/opencontainers/runtime-spec/specs-go"

// appExecProcess also resets legacy specs that predate explicit app OOM scores.
// Copy the process before adjusting it so exec cannot change the stored config.
func appExecProcess(process *runtimespec.Process) *runtimespec.Process {
	copy := *process
	score := 0
	copy.OOMScoreAdj = &score
	copy.Args = append([]string(nil), process.Args...)
	copy.Env = append([]string(nil), process.Env...)
	return &copy
}
