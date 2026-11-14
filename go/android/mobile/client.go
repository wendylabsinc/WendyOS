// Package mobile exposes the shared Apple/Android backend through a bounded,
// process-local JSON ABI. It does not listen on a socket or accept web content.
package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const maxRequestBytes = 2 << 20

type request struct {
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}
type response struct {
	Value            any    `json:"value,omitempty"`
	Error            string `json:"error,omitempty"`
	IdentityRecovery bool   `json:"identityRecovery,omitempty"`
}

type Client struct {
	vpnControl sync.Mutex
	vpnMu      sync.Mutex
	engine     *vpn.Engine

	backend    *backend.Backend
	initialize sync.Once
	initErr    error
	ready      bool
	mu         sync.RWMutex
	methods    map[string]any
}

func New() *Client {
	b := backend.New()
	c := &Client{backend: b}
	// This allowlist is deliberately explicit. Adding a public Backend method does
	// not automatically expose it to Android. The UI owns destructive confirmations.
	c.methods = map[string]any{
		"StartVPN": c.startVPN, "StopVPN": c.stopVPN, "VPNRead": c.readPacket, "VPNWrite": c.writePacket,
		"NearbyDiscover": b.NearbyDiscover, "NearbyOpen": b.NearbyOpen, "NearbyClose": b.NearbyClose, "NearbyAction": b.NearbyAction,
		"Nearby": b.Nearby, "Cloud": b.Cloud, "Account": b.Account, "Accounts": b.Accounts,
		"Login": b.Login, "CancelLogin": b.CancelLogin, "SetMobileState": c.setMobileState,
		"MobileMeshSummary": func() map[string]string {
			state, detail := b.MobileMeshSummary()
			return map[string]string{"state": state, "detail": detail}
		},
		"Connect": func(selector string, forget bool) (any, error) {
			session, err := b.Connect(selector, forget)
			return map[string]any{"session": session, "lite": b.IsLite(session)}, err
		},
		"Disconnect": b.Disconnect, "Shutdown": b.Shutdown, "Poll": b.Poll,
		"Apps": b.Apps, "StartApp": b.StartApp, "StopApp": b.StopApp, "DeleteApp": b.DeleteApp, "OpenApp": b.OpenApp, "Ports": b.Ports,
		"DeviceInfo": b.DeviceInfo, "Resources": b.Resources, "NetworkTraffic": b.NetworkTraffic, "SensorTemperature": b.SensorTemperature,
		"Cameras": b.Cameras, "CameraMetadata": b.CameraMetadata,
		"StartCamera": func(camera uint32, stream, session uint64, stableID string) error {
			return b.StartCamera(camera, stream, session, stableID)
		},
		"StopCamera": b.StopCamera, "StopCameras": b.StopCameras, "StartLogs": b.StartLogs, "StopLogs": b.StopLogs,
		"NetworkSettings": b.NetworkSettings, "JoinWiFi": b.JoinWiFi, "DisconnectWiFi": b.DisconnectWiFi, "ForgetWiFi": b.ForgetWiFi, "ConfigureMesh": b.ConfigureMesh,
		"DeviceEnrollment": b.DeviceEnrollment, "EnrollmentOrganizations": b.EnrollmentOrganizations, "EnrollDevice": b.EnrollDevice,
		"BluetoothScan": b.BluetoothScan, "BluetoothAction": b.BluetoothAction, "AudioDevices": b.AudioDevices, "SetDefaultAudio": b.SetDefaultAudio,
		"OnlineUpdate": b.OnlineUpdate, "Pairings": b.Pairings, "PairSensor": b.PairSensor, "PairIPCamera": b.PairIPCamera,
		"UnitreePairings": b.UnitreePairings, "PairUnitree": b.PairUnitree, "ForgetUnitree": b.ForgetUnitree,
		"Campaigns": b.Campaigns, "CampaignSources": b.CampaignSources, "DeployCampaign": b.DeployCampaign, "CampaignClips": b.CampaignClips,
		"CampaignSettings": b.CampaignSettings, "UpdateCampaignSettings": b.UpdateCampaignSettings,
		"CampaignNotifications": b.CampaignNotifications, "SetCampaignNotifications": b.SetCampaignNotifications,
		"VisualEpisodes": b.VisualEpisodes, "VisualEpisode": b.VisualEpisode, "ExportVisualEpisode": b.ExportVisualEpisode,
		"VisualRecordingStatus": b.VisualRecordingStatus, "StartVisualRecording": b.StartVisualRecording, "StopVisualRecording": b.StopVisualRecording,
		"ChatModels": b.ChatModels, "StartChat": b.StartChat, "PollChat": b.PollChat, "StopChat": b.StopChat, "ResolveChatApproval": b.ResolveChatApproval,
		"ModelHardware": b.ModelHardware, "StartModelRun": b.StartModelRun, "ModelRunStatus": b.ModelRunStatus, "CancelModelRun": b.CancelModelRun,
		"SaveCameraAlignment": b.SaveCameraAlignment, "SaveCameraLens": b.SaveCameraLens,
		"Organizations": func(account string) (any, error) {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopOrganizations(ctx, account)
		},
		"Organization": func(account, org string) (any, error) {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopOrganizationOverview(ctx, account, org)
		},
		"Fleet": func(account, org string) (any, error) {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopFleetOverview(ctx, account, org)
		},
		"AddMember": func(account, org, email, role string) error {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopAddMember(ctx, account, org, email, role)
		},
		"SetMemberRole": func(account, org, user, role string) error {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopSetMemberRole(ctx, account, org, user, role)
		},
		"RemoveMember": func(account, org, user string) error {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopRemoveMember(ctx, account, org, user)
		},
		"CreateInvite": func(account, org, role string, expires int64) (string, error) {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopCreateInvite(ctx, account, org, role, expires)
		},
		"RevokeInvite": func(account, org, id string) error {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopRevokeInvite(ctx, account, org, id)
		},
		"CreateAccess": func(account, org, user, target string, effect cloudpb.AccessEffect) error {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopCreateAccess(ctx, account, org, user, target, effect)
		},
		"DeleteAccess": func(account, org, id string) error {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopDeleteAccess(ctx, account, org, id)
		},
	}
	return c
}
func cloudContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func (c *Client) Initialize(directory, cache string) error {
	c.initialize.Do(func() {
		for _, path := range []string{directory, cache} {
			if !filepath.IsAbs(path) {
				c.initErr = errors.New("app directories must be absolute")
				return
			}
			if err := os.MkdirAll(path, 0700); err != nil {
				c.initErr = err
				return
			}
		}
		if err := os.Setenv("WENDY_CONFIG_DIR", directory); err != nil {
			c.initErr = err
			return
		}
		if err := os.Setenv("TMPDIR", cache); err != nil {
			c.initErr = err
			return
		}
		c.initErr = c.backend.Initialize("")
		c.mu.Lock()
		c.ready = c.initErr == nil
		c.mu.Unlock()
	})
	return c.initErr
}

