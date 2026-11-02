//go:build linux

package btradio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"golang.org/x/sys/unix"
)

// BlueZ remains the discovery/advertising owner. Management changes its selected
// scan/initiation PHYs; raw HCI is used only for a Wendy-owned connection handle.
// No controller reset, user channel takeover or vendor command is used.
var controllerGate = make(chan struct{}, 1)

type controllerIO struct {
	send  func([]byte) error
	read  func(context.Context) ([]byte, error)
	close func()
}

var openController = controllerSocket

type managementError struct {
	opcode uint16
	status byte
}

func (e *managementError) Error() string {
	return fmt.Sprintf("Bluetooth management 0x%04x rejected: status 0x%02x", e.opcode, e.status)
}

func adapterIndex(adapter string) (uint16, error) {
	if !strings.HasPrefix(adapter, "/org/bluez/hci") {
		return 0, fmt.Errorf("invalid Bluetooth adapter %q", adapter)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(adapter, "/org/bluez/hci"), 10, 16)
	return uint16(n), err
}
func waitSocket(ctx context.Context, fd int, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return fmt.Errorf("Bluetooth socket closed")
			}
			return nil
		}
	}
}
func controllerSocket(index uint16, management bool) (*controllerIO, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, err
	}
	channel := uint16(unix.HCI_CHANNEL_MONITOR)
	if management {
		channel = unix.HCI_CHANNEL_CONTROL
	}
	if err = unix.Bind(fd, &unix.SockaddrHCI{Dev: 0xffff, Channel: channel}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	sendFD := fd
	if !management {
		sendFD, err = unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
		if err == nil {
			err = unix.Bind(sendFD, &unix.SockaddrHCI{Dev: index, Channel: unix.HCI_CHANNEL_RAW})
		}
		if err != nil {
			if sendFD >= 0 {
				unix.Close(sendFD)
			}
			unix.Close(fd)
			return nil, err
		}
	}
	return &controllerIO{close: func() {
		if sendFD != fd {
			unix.Close(sendFD)
		}
		unix.Close(fd)
	},
		send: func(p []byte) error {
			n, e := unix.Write(sendFD, p)
			if e == nil && n != len(p) {
				e = fmt.Errorf("short Bluetooth command write")
			}
			return e
		},
		read: func(ctx context.Context) ([]byte, error) {
			for {
				if err := waitSocket(ctx, fd, unix.POLLIN); err != nil {
					return nil, err
				}
				var p [4096]byte
				n, e := unix.Read(fd, p[:])
				if e == unix.EAGAIN || e == unix.EINTR {
					continue
				}
				if e != nil {
					return nil, e
				}
				if management {
					return append([]byte(nil), p[:n]...), nil
				}
				if n < 6 || binary.LittleEndian.Uint16(p[2:4]) != index || binary.LittleEndian.Uint16(p[:2]) != 3 {
					continue
				}
				length := int(binary.LittleEndian.Uint16(p[4:6]))
				if length != n-6 {
					return nil, fmt.Errorf("invalid HCI monitor frame")
				}
				return append([]byte{4}, p[6:n]...), nil
			}
		}}, nil
}
func withController(ctx context.Context, adapter string, management bool, run func(context.Context, *controllerIO) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case controllerGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-controllerGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	index, err := adapterIndex(adapter)
	if err != nil {
		return err
	}
	io, err := openController(index, management)
	if err != nil {
		return err
	}
	defer io.close()
	return run(ctx, io)
}
func managementCommand(ctx context.Context, io *controllerIO, index, opcode uint16, data []byte) ([]byte, error) {
	p := make([]byte, 6+len(data))
	binary.LittleEndian.PutUint16(p, opcode)
	binary.LittleEndian.PutUint16(p[2:], index)
	binary.LittleEndian.PutUint16(p[4:], uint16(len(data)))
	copy(p[6:], data)
	if err := io.send(p); err != nil {
		return nil, err
	}
	for {
		p, err := io.read(ctx)
		if err != nil {
			return nil, err
		}
		if len(p) < 6 || int(binary.LittleEndian.Uint16(p[4:6])) != len(p)-6 {
			return nil, fmt.Errorf("invalid management response")
		}
		event := binary.LittleEndian.Uint16(p)
		if binary.LittleEndian.Uint16(p[2:]) != index || (event != 1 && event != 2) {
			continue
		}
		if len(p) < 9 {
			return nil, fmt.Errorf("short management command response")
		}
		if binary.LittleEndian.Uint16(p[6:]) != opcode {
			continue
		}
		if p[8] != 0 {
			return nil, &managementError{opcode, p[8]}
		}
		if event == 1 {
			return p[9:], nil
		}
	}
}
func hciCommand(ctx context.Context, io *controllerIO, opcode uint16, data []byte) ([]byte, error) {
	p := append([]byte{1, byte(opcode), byte(opcode >> 8), byte(len(data))}, data...)
	if err := io.send(p); err != nil {
		return nil, err
	}
	for {
		p, err := io.read(ctx)
		if err != nil {
			return nil, err
		}
		if len(p) < 3 || p[0] != 4 || int(p[2]) != len(p)-3 {
			return nil, fmt.Errorf("invalid HCI response")
		}
		if p[1] == 14 && len(p) >= 7 && binary.LittleEndian.Uint16(p[4:6]) == opcode {
			if p[6] != 0 {
				return nil, fmt.Errorf("HCI 0x%04x rejected: status 0x%02x", opcode, p[6])
			}
			return p[7:], nil
		}
		if p[1] == 15 && len(p) >= 7 && binary.LittleEndian.Uint16(p[5:7]) == opcode {
			if p[3] != 0 {
				return nil, fmt.Errorf("HCI 0x%04x rejected: status 0x%02x", opcode, p[3])
			}
			return nil, nil
		}
	}
}

