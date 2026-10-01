//go:build js && wasm

// wendy-web exposes the existing Wendy gRPC client to a browser Web Worker.
// Credentials and device TLS stay in the worker; browser storage persists sign-in.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"github.com/coder/websocket"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/encoding/protojson"
)

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type bridge struct {
	auth            *browserauth.Session
	mu              sync.Mutex
	conn            *grpcclient.AgentConnection
	life            context.Context
	close           context.CancelFunc
	logsCancel      context.CancelFunc
	telemetryCancel context.CancelFunc
	shellCancel     context.CancelFunc
	shell           agentpb.WendyShellService_HostShellClient
}

func send(v any) {
	data, _ := json.Marshal(v)
	js.Global().Call("postMessage", js.Global().Get("JSON").Call("parse", string(data)))
}
func event(kind string, data any) { send(map[string]any{"event": kind, "data": data}) }

func main() {
	_ = os.Setenv("WENDY_TLS_SESSION_STORE", "off")
	b := &bridge{auth: &browserauth.Session{Client: &http.Client{Transport: authProxyTransport{}}, Store: browserCredentialStore{}}}
	callback := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 1 {
			return nil
		}
		var r request
		if json.Unmarshal([]byte(args[0].String()), &r) != nil {
			return nil
		}
		go func() {
			value, err := b.handle(r)
			if err != nil {
				send(map[string]any{"id": r.ID, "error": err.Error()})
			} else {
				send(map[string]any{"id": r.ID, "result": value})
			}
		}()
		return nil
	})
	js.Global().Set("wendyRequest", callback)
	send(map[string]any{"ready": true})
	select {}
}

