//go:build js && wasm

// wendy-web exposes the existing Wendy gRPC client to a browser Web Worker.
// Credentials and device TLS stay in the worker; browser storage persists sign-in.
package main

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
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
	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/encoding/protojson"
)

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	// bytes is the request's second argument: data to sign, as a Uint8Array.
	bytes []byte
}

// signature is returned to the page as a Uint8Array, never as a string.
type signature []byte

type bridge struct {
	auth            *browserauth.Session
	initialized     bool
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
		if len(args) < 1 || len(args) > 2 {
			return nil
		}
		var r request
		if json.Unmarshal([]byte(args[0].String()), &r) != nil {
			return nil
		}
		if len(args) == 2 && args[1].InstanceOf(js.Global().Get("Uint8Array")) {
			r.bytes = make([]byte, args[1].Length())
			js.CopyBytesToGo(r.bytes, args[1])
		}
		go func() {
			value, err := b.handle(r)
			if sig, ok := value.(signature); ok && err == nil {
				buf := js.Global().Get("Uint8Array").New(len(sig))
				js.CopyBytesToJS(buf, sig)
				js.Global().Call("postMessage", js.ValueOf(map[string]any{"id": r.ID, "result": map[string]any{"signature": buf}}))
			} else if err != nil {
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
	if r.Method == "init" {
		// A deployment's page pins its services. They are called directly, so
		// the dev dashboard's same-origin auth proxy is not involved.
		// SECURITY: the page is this worker's origin and trust domain: it picks the worker script, and saved sign-ins live in its IndexedDB, so a hostile page needs no init to misuse them. Validate holds the services to HTTPS and fixed paths; a host allowlist would break self-hosted deployments. Records from another auth host do not restore (issuer check), and init runs once.
		var settings browserauth.Settings
		if err := json.Unmarshal(r.Params, &settings); err != nil {
			return nil, err
		}
		if err := settings.Validate(); err != nil {
			return nil, err
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.initialized {
			return nil, errors.New("The Wendy client is already initialized")
		}
		b.initialized = true
		b.auth = &browserauth.Session{Settings: &settings, Client: &http.Client{}, Store: browserCredentialStore{}}
		return true, nil
	}
	// The console builds postbox-relayed payloads itself; this gives it the
	// CLI's validity rule (default 60 days, at most 365) for their valid_until.
	if r.Method == "valid-until" {
		var p struct {
			ValidForSeconds int64 `json:"validForSeconds"`
		}
		if len(r.Params) > 0 {
			if err := json.Unmarshal(r.Params, &p); err != nil {
				return nil, err
			}
		}
		if p.ValidForSeconds > int64(cloudrequest.MaxValidity/time.Second) {
			return nil, fmt.Errorf("validForSeconds %d is more than 365 days", p.ValidForSeconds)
		}
		until, err := cloudrequest.ValidUntil(time.Now(), time.Duration(p.ValidForSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		return map[string]any{"seconds": until.Unix()}, nil
	}
	// SECURITY: signing raw bytes is the approved console design (sem, 2026-10-19 22:55): the console builds its JWS, DPoP and management-request inputs, which verifiers check byte for byte, so no prefix can be added. Only same-origin script reaches this worker; it can use the key, never export it.
	if r.Method == "credential" || r.Method == "sign" {
		var p org
		if len(r.Params) > 0 {
			if err := json.Unmarshal(r.Params, &p); err != nil {
				return nil, err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if r.Method == "sign" {
			if len(r.bytes) == 0 {
				return nil, errors.New("Nothing to sign")
			}
			_, key, err := b.operator(ctx, p, false)
			if err != nil {
				return nil, err
			}
			sig, err := key.Sign(rand.Reader, r.bytes, &mldsa.Options{})
			return signature(sig), err
		}
		saved, key, err := b.operator(ctx, p, true)
		if err != nil {
			return nil, err
		}
		jwk, alg, err := cloudrequest.OperatorPublicJWK(key)
		if err != nil {
			return nil, err
		}
		chain := [][]byte{}
		for _, raw := range []string{saved.Certificate.PemCertificate, saved.Certificate.PemCertificateChain} {
			for rest := []byte(raw); ; {
				var block *pem.Block
				if block, rest = pem.Decode(rest); block == nil {
					break
				}
				chain = append(chain, block.Bytes)
			}
		}
		return map[string]any{"profile": saved.Profile, "algorithm": alg, "chainDER": chain, "accessToken": saved.Tokens.Access, "publicJWK": jwk}, nil
	}
	if strings.HasPrefix(r.Method, "shell-") {
		return nil, errors.New("Shell access is disabled in the browser client")
	}
	if r.Method == "mesh-topology" {
		if js.Global().Get("location").Get("origin").String() != "http://localhost:5173" {
			return nil, errors.New("Local observations require the local browser relay")
		}
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return b.auth.MeshTopology(context.Background(), p.ID, browserCloudDial, &http.Client{Transport: authProxyTransport{}, Timeout: 12 * time.Second}), nil
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
	if r.Method == "cloud-discover" || r.Method == "mesh-discover" {
		// The local relay has a fixed TLS-verified upstream. Never send the session
		// token to a caller-supplied relay URL.
		if js.Global().Get("location").Get("origin").String() != "http://localhost:5173" {
			return nil, errors.New("Cloud discovery currently requires the local browser relay")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		assets, err := b.auth.DiscoverFiltered(ctx, func(ctx context.Context, _ string) (net.Conn, error) {
			ws, _, err := websocket.Dial(ctx, "ws://127.0.0.1:8788/cloud", nil)
			if err != nil {
				return nil, fmt.Errorf("Browser relay connection failed; check the npm run dev terminal: %w", err)
			}
			return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
		}, r.Method == "cloud-discover")
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
			Refresh  bool   `json:"refresh"`
			org
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		switch r.Method {
		case "auth-restore":
			if p.Tenant != "" {
				if err := selectOrg(p.org); err != nil {
					return nil, err
				}
			}
			if p.Refresh {
				return b.auth.RestoreRotated(ctx)
			}
			return b.auth.Restore(ctx)
		case "auth-signout":
			// With no tenant, every organization signs out.
			if p.Tenant == "" || p.Tenant == b.auth.Profile().Tenant {
				if err := b.auth.SignOut(); err != nil {
					return nil, err
				}
			}
			if _, err := storageCall("forget", p.Tenant); err != nil {
				return nil, fmt.Errorf("Could not remove saved sign-in: %w", err)
			}
			return true, nil
		case "auth-begin":
			return b.auth.Begin(ctx, p.Email, p.Issuer, p.Redirect)
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
// The operator key crosses into JS only as a Uint8Array for WebCrypto to wrap
// or unwrap, and is zeroed on both sides once copied; never as a string.
type browserCredentialStore struct{}

func storageCall(method string, args ...any) (js.Value, error) {
	store := js.Global().Get("wendyCredentials")
	if store.IsUndefined() {
		return js.Undefined(), errors.New("Browser credential storage is unavailable")
	}
	type result struct {
		value js.Value
		err   error
	}
	done := make(chan result, 1)
	success := js.FuncOf(func(_ js.Value, args []js.Value) any {
		value := js.Undefined()
		if len(args) > 0 {
			value = args[0]
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
	if err != nil || value.Type() != js.TypeString {
		return nil, err
	}
	return []byte(value.String()), nil
}

// Save would put the key into JS as part of a string; Session uses SaveWrapped.
func (browserCredentialStore) Save([]byte) error {
	return errors.New("The operator key must be saved wrapped")
}
func (browserCredentialStore) SaveWrapped(session, seed []byte, aad string) error {
	buf := js.Global().Get("Uint8Array").New(len(seed))
	js.CopyBytesToJS(buf, seed)
	defer buf.Call("fill", 0)
	_, err := storageCall("saveWrapped", string(session), buf, aad)
	return err
}
func (browserCredentialStore) LoadKey(aad string) ([]byte, error) {
	value, err := storageCall("loadKey", aad)
	if err != nil {
		return nil, err
	}
	if !value.InstanceOf(js.Global().Get("Uint8Array")) {
		return nil, errors.New("Browser returned no operator key")
	}
	defer value.Call("fill", 0)
	seed := make([]byte, value.Length())
	js.CopyBytesToGo(seed, value)
	return seed, nil
}
func (browserCredentialStore) Clear() error { _, err := storageCall("clear"); return err }

// org names one organization's saved sign-in. Tenant alone selects the most
// recent operator signed in to it.
type org struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
}

func selectOrg(o org) error {
	if o.Tenant == "" || strings.Contains(o.Tenant, ":") || strings.Contains(o.Subject, ":") {
		return errors.New("Invalid organization")
	}
	_, err := storageCall("select", o.Tenant, o.Subject)
	return err
}

// savedRecord reads the fields of browserauth's saved session that the page's
// own signer needs. browserauth writes it; the shell only reads it.
type savedRecord struct {
	Certificate config.CertificateInfo `json:"certificate"`
	Tokens      struct {
		Access string `json:"access_token"`
	} `json:"tokens"`
	Profile browserauth.Profile `json:"profile"`
}

// operator returns the selected organization's saved sign-in and its operator
// key. The first use in this worker, a change of organization, and (when fresh
// is set) a cloud token with under a minute left all go through Restore, which
// validates the record and refreshes the token. The seed is never returned.
func (b *bridge) operator(ctx context.Context, o org, fresh bool) (savedRecord, *mldsa.PrivateKey, error) {
	current := b.auth.Profile()
	if o.Tenant != "" && (o.Tenant != current.Tenant || o.Subject != "" && o.Subject != current.Subject) {
		if err := selectOrg(o); err != nil {
			return savedRecord{}, nil, err
		}
	}
	load := func() (savedRecord, error) {
		var saved savedRecord
		raw, err := browserCredentialStore{}.Load()
		if err == nil {
			err = json.Unmarshal(raw, &saved)
		}
		return saved, err
	}
	saved, err := load()
	expires, _ := time.Parse(time.RFC3339, saved.Profile.Expires)
	if err != nil || current.Subject == "" || saved.Profile.Tenant != current.Tenant || saved.Profile.Subject != current.Subject || fresh && time.Until(expires) < time.Minute {
		profile, err := b.auth.Restore(ctx)
		if err != nil {
			return savedRecord{}, nil, err
		}
		if profile == nil {
			return savedRecord{}, nil, errors.New("Sign in with Wendy first")
		}
		if saved, err = load(); err != nil {
			return savedRecord{}, nil, err
		}
	}
	// Another tab may have rotated the token since, so only the identity must match.
	signedIn := b.auth.Profile()
	if saved.Profile.Tenant != signedIn.Tenant || saved.Profile.Subject != signedIn.Subject || o.Tenant != "" && saved.Profile.Tenant != o.Tenant {
		return savedRecord{}, nil, errors.New("Sign in with Wendy first")
	}
	seed, err := browserCredentialStore{}.LoadKey("operator-key:" + saved.Profile.Tenant + ":" + saved.Profile.Subject)
	if err != nil {
		return savedRecord{}, nil, errors.New("Saved operator key is invalid. Sign in again")
	}
	defer clear(seed)
	key, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), seed)
	if err != nil {
		return savedRecord{}, nil, errors.New("Saved operator key is invalid. Sign in again")
	}
	block, _ := pem.Decode([]byte(saved.Certificate.PemCertificate))
	if block == nil {
		return savedRecord{}, nil, errors.New("Saved certificate is invalid. Sign in again")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return savedRecord{}, nil, errors.New("Saved certificate is invalid. Sign in again")
	}
	want, _ := x509.MarshalPKIXPublicKey(key.Public())
	got, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || string(want) != string(got) {
		return savedRecord{}, nil, errors.New("Saved certificate does not match the operator. Sign in again")
	}
	if now := time.Now(); now.After(leaf.NotAfter) || now.Before(leaf.NotBefore) {
		return savedRecord{}, nil, errors.New("Saved certificate has expired or is not yet valid. Sign in again")
	}
	return saved, key, nil
}

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
