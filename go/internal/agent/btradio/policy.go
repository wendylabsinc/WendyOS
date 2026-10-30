// Package btradio coordinates power policy across the agent's Bluetooth advertisements.
package btradio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

var policyPath = "/config/bluetooth-radio.json"
var mu sync.Mutex
var changes sync.Mutex
var loaded bool
var desired = &pb.BluetoothPowerSetting{Mode: "default"}
var loadError error

type registration struct {
	owner context.Context
	apply func(context.Context) error
}

var handlers = map[string]*registration{}
var reports = map[string]*pb.BluetoothAdvertisingPowerStatus{}

func loadLocked() {
	if loaded {
		return
	}
	loaded = true
	data, err := os.ReadFile(policyPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		loadError = err
		return
	}
	var value pb.BluetoothPowerSetting
	if err = json.Unmarshal(data, &value); err != nil {
		loadError = err
		return
	}
	if err = Validate(&value); err != nil {
		loadError = err
		return
	}
	desired = &value
}
func Validate(p *pb.BluetoothPowerSetting) error {
	if p == nil {
		return fmt.Errorf("power setting is missing")
	}
	switch p.Mode {
	case "default", "maximum":
		if p.Dbm != 0 {
			return fmt.Errorf("dBm is only valid in dbm mode")
		}
	case "dbm":
		if p.Dbm < -127 || p.Dbm > 20 {
			return fmt.Errorf("advertising power must be between -127 and 20 dBm")
		}
	default:
		return fmt.Errorf("power must be default, maximum, or a dBm value")
	}
	return nil
}
func Desired() (*pb.BluetoothPowerSetting, error) {
	mu.Lock()
	defer mu.Unlock()
	loadLocked()
	return proto.Clone(desired).(*pb.BluetoothPowerSetting), loadError
}

// PowerBounds excludes malformed/sentinel capability ranges and limits requests
// to the same +20 dBm ceiling accepted by management validation.
func PowerBounds(min, max int32) (int32, int32, bool) {
	if min < -127 || max < -127 || min > max {
		return 0, 0, false
	}
	if max > 20 {
		max = 20
	}
	return min, max, min <= max
}

// Request resolves a policy against the selected controller's advertised limits.
// A nil result means leave power selection to BlueZ/the controller.
func Request(canSet bool, min, max int32) (*int16, error) {
	p, err := Desired()
	if err != nil {
		return nil, err
	}
	if p.Mode == "default" {
		return nil, nil
	}
	if !canSet {
		return nil, fmt.Errorf("this controller cannot set advertising TX power")
	}
	var valid bool
	min, max, valid = PowerBounds(min, max)
	if !valid {
		return nil, fmt.Errorf("controller reported an invalid advertising TX power range")
	}
	value := p.Dbm
	if p.Mode == "maximum" {
		value = max
	}
	if value < min || value > max {
		return nil, fmt.Errorf("advertising power %d dBm is outside this controller's [%d, %d] dBm range", value, min, max)
	}
	result := int16(value)
	return &result, nil
}

// Register applies the current policy and subscribes atomically with Set. The
// returned function waits for in-flight changes before removing this owner.
// A returned subscription remains live after initial failure, so a corrected
// policy can recover advertising without restarting the agent.
func Register(owner context.Context, name string, apply func(context.Context) error) (func(), error) {
	changes.Lock()
	defer changes.Unlock()
	if err := owner.Err(); err != nil {
		return nil, err
	}
	err := apply(owner)
	r := &registration{owner: owner, apply: apply}
	mu.Lock()
	handlers[name] = r
	mu.Unlock()
	return func() {
		changes.Lock()
		defer changes.Unlock()
		mu.Lock()
		defer mu.Unlock()
		if handlers[name] == r {
			delete(handlers, name)
			delete(reports, name)
		}
	}, err
}

// ApplyError means intent was saved, but an active advertisement rejected it.
type ApplyError struct{ Err error }

func (e *ApplyError) Error() string {
	return "Bluetooth policy saved but not fully applied: " + e.Err.Error()
}
func (e *ApplyError) Unwrap() error { return e.Err }

func Report(name string, requested *int16, err error) {
	s := &pb.BluetoothAdvertisingPowerStatus{Name: name, Registered: err == nil}
	if requested != nil {
		v := int32(*requested)
		s.RequestedDbm = &v
	}
	if err != nil {
		s.Error = err.Error()
	}
	mu.Lock()
	reports[name] = s
	mu.Unlock()
}

// ReportFailure preserves the last known registration when a new request fails
// before touching it (for example while waiting for the effect gate).
func ReportFailure(name string, err error) {
	if err == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	previous := reports[name]
	if previous == nil {
		previous = &pb.BluetoothAdvertisingPowerStatus{Name: name}
	} else {
		previous = proto.Clone(previous).(*pb.BluetoothAdvertisingPowerStatus)
	}
	previous.Error = err.Error()
	reports[name] = previous
}

func Reports() []*pb.BluetoothAdvertisingPowerStatus {
	mu.Lock()
	defer mu.Unlock()
	out := make([]*pb.BluetoothAdvertisingPowerStatus, 0, len(reports))
	for _, v := range reports {
		out = append(out, proto.Clone(v).(*pb.BluetoothAdvertisingPowerStatus))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Set persists only user configuration, then updates every active Wendy-owned
// advertising instance. Re-registration never disconnects established links.
// A partial application is returned as an error, retaining the saved intent.
func Set(ctx context.Context, p *pb.BluetoothPowerSetting) error {
	changes.Lock()
	defer changes.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := Validate(p); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(policyPath), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(policyPath), ".bluetooth-radio-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(path, policyPath); err != nil {
		return err
	}
	mu.Lock()
	loaded = true
	loadError = nil
	desired = proto.Clone(p).(*pb.BluetoothPowerSetting)
	list := make([]*registration, 0, len(handlers))
	for _, f := range handlers {
		list = append(list, f)
	}
	mu.Unlock()
	var failures []error
	for _, r := range list {
		if r.owner.Err() != nil {
			continue
		}
		applyCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(r.owner, cancel)
		if r.owner.Err() != nil {
			cancel()
		}
		err = r.apply(applyCtx)
		stop()
		cancel()
		if err != nil && r.owner.Err() == nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return &ApplyError{Err: err}
	}
	return nil
}