func (b *bridge) handle(r request) (any, error) {
	if strings.HasPrefix(r.Method, "shell-") {
		return nil, errors.New("Shell access is disabled in the browser client")
	}
	if r.Method == "cloud-connect" {
		if js.Global().Get("location").Get("origin").String() != "http://localhost:5173" {
			return nil, errors.New("Cloud sessions require the local browser relay")
		}
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.close != nil {
			b.close()
		}
		if b.conn != nil {
			b.conn.Close()
			b.conn = nil
		}
		life, stop := context.WithCancel(context.Background())
		ctx, cancel := context.WithTimeout(life, 35*time.Second)
		defer cancel()
		conn, err := b.auth.ConnectDevice(ctx, p.ID, browserCloudDial, &http.Client{Transport: authProxyTransport{}, Timeout: 15 * time.Second})
		if err != nil {
			stop()
			return nil, err
		}
		_, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			stop()
			conn.Close()
			return nil, fmt.Errorf("Connecting to cloud device: %w", err)
		}
		b.conn, b.life, b.close = conn, life, stop
		return true, nil
	}
	if r.Method == "cloud-discover" {
		// The local relay has a fixed TLS-verified upstream. Never send the session
		// token to a caller-supplied relay URL.
		if js.Global().Get("location").Get("origin").String() != "http://localhost:5173" {
			return nil, errors.New("Cloud discovery currently requires the local browser relay")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		assets, err := b.auth.Discover(ctx, func(ctx context.Context, _ string) (net.Conn, error) {
			ws, _, err := websocket.Dial(ctx, "ws://127.0.0.1:8788/cloud", nil)
			if err != nil {
				return nil, fmt.Errorf("Browser relay connection failed; check the npm run dev terminal: %w", err)
			}
			return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
		})
		profile := b.auth.Profile()
		if profile.Subject != "" {
			event("auth-profile", profile)
		}
		if err != nil {
			return nil, err
		}
		result := []json.RawMessage{}
		for _, asset := range assets {
			result = append(result, json.RawMessage(protojson.Format(asset)))
		}
		return result, nil
	}
	if strings.HasPrefix(r.Method, "auth-") {
		var p struct {
			Email    string `json:"email"`
			Redirect string `json:"redirect"`
			Code     string `json:"code"`
			State    string `json:"state"`
			Issuer   string `json:"issuer"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		switch r.Method {
		case "auth-restore":
			return b.auth.Restore(ctx)
		case "auth-signout":
			return true, b.auth.SignOut()
		case "auth-begin":
			return b.auth.Begin(ctx, p.Email, p.Redirect)
		case "auth-complete":
			return b.auth.Complete(ctx, p.Code, p.State, p.Issuer)
		}
		return nil, errors.New("Unknown authentication action")
	}

	if r.Method == "connect" {
		return nil, errors.New("Direct certificate connections are disabled; sign in with Wendy")
	}
	if r.Method == "disconnect" {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.close != nil {
			b.close()
		}
		if b.conn != nil {
			_ = b.conn.Close()
			b.conn = nil
		}
		return true, nil
	}
	b.mu.Lock()
	conn, life := b.conn, b.life
	b.mu.Unlock()
	if conn == nil {
		return nil, errors.New("Connect to a device first")
	}
	ctx, cancel := context.WithTimeout(life, 30*time.Second)
	defer cancel()
	var p struct {
		App   string   `json:"app"`
		Image string   `json:"image"`
		Env   []string `json:"env"`
		Data  string   `json:"data"`
		Rows  uint32   `json:"rows"`
		Cols  uint32   `json:"cols"`
	}
	if len(r.Params) > 0 {
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
	}
	switch r.Method {
	case "telemetry-stop":
		b.mu.Lock()
		if b.telemetryCancel != nil {
			b.telemetryCancel()
		}
		if b.logsCancel != nil {
			b.logsCancel()
		}
		b.mu.Unlock()
		return true, nil
	case "metrics", "traces":
		b.mu.Lock()
		if b.telemetryCancel != nil {
			b.telemetryCancel()
		}
		streamCtx, stop := context.WithCancel(life)
		b.telemetryCancel = stop
		b.mu.Unlock()
		last := int32(20)
		if r.Method == "metrics" {
			req := &agentpb.StreamMetricsRequest{LastN: &last}
			if p.App != "" {
				req.AppName = &p.App
			}
			stream, err := conn.TelemetryService.StreamMetrics(streamCtx, req)
			if err != nil {
				stop()
				return nil, err
			}
			go func() {
				defer stop()
				for {
					msg, err := stream.Recv()
					if err != nil {
						if streamCtx.Err() == nil {
							event("metrics-error", err.Error())
						}
						return
					}
					event("metrics", json.RawMessage(protojson.Format(msg)))
				}
			}()
		} else {
			req := &agentpb.StreamTracesRequest{LastN: &last}
			if p.App != "" {
				req.AppName = &p.App
			}
			stream, err := conn.TelemetryService.StreamTraces(streamCtx, req)
			if err != nil {
				stop()
				return nil, err
			}
			go func() {
				defer stop()
				for {
					msg, err := stream.Recv()
					if err != nil {
						if streamCtx.Err() == nil {
							event("traces-error", err.Error())
						}
						return
					}
					event("traces", json.RawMessage(protojson.Format(msg)))
				}
			}()
		}
		return true, nil
	case "snapshot":
		v, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			return nil, err
		}
		result := map[string]any{"version": json.RawMessage(protojson.Format(v)), "apps": []json.RawMessage{}}
		warnings := []string{}
		apps := []json.RawMessage{}
		stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err == nil {
			for {
				item, e := stream.Recv()
				if e == io.EOF {
					break
				}
				if e != nil {
					err = e
					break
				}
				if item.Container != nil {
					apps = append(apps, json.RawMessage(protojson.Format(item.Container)))
				}
			}
		}
		if err != nil {
			warnings = append(warnings, "Applications: "+err.Error())
		}
		result["apps"] = apps
		stats, err := conn.ContainerService.GetResourceStats(ctx, &agentpb.GetResourceStatsRequest{})
		if err == nil {
			result["stats"] = json.RawMessage(protojson.Format(stats))
		} else {
			warnings = append(warnings, "Resource metrics: "+err.Error())
		}
		result["warnings"] = warnings
		return result, nil
	case "start", "restart":
		if p.App == "" {
			return nil, errors.New("App name is required")
		}
		if r.Method == "restart" {
			if _, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: p.App}); err != nil {
				return nil, err
			}
		}
		stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{AppName: p.App})
		if err != nil {
			return nil, err
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return nil, err
			}
			if msg.GetStarted() != nil {
				return true, nil
			}
		}
	case "stop":
		if p.App == "" {
			return nil, errors.New("App name is required")
		}
		_, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: p.App})
		return true, err
	case "create":
		if p.App == "" || p.Image == "" {
			return nil, errors.New("App name and image are required")
		}
		_, err := conn.ContainerService.CreateContainer(ctx, &agentpb.CreateContainerRequest{AppName: p.App, ImageName: p.Image, Env: p.Env})
		return true, err
	case "logs":
		b.mu.Lock()
		if b.logsCancel != nil {
			b.logsCancel()
		}
		streamCtx, stop := context.WithCancel(life)
		b.logsCancel = stop
		b.mu.Unlock()
		last := int32(100)
		req := &agentpb.StreamLogsRequest{LastN: &last}
		if p.App != "" {
			req.AppName = &p.App
		}
		stream, err := conn.TelemetryService.StreamLogs(streamCtx, req)
		if err != nil {
			stop()
			return nil, err
		}
		go func() {
			defer stop()
			for {
				msg, err := stream.Recv()
				if err != nil {
					if streamCtx.Err() == nil {
						event("logs-error", err.Error())
					}
					return
				}
				event("logs", json.RawMessage(protojson.Format(msg)))
			}
		}()
		return true, nil
	case "logs-stop":
		b.mu.Lock()
		if b.logsCancel != nil {
			b.logsCancel()
		}
		b.mu.Unlock()
		return true, nil
	case "shell-open":
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.shellCancel != nil {
			b.shellCancel()
		}
		streamCtx, stop := context.WithCancel(life)
		b.shellCancel = stop
		s, err := conn.ShellService.HostShell(streamCtx)
		if err != nil {
			stop()
			return nil, err
		}
		if p.Cols == 0 {
			p.Cols = 100
		}
		if p.Rows == 0 {
			p.Rows = 30
		}
		err = s.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Start_{Start: &agentpb.HostShellRequest_Start{TermSize: &agentpb.WindowSize{Rows: p.Rows, Cols: p.Cols}}}})
		if err != nil {
			stop()
			return nil, err
		}
		b.shell = s
		go func() {
			defer stop()
			for {
				msg, err := s.Recv()
				if err != nil {
					if streamCtx.Err() == nil {
						event("shell-exit", err.Error())
					}
					return
				}
				switch v := msg.ResponseType.(type) {
				case *agentpb.HostShellResponse_StdoutData:
					event("shell-data", v.StdoutData)
				case *agentpb.HostShellResponse_ExitCode:
					event("shell-exit", fmt.Sprintf("Shell exited with code %d", v.ExitCode))
				}
			}
		}()
		return true, nil
	case "shell-input", "shell-resize", "shell-close":
		b.mu.Lock()
		defer b.mu.Unlock()
		if r.Method == "shell-close" {
			if b.shellCancel != nil {
				b.shellCancel()
			}
			b.shell = nil
			return true, nil
		}
		if b.shell == nil {
			return nil, errors.New("Open a terminal session first")
		}
		if r.Method == "shell-input" {
			return true, b.shell.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_StdinData{StdinData: []byte(p.Data)}})
		}
		return true, b.shell.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Resize{Resize: &agentpb.WindowSize{Rows: p.Rows, Cols: p.Cols}}})
	default:
		return nil, fmt.Errorf("Unknown operation %q", r.Method)
	}
}

// Only HTTP transport crosses the same-origin proxy. DPoP signs the original
// upstream URL; operator keys never leave this worker.
type authProxyTransport struct{}

func (authProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target := req.URL.String()
	origin := js.Global().Get("location").Get("origin").String()
	proxy, err := url.Parse(origin + "/api/wendy-auth?url=" + url.QueryEscape(target))
	if err != nil {
		return nil, err
	}
	clone.URL = proxy
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

// IndexedDB operations execute inside this worker, never in the UI thread.
type browserCredentialStore struct{}

func storageCall(method string, args ...any) (string, error) {
	store := js.Global().Get("wendyCredentials")
	if store.IsUndefined() {
		return "", errors.New("Browser credential storage is unavailable")
	}
	type result struct {
		value string
		err   error
	}
	done := make(chan result, 1)
	success := js.FuncOf(func(_ js.Value, args []js.Value) any {
		value := ""
		if len(args) > 0 && args[0].Type() == js.TypeString {
			value = args[0].String()
		}
		done <- result{value: value}
		return nil
	})
	failure := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- result{err: errors.New("Browser could not access saved credentials")}
		return nil
	})
	store.Call(method, args...).Call("then", success, failure)
	r := <-done
	success.Release()
	failure.Release()
	return r.value, r.err
}
func (browserCredentialStore) Load() ([]byte, error) {
	value, err := storageCall("load")
	return []byte(value), err
}
func (browserCredentialStore) Save(value []byte) error {
	_, err := storageCall("save", string(value))
	return err
}
func (browserCredentialStore) Clear() error { _, err := storageCall("clear"); return err }

// Only Wendy public HTTPS endpoints are eligible for the broker relay.
func browserCloudDial(ctx context.Context, endpoint string) (net.Conn, error) {
	path := "/cloud"
	if endpoint != "api.dev.wendy.sh:443" {
		target, err := cloudrelay.BrowserBrokerTarget(endpoint)
		if err != nil {
			return nil, err
		}
		path = "/broker?endpoint=" + url.QueryEscape(target)
	}
	ws, _, err := websocket.Dial(ctx, "ws://127.0.0.1:8788"+path, nil)
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
}
