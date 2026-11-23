package containerd

import runtimespec "github.com/opencontainers/runtime-spec/specs-go"

// appExecProcess also resets legacy specs that predate explicit app OOM scores.
// It is the single copy point: the returned process owns its Args and Env, so
// callers may modify or append to them without touching the stored config.
func appExecProcess(process *runtimespec.Process) *runtimespec.Process {
	p := *process
	score := 0
	p.OOMScoreAdj = &score
	p.Args = append([]string(nil), process.Args...)
	p.Env = append([]string(nil), process.Env...)
	return &p
}
