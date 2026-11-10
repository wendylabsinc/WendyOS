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

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/modelhub"
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
	vpnControl      sync.Mutex
	vpnMu           sync.Mutex
	engine          *vpn.Engine
	vpnConfig       vpn.Config
	vpnController   vpn.Config
	vpnContext      context.Context
	vpnCancel       context.CancelFunc
	vpnReachability vpn.ReachabilityState

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
		"StartVPN": c.startVPN, "RefreshVPN": c.refreshVPN, "CancelVPN": c.cancelVPN, "StopVPN": c.stopVPN, "VPNRead": c.readPacket, "VPNWrite": c.writePacket,
		"NearbyDiscover": b.NearbyDiscover, "NearbyOpen": b.NearbyOpen, "NearbyClose": b.NearbyClose, "NearbyAction": b.NearbyAction,
		// Setup state survives a failed step, as in the iOS bridge: attempted and
		// pending must stay visible so enrollment is never sent twice.
		"NearbySetupOpen": func(address string) nearbySetupResult {
			state, err := b.NearbyOpen(address)
			return newNearbySetupResult(state, err)
		},
		"NearbySetupAction": func(session uint64, action int, ssid, password, account, organization, name string) nearbySetupResult {
			state, err := b.NearbyAction(session, action, ssid, password, account, organization, name)
			return newNearbySetupResult(state, err)
		},
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
		// Calls added with the redesigned iOS app (#2715).
		"CameraControls": b.CameraControls, "SetWiFiPriority": b.SetWiFiPriority, "StartHostedChat": b.StartHostedChat,
		"ManageDevice": b.ManageDevice, "PrepareLocalReset": b.PrepareLocalReset, "ResetLocalEnrollment": b.ResetLocalEnrollment,
		"MobileOSUpdate": b.MobileOSUpdate, "SiteModels": b.SiteModels,
		"BeginRecordingRead": b.BeginRecordingRead, "EndRecordingRead": b.EndRecordingRead, "RecordingLibrary": b.RecordingLibrary,
		"RecordingDetails": b.RecordingDetails, "PlayRecording": b.PlayRecording, "ExportRecordings": b.ExportRecordings,
		// Thumbnails carry their format, as the iOS bridge reports it beside the bytes.
		"RecordingThumbnail": func(id uint64, selector string, secure bool, episode string, file data.File) (map[string]any, error) {
			raw, err := b.RecordingThumbnail(id, selector, secure, episode, file)
			if err != nil {
				return nil, err
			}
			return map[string]any{"format": backend.RecordingPreviewFormat(file), "bytes": raw}, nil
		},
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
		"DeviceAccess": func(account, org, device string) (any, error) {
			ctx, cancel := cloudContext()
			defer cancel()
			return commands.DesktopDeviceAccess(ctx, account, org, device)
		},
		// Hugging Face lookups match the iOS bridge's timeouts. They read public
		// metadata only; nothing is downloaded or executed on the phone.
		"SearchModels": func(query string) ([]modelhub.Repository, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return modelhub.New().Search(ctx, query)
		},
		"ModelFiles": func(repo string) (modelhub.Details, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return modelhub.New().Details(ctx, repo)
		},
		"VisionModel": func(repo string, classification bool) (modelhub.VisionModel, error) {
			task := "object-detection"
			if classification {
				task = "image-classification"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			return modelhub.New().VisionDetails(ctx, repo, task)
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
	c.vpnControl.Lock()
	defer c.vpnControl.Unlock()
	c.vpnMu.Lock()
	running := c.engine != nil
	c.vpnMu.Unlock()
	if !active && running {
		return c.backend.Disconnect()
	}
	return c.backend.SetMobileState(active, account, organization)
}
func (c *Client) startVPN(account, organization string) (vpn.Config, error) {
	c.vpnControl.Lock()
	defer c.vpnControl.Unlock()
	c.vpnMu.Lock()
	running := c.engine != nil
	c.vpnMu.Unlock()
	if running {
		return vpn.Config{}, errors.New("stop the current VPN first")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.vpnMu.Lock()
	c.vpnContext, c.vpnCancel = ctx, cancel
	c.vpnMu.Unlock()
	// Consent backgrounds the Activity. Restore the broker before starting the
	// same automatic controller used by the iOS packet tunnel extension.
	if err := c.backend.SetMobileState(true, account, organization); err != nil {
		cancel()
		return vpn.Config{}, err
	}
	if err := ctx.Err(); err != nil {
		return vpn.Config{}, err
	}
	controller, err := c.backend.StartVPN(nil, "auto")
	if err != nil {
		cancel()
		return vpn.Config{}, err
	}
	initial, stop := context.WithTimeout(ctx, 25*time.Second)
	config, err := vpn.Refresh(initial, controller, true)
	stop()
	if err == nil {
		var engine *vpn.Engine
		engine, err = newVPNEngine(config)
		if err == nil {
			c.vpnMu.Lock()
			c.engine, c.vpnConfig, c.vpnController = engine, config, controller
			c.vpnMu.Unlock()
			c.vpnReachability = vpn.ReachabilityState{}
			return config, nil
		}
	}
	cancel()
	_ = c.backend.StopVPN()
	return vpn.Config{}, err
}

func (c *Client) refreshVPN() (*vpn.Config, error) {
	c.vpnControl.Lock()
	defer c.vpnControl.Unlock()
	c.vpnMu.Lock()
	session, controller := c.vpnContext, c.vpnController
	running := c.engine != nil
	c.vpnMu.Unlock()
	if !running || session == nil {
		return nil, errors.New("VPN is stopped")
	}
	ctx, cancel := context.WithTimeout(session, vpn.RefreshTimeout+10*time.Second)
	defer cancel()
	online := c.vpnReachability.Observe(vpn.InternetReachable(ctx))
	return c.refreshVPNConfig(ctx, controller, online)
}

// Caller holds vpnControl. A failed refresh retains the last device routes,
// matching iOS; a lost controller ends the session instead of showing a dead VPN.
func (c *Client) refreshVPNConfig(ctx context.Context, controller vpn.Config, online bool) (*vpn.Config, error) {
	config, err := vpn.Refresh(ctx, controller, online)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !vpn.Healthy(ctx, controller) {
			return nil, errors.New("Wendy lost its mesh relay. Turn the VPN off and on again.")
		}
		return nil, nil
	}
	c.vpnMu.Lock()
	current := c.vpnConfig.Endpoint
	c.vpnMu.Unlock()
	if config.Endpoint == current {
		return nil, nil
	}
	next, err := newVPNEngine(config)
	if err != nil {
		return nil, nil
	}
	c.vpnMu.Lock()
	old := c.engine
	c.engine, c.vpnConfig = next, config
	c.vpnMu.Unlock()
	if old != nil {
		old.Close()
	}
	return &config, nil
}

func (c *Client) cancelVPN() {
	c.vpnMu.Lock()
	cancel := c.vpnCancel
	c.vpnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) stopVPN() error {
	c.cancelVPN()
	c.vpnControl.Lock()
	defer c.vpnControl.Unlock()
	c.vpnMu.Lock()
	engine := c.engine
	c.engine, c.vpnContext, c.vpnCancel = nil, nil, nil
	c.vpnConfig, c.vpnController = vpn.Config{}, vpn.Config{}
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

type nearbySetupResult struct {
	State backend.NearbySetupState
	Error string `json:",omitempty"`
}

func newNearbySetupResult(state backend.NearbySetupState, err error) nearbySetupResult {
	result := nearbySetupResult{State: state}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
