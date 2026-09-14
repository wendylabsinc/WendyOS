package mcp

import "time"

// maxProxyDiag caps how many proxy failures are retained. The reconciler now
// retries a failing app for as long as the session lasts, so without a bound a
// connected-but-unreachable device would grow this slice by thousands of
// entries over a multi-day session and wendy_status would serialise all of it.
// Oldest entries are dropped first.
const maxProxyDiag = 200

// proxyDiagEntry records a single container-MCP proxy failure so it can be
// surfaced to callers instead of vanishing to stderr.
type proxyDiagEntry struct {
	AppName string `json:"app_name"`
	Stage   string `json:"stage"`
	Error   string `json:"error"`
	Time    string `json:"time"`
	// Count is how many times this exact failure has been seen; Time is the
	// most recent. Repeats collapse rather than accumulate -- one app failing
	// every pass is one fact, not one fact per tick.
	Count int `json:"count"`
}

// recordProxyDiag records a container-MCP proxy failure and reports whether it
// is the first sighting of that exact failure. No-op if err is nil. A repeat of
// the same app, stage and error updates the existing entry instead of appending
// a new one, so a caller can log once rather than once per pass.
func (s *mcpServer) recordProxyDiag(appName, stage string, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	now := time.Now().UTC().Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.proxyDiag {
		if e := &s.proxyDiag[i]; e.AppName == appName && e.Stage == stage && e.Error == msg {
			e.Count++
			e.Time = now
			return false
		}
	}
	s.proxyDiag = append(s.proxyDiag, proxyDiagEntry{
		AppName: appName,
		Stage:   stage,
		Error:   msg,
		Time:    now,
		Count:   1,
	})
	if len(s.proxyDiag) > maxProxyDiag {
		s.proxyDiag = append(s.proxyDiag[:0], s.proxyDiag[len(s.proxyDiag)-maxProxyDiag:]...)
	}
	return true
}

// proxyDiagnostics returns a copy of all recorded container-MCP proxy
// diagnostics.
func (s *mcpServer) proxyDiagnostics() []proxyDiagEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]proxyDiagEntry, len(s.proxyDiag))
	copy(out, s.proxyDiag)
	return out
}