// Invoke can run concurrently with Poll and lifecycle cancellation. Backend
// operations retain their own session validation and serialization.
func (c *Client) Invoke(input []byte) []byte {
	value, err := c.invoke(input)
	r := response{Value: value}
	if err != nil {
		r.Value = nil
		r.Error = err.Error()
		var recovery *backend.IdentityRecoveryError
		r.IdentityRecovery = errors.As(err, &recovery)
	}
	output, err := json.Marshal(r)
	if err != nil {
		output, _ = json.Marshal(response{Error: "encode response: " + err.Error()})
	}
	return output
}
func (c *Client) invoke(input []byte) (any, error) {
	if len(input) > maxRequestBytes {
		return nil, errors.New("request exceeds 2 MiB")
	}
	var req request
	if err := decode(input, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if req.Method == "Initialize" {
		if len(req.Args) != 2 {
			return nil, errors.New("Initialize requires configuration and cache directories")
		}
		var directory, cache string
		if err := decode(req.Args[0], &directory); err != nil {
			return nil, err
		}
		if err := decode(req.Args[1], &cache); err != nil {
			return nil, err
		}
		return nil, c.Initialize(directory, cache)
	}
	c.mu.RLock()
	ready := c.ready
	c.mu.RUnlock()
	if !ready {
		return nil, errors.New("Wendy has not initialized")
	}
	method, ok := c.methods[req.Method]
	if !ok {
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
	return invoke(method, req.Args)
}
func decode(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}
func invoke(method any, args []json.RawMessage) (any, error) {
	f := reflect.ValueOf(method)
	t := f.Type()
	if len(args) != t.NumIn() {
		return nil, fmt.Errorf("expected %d arguments, received %d", t.NumIn(), len(args))
	}
	values := make([]reflect.Value, len(args))
	for i, raw := range args {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("argument %d must not be null", i+1)
		}
		value := reflect.New(t.In(i))
		if err := decode(raw, value.Interface()); err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		values[i] = value.Elem()
	}
	results := f.Call(values)
	if len(results) > 0 && t.Out(len(results)-1).Implements(reflect.TypeFor[error]()) {
		last := results[len(results)-1]
		results = results[:len(results)-1]
		if !last.IsNil() {
			return nil, last.Interface().(error)
		}
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0].Interface(), nil
}

func (c *Client) setMobileState(active bool, account, organization string) error {
	c.vpnMu.Lock()
	running := c.engine != nil
	c.vpnMu.Unlock()
	if !active && running {
		return c.backend.Disconnect()
	}
	return c.backend.SetMobileState(active, account, organization)
}
func (c *Client) startVPN(routes []vpn.Route) (vpn.Config, error) {
	c.vpnControl.Lock()
	defer c.vpnControl.Unlock()
	c.vpnMu.Lock()
	defer c.vpnMu.Unlock()
	if c.engine != nil {
		return vpn.Config{}, errors.New("stop the current VPN first")
	}
	config, err := c.backend.StartVPN(routes)
	if err != nil {
		return vpn.Config{}, err
	}
	engine, err := vpn.NewEngine(config)
	if err != nil {
		_ = c.backend.StopVPN()
		return vpn.Config{}, err
	}
	c.engine = engine
	return config, nil
}
func (c *Client) stopVPN() error {
	c.vpnControl.Lock()
	defer c.vpnControl.Unlock()
	c.vpnMu.Lock()
	engine := c.engine
	c.engine = nil
	c.vpnMu.Unlock()
	if engine != nil {
		engine.Close()
	}
	return c.backend.StopVPN()
}
func (c *Client) readPacket() ([]byte, error) {
	c.vpnMu.Lock()
	engine := c.engine
	c.vpnMu.Unlock()
	if engine == nil {
		return nil, errors.New("VPN is stopped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	return engine.Read(ctx), nil
}
func (c *Client) writePacket(packet []byte) error {
	if len(packet) == 0 || len(packet) > 65535 {
		return errors.New("invalid VPN packet size")
	}
	c.vpnMu.Lock()
	engine := c.engine
	c.vpnMu.Unlock()
	if engine == nil {
		return errors.New("VPN is stopped")
	}
	engine.Write(packet)
	return nil
}