const codedPHYMask = uint32(1<<13 | 1<<14)

var errCodedPHYUnavailable = errors.New("controller does not expose configurable Coded TX/RX PHYs")

func CodedPHYAvailable(ctx context.Context, adapter string) error {
	return withController(ctx, adapter, true, func(ctx context.Context, io *controllerIO) error {
		index, _ := adapterIndex(adapter)
		data, err := managementCommand(ctx, io, index, 0x44, nil)
		if err != nil {
			return err
		}
		if len(data) != 12 {
			return fmt.Errorf("invalid PHY capability response")
		}
		if binary.LittleEndian.Uint32(data)&binary.LittleEndian.Uint32(data[4:])&codedPHYMask != codedPHYMask {
			return errCodedPHYUnavailable
		}
		return nil
	})
}
func ApplyMeshPHY(ctx context.Context, adapter, mode string) error {
	if err := ValidateMeshMode(mode); err != nil {
		return err
	}
	return withController(ctx, adapter, true, func(ctx context.Context, io *controllerIO) error {
		index, _ := adapterIndex(adapter)
		data, err := managementCommand(ctx, io, index, 0x44, nil)
		if err != nil {
			// Older kernels have no Get PHY command. Normal operation must not
			// require Coded controls, while explicit Coded requests stay strict.
			if rejected, ok := err.(*managementError); ok && rejected.status == 0x01 && mode == "normal" {
				return nil
			}
			return err
		}
		if len(data) != 12 {
			return fmt.Errorf("invalid PHY capability response")
		}
		supported := binary.LittleEndian.Uint32(data)
		configurable := binary.LittleEndian.Uint32(data[4:])
		selected := binary.LittleEndian.Uint32(data[8:])
		next := selected
		if mode == "normal" {
			next &^= codedPHYMask
		} else {
			if supported&configurable&codedPHYMask != codedPHYMask {
				return errCodedPHYUnavailable
			}
			next |= codedPHYMask
		}
		if next != selected {
			b := make([]byte, 4)
			binary.LittleEndian.PutUint32(b, next)
			if _, err = managementCommand(ctx, io, index, 0x45, b); err != nil {
				return err
			}
		}
		check, err := managementCommand(ctx, io, index, 0x44, nil)
		if err != nil {
			return err
		}
		if len(check) != 12 || binary.LittleEndian.Uint32(check[8:]) != next {
			return fmt.Errorf("controller did not retain selected PHYs")
		}
		return nil
	})
}
func TuneMeshLink(ctx context.Context, adapter string, handle uint16) error {
	return withController(ctx, adapter, false, func(ctx context.Context, io *controllerIO) error {
		// Read intent only after controller admission. A profile change may
		// have completed while this new link was queued behind another command.
		mode, err := MeshMode()
		if err != nil {
			return err
		}
		phy := byte(1)
		options := uint16(0)
		if mode != "normal" {
			phy = 4
			options = 1
			if mode == "coded-s8" {
				options = 2
			}
		}
		p := []byte{byte(handle), byte(handle >> 8), 0, phy, phy, byte(options), byte(options >> 8)}
		if _, err := hciCommand(ctx, io, 0x2032, p); err != nil {
			return err
		}
		// An accepted Set PHY is asynchronous. Require observed TX/RX PHY, rather
		// than claiming success merely from Command Status. HCI does not report S2/S8.
		want := byte(1)
		if phy == 4 {
			want = 3
		}
		for {
			result, err := hciCommand(ctx, io, 0x2030, []byte{byte(handle), byte(handle >> 8)})
			if err != nil {
				return err
			}
			if len(result) != 4 || binary.LittleEndian.Uint16(result) != handle {
				return fmt.Errorf("invalid PHY readback")
			}
			if result[2] == want && result[3] == want {
				return nil
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("selected PHY not observed: %w", ctx.Err())
			case <-timer.C:
			}
		}
	})
}

