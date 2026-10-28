package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type runLogRecord struct {
	Target    commandTarget `json:"target"`
	Output    string        `json:"output"`
	Truncated bool          `json:"truncated"`
}

func (s *mcpServer) saveRunLog(target commandTarget, output string, truncated bool) string {
	uri := "wendy://runs/" + uuid.NewString() + "/logs"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runLogs == nil {
		s.runLogs = make(map[string]runLogRecord)
	}
	s.runLogs[uri] = runLogRecord{Target: target, Output: output, Truncated: truncated}
	s.runLogOrder = append(s.runLogOrder, uri)
	for len(s.runLogOrder) > 8 {
		delete(s.runLogs, s.runLogOrder[0])
		s.runLogOrder = s.runLogOrder[1:]
	}
	return uri
}

func (s *mcpServer) registerRunLogResource(srv *server.MCPServer) {
	srv.AddResourceTemplate(mcpgo.NewResourceTemplate("wendy://runs/{run_id}/logs", "Deployment build logs",
		mcpgo.WithTemplateDescription("Bounded build-log tails for the last eight run calls in this server session. Read a returned build_log_uri only when build details are needed."),
		mcpgo.WithTemplateMIMEType("application/json")), s.handleRunLogResource)
}

func (s *mcpServer) handleRunLogResource(_ context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	if policy := s.bindingPolicy(); policy != nil && !policy.allows(mcpgo.NewTool("run")) {
		return nil, fmt.Errorf("deployment logs require access to run")
	}
	uri := req.Params.URI
	id := strings.TrimSuffix(strings.TrimPrefix(uri, "wendy://runs/"), "/logs")
	if _, err := uuid.Parse(id); err != nil {
		return nil, fmt.Errorf("invalid deployment log URI")
	}
	s.mu.RLock()
	record, found := s.runLogs[uri]
	s.mu.RUnlock()
	if !found {
		return nil, fmt.Errorf("deployment log not found or expired from this server session")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: uri, MIMEType: "application/json", Text: string(data)}}, nil
}
