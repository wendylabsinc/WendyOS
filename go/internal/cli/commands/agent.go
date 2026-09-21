package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/a2a"
	"github.com/wendylabsinc/wendy/go/internal/cli/agentservice"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Run persistent agents and communicate over A2A", GroupID: "manage"}
	cmd.AddCommand(newAgentServeCmd(), newAgentExampleCmd())
	for _, action := range []string{"send", "get", "cancel", "tasks", "event"} {
		cmd.AddCommand(newAgentClientCmd(action))
	}
	return cmd
}
func newAgentExampleCmd() *cobra.Command {
	return &cobra.Command{Use: "example", Short: "Print an example persistent agent configuration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c := agentservice.Config{Name: "device-brain", Profile: "device-reasoning", Workspace: ".", Model: chat.ModelSpec{Provider: "local", Model: "replace-with-your-model", BaseURL: "http://localhost:8080/v1"}, AllowTools: []string{}, TaskTimeoutSeconds: 600, MaxTasks: 200, Triggers: []agentservice.Trigger{{Type: "person.detected", Source: "front-camera", MinConfidence: 0.85, Consecutive: 3, MaxGapSeconds: 2, CooldownSeconds: 30, MaxAgeSeconds: 10, Prompt: "Inspect this observation and report whether assistance is needed."}}}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	}}
}
func newAgentServeCmd() *cobra.Command {
	var file, stateDir, listen, publicURL, tokenEnv, cert, key string
	cmd := &cobra.Command{Use: "serve", Short: "Host a durable agent queue, event ingress, and A2A endpoint", Long: "Host an agent independently of chat. Run under a service manager or a Wendy application for automatic startup. State and memory persist in --state-dir. Queued tasks resume after restart; interrupted tasks fail for inspection instead of repeating effects.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := agentservice.LoadConfig(file)
		if err != nil {
			return err
		}
		if stateDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			stateDir = filepath.Join(home, ".wendy", "agents", c.Name)
		}
		stateDir, err = filepath.Abs(stateDir)
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		opts, err := c.SessionOptions(executable, filepath.Join(stateDir, "memory"))
		if err != nil {
			return err
		}
		host, port, err := net.SplitHostPort(listen)
		if err != nil {
			return fmt.Errorf("listen address: %w", err)
		}
		// Pin localhost to a literal loopback address before binding or advertising it.
		if strings.EqualFold(host, "localhost") {
			host = "127.0.0.1"
			listen = net.JoinHostPort(host, port)
		}
		ip := net.ParseIP(host)
		loopback := ip != nil && ip.IsLoopback()
		tls := cert != "" && key != ""
		if (cert == "") != (key == "") {
			return errors.New("provide both --tls-cert and --tls-key")
		}
		if !loopback && !tls {
			return errors.New("non-loopback listeners require TLS; use --tls-cert and --tls-key, or a loopback listener behind a tunnel")
		}
		defaultPublicURL := publicURL == ""
		if defaultPublicURL {
			if ip == nil || ip.IsUnspecified() {
				return errors.New("set --public-url to the reachable agent URL")
			}
			scheme := "http"
			if tls {
				scheme = "https"
			}
			publicURL = scheme + "://" + listen
		}
		if err := a2a.ValidateURL(publicURL); err != nil {
			return err
		}
		token := os.Getenv(tokenEnv)
		if len(token) < 16 {
			return fmt.Errorf("set %s to an agent access token of at least 16 characters", tokenEnv)
		}
		runner := func(ctx context.Context, prompt string) (string, error) {
			taskOpts := opts
			taskOpts.DelegationDepth = a2a.DelegationDepth(ctx)
			session, err := chat.NewSession(ctx, taskOpts)
			if err != nil {
				return "", err
			}
			defer session.Close()
			approve := func(ctx context.Context, call chat.ToolCall) (bool, error) {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				for _, name := range c.AllowTools {
					if name == call.Name {
						return true, nil
					}
				}
				return false, nil
			}
			err = session.Engine.Turn(ctx, prompt, nil, approve)
			final := ""
			for _, m := range session.Engine.Messages() {
				if m.Role == "assistant" && len(m.ToolCalls) == 0 {
					final = m.Content
				}
			}
			return final, err
		}
		service, err := agentservice.Open(c, stateDir, runner)
		if err != nil {
			return err
		}
		defer service.Close()
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			return err
		}
		defer listener.Close()
		if defaultPublicURL {
			scheme := "http"
			if tls {
				scheme = "https"
			}
			publicURL = scheme + "://" + listener.Addr().String()
		}
		handler, err := service.Handler(publicURL, token)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
		workers := make(chan error, 1)
		serving := make(chan error, 1)
		go func() { workers <- service.Run(ctx) }()
		go func() {
			if tls {
				serving <- server.ServeTLS(listener, cert, key)
			} else {
				serving <- server.Serve(listener)
			}
		}()
		fmt.Fprintf(cmd.ErrOrStderr(), "Agent %s listening at %s; state: %s\n", c.Name, publicURL, stateDir)
		var result error
		workerStopped := false
		select {
		case <-ctx.Done():
		case result = <-workers:
			workerStopped = true
		case result = <-serving:
		}
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
		if !workerStopped {
			workerErr := <-workers
			if result == nil && !errors.Is(workerErr, context.Canceled) {
				result = workerErr
			}
		}
		if errors.Is(result, http.ErrServerClosed) || errors.Is(result, context.Canceled) {
			return nil
		}
		return result
	}}
	cmd.Flags().StringVar(&file, "config", "", "Agent service JSON configuration")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Persistent queue and memory directory")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8787", "HTTP listen address; non-loopback requires TLS")
	cmd.Flags().StringVar(&publicURL, "public-url", "", "Reachable base URL advertised in the A2A card")
	cmd.Flags().StringVar(&tokenEnv, "token-env", "WENDY_AGENT_TOKEN", "Environment variable containing the service access token")
	cmd.Flags().StringVar(&cert, "tls-cert", "", "TLS certificate PEM for direct remote connections")
	cmd.Flags().StringVar(&key, "tls-key", "", "TLS private key PEM for direct remote connections")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