var linksMu sync.Mutex
var meshLinks = map[string]map[uint16]int{}

func TrackMeshLink(adapter string, handle uint16) func() {
	linksMu.Lock()
	if meshLinks[adapter] == nil {
		meshLinks[adapter] = map[uint16]int{}
	}
	meshLinks[adapter][handle]++
	linksMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			linksMu.Lock()
			defer linksMu.Unlock()
			meshLinks[adapter][handle]--
			if meshLinks[adapter][handle] == 0 {
				delete(meshLinks[adapter], handle)
			}
		})
	}
}

// TrackAndTuneMeshLink admits a new CoC into profile reconciliation. Tracking
// first ensures a concurrent profile update either includes this handle or is
// observed here. An explicitly selected normal profile also reconciles a CoC
// established on Coded before the update's tracked-link pass.
func TrackAndTuneMeshLink(ctx context.Context, adapter string, handle uint16) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	untrack := TrackMeshLink(adapter, handle)
	if err := tuneAdmittedMeshLink(ctx, adapter, handle); err != nil {
		untrack()
		return nil, err
	}
	return untrack, nil
}

func tuneAdmittedMeshLink(ctx context.Context, adapter string, handle uint16) error {
	mode, err := MeshMode()
	if err != nil {
		return err
	}
	if mode == "normal" {
		// Fresh default operation must not require PHY controls. Explicit normal
		// reconciles capable controllers, while legacy/no-Coded adapters retain
		// their existing operation without submitting unsupported HCI commands.
		if !MeshModeConfigured() {
			return nil
		}
		if err := CodedPHYAvailable(ctx, adapter); err != nil {
			var rejected *managementError
			if errors.Is(err, errCodedPHYUnavailable) ||
				(errors.As(err, &rejected) && rejected.status == 0x01) {
				return nil
			}
			return err
		}
	}
	return TuneMeshLink(ctx, adapter, handle)
}

func ApplyTrackedMeshPHY(ctx context.Context, adapter string) error {
	// Hold the registration through submission so an unregistered owner is not tuned.
	linksMu.Lock()
	defer linksMu.Unlock()
	for handle := range meshLinks[adapter] {
		if err := TuneMeshLink(ctx, adapter, handle); err != nil {
			return err
		}
	}
	return nil
}
func LinkStatus(ctx context.Context, adapter string) []*pb.BluetoothLinkRadioStatus {
	linksMu.Lock()
	defer linksMu.Unlock()
	out := make([]*pb.BluetoothLinkRadioStatus, 0, len(meshLinks[adapter]))
	for handle := range meshLinks[adapter] {
		s := &pb.BluetoothLinkRadioStatus{Handle: uint32(handle)}
		err := withController(ctx, adapter, false, func(ctx context.Context, io *controllerIO) error {
			p := []byte{byte(handle), byte(handle >> 8)}
			data, err := hciCommand(ctx, io, 0x2030, p)
			if err != nil {
				return err
			}
			if len(data) != 4 || binary.LittleEndian.Uint16(data) != handle {
				return fmt.Errorf("invalid PHY readback")
			}
			s.TxPhy = uint32(data[2])
			s.RxPhy = uint32(data[3])
			powerPHY := byte(s.TxPhy)
			if powerPHY == 3 {
				if mode, _ := MeshMode(); mode == "coded-s2" {
					powerPHY = 4
				}
			}
			// The enhanced command reports local current/max power per PHY.
			// Some Intel firmware returns reserved values for legacy Read TX Power.
			data, err = hciCommand(ctx, io, 0x2076, append(p, powerPHY))
			if err == nil {
				if len(data) != 5 || binary.LittleEndian.Uint16(data) != handle || data[2] != powerPHY {
					return fmt.Errorf("invalid enhanced power readback")
				}
				s.CurrentDbm = knownPower(data[3])
				s.MaximumDbm = knownPower(data[4])
				return nil
			}
			for kind := byte(0); kind <= 1; kind++ {
				data, err = hciCommand(ctx, io, 0x0c2d, append(p, kind))
				if err != nil {
					return err
				}
				if len(data) != 3 || binary.LittleEndian.Uint16(data) != handle {
					return fmt.Errorf("invalid power readback")
				}
				if kind == 0 {
					s.CurrentDbm = knownPower(data[2])
				} else {
					s.MaximumDbm = knownPower(data[2])
				}
			}
			return nil
		})
		if err != nil {
			s.Error = err.Error()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

// HCI reserves values outside [-127,+20]; never present them as measurements.
func knownPower(raw byte) *int32 {
	value := int32(int8(raw))
	if value < -127 || value > 20 {
		return nil
	}
	return &value
}
