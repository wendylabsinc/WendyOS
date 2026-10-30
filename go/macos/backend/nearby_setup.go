package backend

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/nearbysetup"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type NearbyDevice struct {
	Address, Name string
	RSSI          int
}
type NearbySetupState struct {
	Session                                                                 uint64
	Connected, Lite, Enrolled, Attempted, Pending, Completed, WiFiConnected bool
	SSID, DeviceID                                                          string
	Networks                                                                []string
}
type nearbyAttempt struct {
	attempted  bool
	credential *commands.DesktopNearbyCredential
	completed  bool
}
type nearbyConnection interface {
	Status(context.Context, bool) (*pb.SetupStatusResponse, error)
	Call(context.Context, *pb.BluetoothCommand) (*pb.BluetoothResponse, error)
	Close()
}

var nearbyConnectFn = func(ctx context.Context, address string) (nearbyConnection, error) {
	return nearbysetup.Connect(ctx, address)
}
var nearbyPrepareFn = commands.DesktopPrepareNearbyEnrollment
var nearbyCheckFn = commands.DesktopCheckNearbyEnrollment

type nearbySetup struct {
	mu       sync.Mutex
	next     uint64
	state    NearbySetupState
	client   nearbyConnection
	address  string
	attempts map[string]*nearbyAttempt
}