func newAgentClientCmd(action string) *cobra.Command {
	var endpoint, tokenEnv, prompt, id, file, requestID string
	var wait bool
	descriptions := map[string]string{"send": "Submit a durable task to an A2A agent", "get": "Read an A2A task", "cancel": "Cancel an A2A task", "tasks": "List retained A2A tasks", "event": "Post a sensor event to a Wendy agent"}
	cmd := &cobra.Command{Use: action, Short: descriptions[action], Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		token := os.Getenv(tokenEnv)
		if token == "" {
			return fmt.Errorf("set %s to the remote agent access token", tokenEnv)
		}
		client, err := a2a.NewClient(endpoint, token)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		var result any
		switch action {
		case "send":
			if strings.TrimSpace(prompt) == "" {
				return errors.New("--prompt must not be empty")
			}
			if requestID == "" {
				requestID = uuid.NewString()
			}
			req := a2a.SendRequest{Message: a2a.Message{MessageID: requestID, Role: "ROLE_USER", Parts: []a2a.Part{{Text: prompt}}}}
			req.Configuration.ReturnImmediately = true
			response, e := client.Send(ctx, req)
			err = e
			result = response
			if err == nil && wait && response.Task != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Task %s submitted. Interrupting this client leaves the remote task running.\n", response.Task.ID)
				task := *response.Task
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for !a2a.Terminal(task.Status.State) {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-ticker.C:
					}
					task, err = client.Get(ctx, task.ID)
					if err != nil {
						return err
					}
				}
				result = task
			}
		case "get":
			result, err = client.Get(ctx, id)
		case "cancel":
			result, err = client.Cancel(ctx, id)
		case "tasks":
			result, err = listAllAgentTasks(ctx, client)
		case "event":
			var input io.Reader = cmd.InOrStdin()
			if file != "-" {
				f, e := os.Open(file)
				if e != nil {
					return e
				}
				defer f.Close()
				input = f
			}
			var event agentservice.SensorEvent
			dec := json.NewDecoder(io.LimitReader(input, 65537))
			dec.DisallowUnknownFields()
			if e := dec.Decode(&event); e != nil {
				return e
			}
			if e := dec.Decode(new(any)); e != io.EOF {
				return errors.New("expected one event JSON object")
			}
			var response agentservice.EventResult
			err = client.Do(ctx, "POST", "/events", event, &response)
			result = response
		}
		if err != nil {
			return err
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
			return err
		}
		if task, ok := result.(a2a.Task); ok && action == "send" && wait && task.Status.State != a2a.Completed {
			return fmt.Errorf("remote task %s ended in %s", task.ID, task.Status.State)
		}
		return nil
	}}
	cmd.Flags().StringVar(&endpoint, "url", "http://127.0.0.1:8787", "A2A HTTP+JSON agent base URL")
	cmd.Flags().StringVar(&tokenEnv, "token-env", "WENDY_AGENT_TOKEN", "Environment variable containing the agent access token")
	if action == "send" {
		cmd.Flags().StringVar(&prompt, "prompt", "", "Task to submit")
		cmd.Flags().StringVar(&requestID, "request-id", "", "Stable message ID for retrying the same submission")
		cmd.Flags().BoolVar(&wait, "wait", false, "Wait for completion; client interruption does not cancel the remote task")
		_ = cmd.MarkFlagRequired("prompt")
	}
	if action == "get" || action == "cancel" {
		cmd.Flags().StringVar(&id, "task", "", "Task ID")
		_ = cmd.MarkFlagRequired("task")
	}
	if action == "event" {
		cmd.Flags().StringVar(&file, "file", "-", "Event JSON file, or - for standard input")
	}
	return cmd
}

func listAllAgentTasks(ctx context.Context, client *a2a.Client) (a2a.TaskList, error) {
	result := a2a.TaskList{Tasks: []a2a.Task{}}
	token := ""
	seen := map[string]bool{}
	for pages := 0; pages < 1000; pages++ {
		var page a2a.TaskList
		if err := client.Do(ctx, "GET", "/tasks?pageToken="+url.QueryEscape(token), nil, &page); err != nil {
			return a2a.TaskList{}, err
		}
		result.Tasks = append(result.Tasks, page.Tasks...)
		result.TotalSize = page.TotalSize
		result.PageSize = len(result.Tasks)
		token = page.NextPageToken
		if token == "" {
			return result, nil
		}
		if seen[token] {
			return a2a.TaskList{}, errors.New("agent repeated a task continuation token")
		}
		seen[token] = true
	}
	return a2a.TaskList{}, errors.New("agent task listing exceeded 1000 pages")
}