func (b *Backend) NearbyDiscover() ([]NearbyDevice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	stream, err := scan.DiscoverBluetoothContinuous(ctx, scan.Options{Services: []string{nearbysetup.ServiceUUID}})
	if err != nil {
		return nil, errors.New("Bluetooth is unavailable. Enable Bluetooth and allow Wendy access in Settings")
	}
	found := map[string]scan.BLEDeviceInfo{}
	for snapshot := range stream {
		for _, d := range snapshot {
			if !nearbySetupSighting(d, time.Now()) {
				continue
			}
			found[d.Address] = d
		}
	}
	out := make([]NearbyDevice, 0, len(found))
	for _, d := range found {
		if nearbySetupSighting(d, time.Now()) {
			out = append(out, NearbyDevice{Address: d.Address, Name: d.Name, RSSI: d.RSSI})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Address < out[j].Address
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func nearbySetupSighting(d scan.BLEDeviceInfo, now time.Time) bool {
	age := now.Sub(d.LastSeen)
	return d.Address != "" && d.RSSI >= -80 && d.RSSI < 0 && !d.LastSeen.IsZero() && age >= 0 && age < 20*time.Second
}
func (n *nearbySetup) snapshot() NearbySetupState {
	s := n.state
	s.Networks = append([]string(nil), s.Networks...)
	if a := n.attempts[n.address]; a != nil {
		s.Attempted = a.attempted
		s.Completed = a.completed
		if a.credential != nil {
			s.Pending = !a.completed
			s.DeviceID = a.credential.DeviceID
		}
	}
	return s
}
func (b *Backend) NearbyOpen(address string) (NearbySetupState, error) {
	n := &b.nearby
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client != nil {
		n.client.Close()
		n.client = nil
	}
	n.next++
	n.address = address
	n.state = NearbySetupState{Session: n.next}
	if n.attempts == nil {
		n.attempts = map[string]*nearbyAttempt{}
	}
	if n.attempts[address] == nil {
		n.attempts[address] = &nearbyAttempt{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	c, err := nearbyConnectFn(ctx, address)
	if err != nil {
		return n.snapshot(), err
	}
	n.client = c
	state, err := c.Status(ctx, false)
	if err != nil {
		c.Close()
		n.client = nil
		return n.snapshot(), err
	}
	n.state.Connected = true
	n.state.Lite = state.GetDeviceClass() == "C"
	n.state.Enrolled = state.GetEnrolled()
	if !state.GetEnrolled() && !(state.GetAcmeSupported() || n.state.Lite && state.GetLiteSupported()) {
		c.Close()
		n.client = nil
		n.state.Connected = false
		return n.snapshot(), errors.New("Update this device to support nearby enrollment")
	}
	return n.snapshot(), nil
}
func (b *Backend) NearbyClose(session uint64) {
	n := &b.nearby
	n.mu.Lock()
	defer n.mu.Unlock()
	if session != 0 && n.state.Session != session {
		return
	}
	if n.client != nil {
		n.client.Close()
		n.client = nil
	}
	n.state.Connected = false
}

// Actions: 0 status/keepalive, 1 scan Wi-Fi, 2 connect Wi-Fi, 3 enroll, 4 check Cloud.
func (b *Backend) NearbyAction(session uint64, action int, ssid, password, account, organization, name string) (NearbySetupState, error) {
	n := &b.nearby
	n.mu.Lock()
	defer n.mu.Unlock()
	if session == 0 || n.state.Session != session {
		return NearbySetupState{}, errors.New("This setup session has ended")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	a := n.attempts[n.address]
	if action == 4 {
		if a == nil || a.credential == nil {
			return n.snapshot(), errors.New("Check your Cloud device list before starting enrollment again")
		}
		ok, err := nearbyCheckFn(ctx, a.credential)
		if ok {
			a.completed = true
		}
		return n.snapshot(), err
	}
	if n.client == nil || !n.state.Connected {
		return n.snapshot(), errors.New("Bluetooth connection ended. Retry connection")
	}
	fail := func(err error) (NearbySetupState, error) {
		n.state.Connected = false
		n.client.Close()
		return n.snapshot(), err
	}
	switch action {
	case 0:
		state, err := n.client.Status(ctx, false)
		if err != nil {
			return fail(err)
		}
		n.state.Enrolled = state.GetEnrolled()
	case 1:
		r, err := n.client.Call(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_WifiList{WifiList: &pb.WifiListCommand{}}})
		if err != nil {
			return fail(err)
		}
		if r.GetWifiList() == nil {
			return n.snapshot(), errors.New("Could not scan Wi-Fi networks; try again")
		}
		n.state.Networks = nil
		for _, network := range r.GetWifiList().GetNetworks() {
			n.state.Networks = append(n.state.Networks, network.GetSsid())
		}
	case 2:
		if ssid == "" || len(ssid) > 32 || len(password) > 64 {
			return n.snapshot(), errors.New("Enter a valid network name and password")
		}
		r, err := n.client.Call(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_WifiConnect{WifiConnect: &pb.WifiConnectCommand{Ssid: ssid, Password: password}}})
		if err != nil {
			return fail(err)
		}
		if !r.GetWifiConnect().GetSuccess() {
			return n.snapshot(), errors.New("Wi-Fi did not connect. Check the network and password")
		}
		r, err = n.client.Call(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_WifiStatus{WifiStatus: &pb.WifiStatusCommand{}}})
		if err != nil {
			return fail(err)
		}
		wifi := r.GetWifiStatus()
		n.state.WiFiConnected = wifi.GetConnected()
		n.state.SSID = wifi.GetSsid()
		if !n.state.WiFiConnected || n.state.SSID != ssid {
			return n.snapshot(), errors.New("The requested Wi-Fi connection was not confirmed")
		}
	case 3:
		if a.attempted {
			return n.snapshot(), errors.New("Enrollment was already requested. Check its status before trying again")
		}
		state, err := n.client.Status(ctx, n.state.Lite)
		if err != nil {
			return fail(err)
		}
		n.state.Enrolled = state.GetEnrolled()
		credential, err := nearbyPrepareFn(ctx, account, organization, name, state, func() { a.attempted = true })
		if err != nil {
			return n.snapshot(), err
		}
		a.credential = credential
		r, err := n.client.Call(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_SetupEnroll{SetupEnroll: credential.Request}})
		clear(credential.Request.LiteConfig)
		credential.Request = nil
		if err != nil {
			return fail(errors.New("Enrollment was sent but its reply was lost. Check enrollment status"))
		}
		reply := r.GetSetupEnroll()
		if reply == nil || reply.GetPrincipalUri() != credential.Principal || reply.GetPending() != credential.Lite {
			return n.snapshot(), errors.New("Enrollment was not confirmed by the device. Check enrollment status")
		}
		if credential.Lite {
			n.client.Close()
			n.state.Connected = false
		} else {
			a.completed = true
		}
	default:
		return n.snapshot(), errors.New("Unknown setup action")
	}
	return n.snapshot(), nil
}
