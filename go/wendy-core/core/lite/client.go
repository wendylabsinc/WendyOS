// Package lite implements portable WendyCom operations over a host-owned link.
package lite

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wendypb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

// The WendyCom protocol version this client speaks, exchanged in the handshake.
// The major number must match the device's; minor is informational.
const (
	versionMajor = 2
	versionMinor = 0
)

type protocolVersion struct {
	Major uint32
	Minor uint32
}

// DeviceIdentity is the firmware-reported device identifier and names.
type DeviceIdentity struct {
	ID          string
	Name        string
	DisplayName string
}

// DeviceInfo reports the firmware and supported application formats.
type DeviceInfo struct {
	OS              string
	OSVersion       string
	CPUArchitecture string
	// Target is the SoC name, e.g. "esp32c6".
	Target string
	// Board identifies the board, e.g. "esp32s3_seeed_xiao_native". Empty when
	// the device does not know which board it runs on.
	Board            string
	WasmAppSupport   bool
	NativeAppSupport bool
}

// AppType selects a WASM or native application image.
type AppType int

// ConfPushMode selects how a pushed configuration combines with the one
// stored on the device: replace it entirely, or update it keeping the root
// fields the pushed configuration does not provide.
type ConfPushMode int

const (
	ConfPushModeReplace ConfPushMode = iota
	ConfPushModeUpdate
)

// ConsoleChunk is one piece of console output streamed by the device.
type ConsoleChunk struct {
	Gap    bool // data was lost before this chunk
	Stderr bool
	Data   []byte
}

const (
	AppTypeWasm AppType = iota
	AppTypeNative

	// consoleLease is the attachment duration requested in rolling mode;
	// consoleRenew re-arms it well before it expires.
	consoleLease = 20 * time.Second
	consoleRenew = 10 * time.Second
)

// subscription is one waiter registered with the read loop. The read loop
// delivers every message matching filter to ch. The channel capacity is a
// throughput smoother, not a correctness mechanism: dispatch blocks on a full
// channel, so a subscriber must keep draining until unsubscribed.
type subscription struct {
	filter func(*wendypb.WendyComMessage) bool
	ch     chan *wendypb.WendyComMessage
}

// sensorDataListener boxes a listener so registrations have an identity:
// func values are not comparable, so the slice could not be pruned otherwise.
type sensorDataListener struct {
	fn func(*wendypb.SensorData)
}

// Client drives one connection to one device: connect once, then
// Close is terminal — create a new client to reconnect.
type Client struct {
	entropy             platform.Entropy
	link                wcomLink
	requestIdGen        atomic.Uint32
	eventIdGen          atomic.Uint32
	peerProtocolVersion protocolVersion

	closeOnce sync.Once
	closeErr  error
	readDone  sync.WaitGroup // tracks the readLoop goroutine
	done      chan struct{}  // closed once the read loop exits; see Done

	mu      sync.Mutex // guards subs and readErr
	subs    []*subscription
	readErr error // set once the read loop dies; refuses new subscriptions

	// Sensor data is delivered straight from the read loop, so its listeners
	// must not wait on mu. The stored slice is never mutated once published:
	// dispatch loads and ranges over it lock-free, and registration swaps in
	// a fresh copy under listenerMu.
	listenerMu          sync.Mutex
	sensorDataListeners atomic.Pointer[[]*sensorDataListener]
}

// NewClient creates one client; the host supplies secure handshake entropy.
func NewClient(entropy platform.Entropy) *Client {
	return &Client{done: make(chan struct{}), entropy: entropy}
}

// Done returns a channel that is closed once the connection is gone: lost, or
// torn down by Close. It lets a caller that only listens — sensor data reaches
// their listeners with no end-of-stream marker — notice the device dropping
// off. It never closes on a client that never connected.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// Close terminates the host link and waits for dispatch to stop.
func (c *Client) Close() error {
	if c.link == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.link.close()
		c.readDone.Wait()
	})
	return c.closeErr
}

func (c *Client) Ping() error {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_Ping{
			Ping: &wendypb.WendyComPingParams{},
		},
	}, 0)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

// ResetTargetDevice reboots the device. appAutoStart=false keeps the app
// stopped after the reboot; delay postpones the auto-start (an AppStart
// command cuts the delay short). Both settings apply to the next boot only.
func (c *Client) ResetTargetDevice(appAutoStart bool, delay time.Duration) error {
	if delay < 0 || delay/time.Millisecond > math.MaxUint32 {
		return fmt.Errorf("delay %v out of range", delay)
	}
	// The device reboots on receipt, so no ack is expected.
	return c.link.send(&wendypb.WendyComMessage{
		Msg: &wendypb.WendyComMessage_Command{Command: &wendypb.WendyComCommand{
			RequestId: c.requestIdGen.Add(1),
			Params: &wendypb.WendyComCommand_Reboot{
				Reboot: &wendypb.WendyComRebootParams{
					AppAutoStart:      proto.Bool(appAutoStart),
					AppAutoStartDelay: uint32(delay / time.Millisecond),
				},
			},
		}},
	})
}

// PushApp streams a host-opened application and reports acknowledged progress.
func (c *Client) PushApp(f io.Reader, fileSize int64, appType AppType, onProgress func(written, total uint32)) error {
	var pbAppType wendypb.WendyComAppType
	switch appType {
	case AppTypeWasm:
		pbAppType = wendypb.WendyComAppType_WENDY_COM_APP_TYPE_WASM
	case AppTypeNative:
		pbAppType = wendypb.WendyComAppType_WENDY_COM_APP_TYPE_NATIVE
	default:
		return fmt.Errorf("unknown app type %d", appType)
	}

	if fileSize < 0 || fileSize > math.MaxUint32 {
		return fmt.Errorf("app file too large: %d bytes exceeds 4 GiB limit", fileSize)
	}
	size := uint32(fileSize)

	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_AppPushBegin{
			AppPushBegin: &wendypb.WendyComAppPushBeginParams{Size: size, AppType: pbAppType},
		},
	}, 0)
	if err != nil {
		return fmt.Errorf("push begin: %w", err)
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("push begin: device returned error: %w", err)
	}

	buf := make([]byte, c.link.preferredChunkSize())
	var offset uint32
	for {
		n, err := f.Read(buf)
		if n > 0 {
			resp, serr := c.sendCommand(&wendypb.WendyComCommand{
				RequestId: c.requestIdGen.Add(1),
				Params: &wendypb.WendyComCommand_AppPushData{
					AppPushData: &wendypb.WendyComAppPushDataParams{
						Offset: offset,
						Data:   buf[:n],
					},
				},
			}, 0)
			if serr != nil {
				return fmt.Errorf("push data at offset %d: %w", offset, serr)
			}
			if err := resultToError(resp.Result); err != nil {
				return fmt.Errorf("push data at offset %d: device returned error: %w", offset, err)
			}
			offset += uint32(n)
			if onProgress != nil {
				onProgress(offset, size)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}

	resp, err = c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_AppPushEnd{
			AppPushEnd: &wendypb.WendyComAppPushEndParams{},
		},
	}, 0)
	if err != nil {
		return fmt.Errorf("push end: %w", err)
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("push end: device returned error: %w", err)
	}
	return nil
}

// PushConf writes a configuration to the device's wendy_conf partition. The
// new configuration takes effect after reboot.
func (c *Client) PushConf(conf *wendypb.WendyConf, mode ConfPushMode, onProgress func(written, total uint32)) error {
	var pbMode wendypb.WendyComConfPushMode
	switch mode {
	case ConfPushModeReplace:
		pbMode = wendypb.WendyComConfPushMode_WENDY_COM_CONF_PUSH_MODE_REPLACE
	case ConfPushModeUpdate:
		pbMode = wendypb.WendyComConfPushMode_WENDY_COM_CONF_PUSH_MODE_UPDATE
	default:
		return fmt.Errorf("unknown conf push mode %d", mode)
	}

	blob, err := proto.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal conf: %w", err)
	}
	if len(blob) > math.MaxUint32 {
		return fmt.Errorf("conf too large: %d bytes exceeds 4 GiB limit", len(blob))
	}
	size := uint32(len(blob))

	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_ConfPushBegin{
			ConfPushBegin: &wendypb.WendyComConfPushBeginParams{Size: size, Mode: pbMode},
		},
	}, 0)
	if err != nil {
		return fmt.Errorf("push begin: %w", err)
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("push begin: device returned error: %w", err)
	}

	chunk := c.link.preferredChunkSize()
	for offset := uint32(0); offset < size; {
		n := min(size-offset, uint32(chunk))
		resp, err := c.sendCommand(&wendypb.WendyComCommand{
			RequestId: c.requestIdGen.Add(1),
			Params: &wendypb.WendyComCommand_ConfPushData{
				ConfPushData: &wendypb.WendyComConfPushDataParams{
					Offset: offset,
					Data:   blob[offset : offset+n],
				},
			},
		}, 0)
		if err != nil {
			return fmt.Errorf("push data at offset %d: %w", offset, err)
		}
		if err := resultToError(resp.Result); err != nil {
			return fmt.Errorf("push data at offset %d: device returned error: %w", offset, err)
		}
		offset += n
		if onProgress != nil {
			onProgress(offset, size)
		}
	}

	resp, err = c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_ConfPushEnd{
			ConfPushEnd: &wendypb.WendyComConfPushEndParams{},
		},
	}, 0)
	if err != nil {
		return fmt.Errorf("push end: %w", err)
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("push end: device returned error: %w", err)
	}
	return nil
}

func (c *Client) StopApp() error {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_AppStop{
			AppStop: &wendypb.WendyComAppStopParams{},
		},
	}, 0)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

func (c *Client) StartApp() error {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_AppStart{
			AppStart: &wendypb.WendyComAppStartParams{},
		},
	}, 0)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

func (c *Client) GetDeviceIdentity(timeout time.Duration) (*DeviceIdentity, error) {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_GetDeviceIdentity{
			GetDeviceIdentity: &wendypb.WendyComGetDeviceIdentityParams{},
		},
	}, timeout)
	if err != nil {
		return nil, err
	}
	if err := resultToError(resp.Result); err != nil {
		return nil, fmt.Errorf("device returned error: %w", err)
	}
	di := resp.GetDeviceIdentity()
	if di == nil {
		return nil, fmt.Errorf("device returned no identity")
	}
	return &DeviceIdentity{ID: di.GetId(), Name: di.GetName(), DisplayName: di.GetDisplayName()}, nil
}

func (c *Client) GetDeviceInfo(timeout time.Duration) (*DeviceInfo, error) {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_GetDeviceInfo{
			GetDeviceInfo: &wendypb.WendyComGetDeviceInfoParams{},
		},
	}, timeout)
	if err != nil {
		return nil, err
	}
	if err := resultToError(resp.Result); err != nil {
		return nil, fmt.Errorf("device returned error: %w", err)
	}
	di := resp.GetDeviceInfo()
	if di == nil {
		return nil, fmt.Errorf("device returned no info")
	}
	return &DeviceInfo{
		OS:               di.GetOs(),
		OSVersion:        di.GetOsVersion(),
		CPUArchitecture:  di.GetCpuArchitecture(),
		Target:           di.GetTarget(),
		Board:            di.GetBoard(),
		WasmAppSupport:   di.GetWasmAppSupport(),
		NativeAppSupport: di.GetNativeAppSupport(),
	}, nil
}

// GetSensorManifest asks the device for its sensor-link manifest. The
// returned type is the generated wendypb type directly, not a mirrored
// client-package struct: sensor-link is a wire contract shared across
// modules beyond this console, so there's no reason to hide it behind a
// second type.
func (c *Client) GetSensorManifest(timeout time.Duration) (*wendypb.SensorManifest, error) {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_SensorLinkGetManifest{
			SensorLinkGetManifest: &wendypb.GetSensorManifest{},
		},
	}, timeout)
	if err != nil {
		return nil, err
	}
	if err := resultToError(resp.Result); err != nil {
		return nil, fmt.Errorf("device returned error: %w", err)
	}
	m := resp.GetSensorLinkManifest()
	if m == nil {
		return nil, fmt.Errorf("device returned no manifest")
	}
	return m, nil
}

// SensorLinkSubscribe asks the device to start streaming SensorData for
// the given channel IDs.
func (c *Client) SensorLinkSubscribe(channelIDs []uint32, timeout time.Duration) error {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_SensorLinkSubscribe{
			SensorLinkSubscribe: &wendypb.Subscribe{ChannelId: channelIDs},
		},
	}, timeout)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

// SensorLinkUnsubscribe asks the device to stop streaming SensorData for
// the given channel IDs.
func (c *Client) SensorLinkUnsubscribe(channelIDs []uint32, timeout time.Duration) error {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_SensorLinkUnsubscribe{
			SensorLinkUnsubscribe: &wendypb.Unsubscribe{ChannelId: channelIDs},
		},
	}, timeout)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

// ConsoleAttach asks the device to stream its console output and returns the
// chunk channel plus an idempotent detach function. The channel is closed on
// detach and on connection loss. detach stops local delivery, then tells the
// device to stop streaming and returns its result. With abrupt the detach
// command is only sent, without waiting for the device's acknowledgment —
// for teardown paths where waiting could hang (device unresponsive, network
// loss) and the caller closes the connection right after.
//
// With rollingMode the attachment is a lease: it is requested for
// consoleLease and silently renewed every consoleRenew, so the device
// detaches by itself within consoleLease if this client dies without
// detaching. Without rollingMode the device stays attached until detach.
//
// With blockingMode the device captures losslessly: writers on the device
// block when the capture buffer is full, until it is drained. Without
// blockingMode the oldest buffered data is dropped instead and the loss is
// reported as a gap.
func (c *Client) ConsoleAttach(rollingMode bool, blockingMode bool) (<-chan ConsoleChunk, func(abrupt bool) error, error) {
	eventID := c.eventIdGen.Add(1)

	// Subscribe before attaching so no chunk is lost.
	sub, err := c.subscribe(func(m *wendypb.WendyComMessage) bool {
		e := m.GetEvent()
		return e != nil && e.GetEventId() == eventID && e.GetConsoleData() != nil
	}, 16)
	if err != nil {
		return nil, nil, err
	}

	var lease time.Duration
	if rollingMode {
		lease = consoleLease
	}
	if err := c.sendConsoleAttach(eventID, lease, blockingMode); err != nil {
		c.unsubscribe(sub)
		return nil, nil, err
	}

	out := make(chan ConsoleChunk)
	done := make(chan struct{})  // closed by detach
	ended := make(chan struct{}) // closed once sub.ch is closed (detach or connection loss)
	go func() {
		for msg := range sub.ch {
			cd := msg.GetEvent().GetConsoleData()
			chunk := ConsoleChunk{
				Gap:    cd.GetGap(),
				Stderr: cd.GetIo() == wendypb.WendyComConsoleIo_WENDY_COM_CONSOLE_IO_STANDARD_ERROR,
				Data:   cd.GetData(),
			}
			// Keep draining sub.ch after detach: a dispatch blocked on a full
			// sub.ch holds the client mutex, and detach's unsubscribe needs
			// that mutex — discarding here breaks the cycle.
			select {
			case out <- chunk:
			case <-done:
			}
		}
		close(ended)
		close(out)
	}()

	var renewer sync.WaitGroup
	if rollingMode {
		renewer.Add(1)
		go func() {
			defer renewer.Done()
			ticker := time.NewTicker(consoleRenew)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					// A failed renewal is not fatal: the next tick retries,
					// and a lost connection ends the loop via ended.
					_ = c.sendConsoleAttach(eventID, consoleLease, blockingMode)
				case <-ended:
					return
				}
			}
		}()
	}

	var once sync.Once
	var detachErr error
	detach := func(abrupt bool) error {
		once.Do(func() {
			close(done)
			c.unsubscribe(sub)
			renewer.Wait() // no renewal may land after the detach command
			detachErr = c.sendConsoleDetach(eventID, abrupt)
		})
		return detachErr
	}
	return out, detach, nil
}

func (c *Client) sendConsoleAttach(eventID uint32, duration time.Duration, blocking bool) error {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_ConsoleAttach{
			ConsoleAttach: &wendypb.WendyComConsoleAttachParams{
				EventId:  eventID,
				Duration: uint32(duration / time.Millisecond),
				Blocking: blocking,
			},
		},
	}, 0)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

func (c *Client) sendConsoleDetach(eventID uint32, abrupt bool) error {
	cmd := &wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_ConsoleDetach{
			ConsoleDetach: &wendypb.WendyComConsoleDetachParams{EventId: eventID},
		},
	}
	if abrupt {
		return c.link.send(&wendypb.WendyComMessage{
			Msg: &wendypb.WendyComMessage_Command{Command: cmd},
		})
	}
	resp, err := c.sendCommand(cmd, 0)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("device returned error: %w", err)
	}
	return nil
}

// ConsolePushStdinData injects data into the device's stdin. Fire-and-forget:
// the device sends no acknowledgment.
func (c *Client) ConsolePushStdinData(data []byte) error {
	for len(data) > 0 {
		n := min(len(data), c.link.preferredChunkSize())
		err := c.link.send(&wendypb.WendyComMessage{
			Msg: &wendypb.WendyComMessage_Event{
				Event: &wendypb.WendyComEvent{
					Data: &wendypb.WendyComEvent_ConsoleData{
						ConsoleData: &wendypb.WendyComConsoleData{
							Io:   wendypb.WendyComConsoleIo_WENDY_COM_CONSOLE_IO_STANDARD_INPUT,
							Gap:  false,
							Data: data[:n],
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func resultToError(result wendypb.WendyComResult) error {
	switch result {
	case wendypb.WendyComResult_WENDY_COM_RESULT_OK:
		return nil
	case wendypb.WendyComResult_WENDY_COM_RESULT_BAD_PROTOCOL_VERSION:
		return errors.New("bad protocol version")
	case wendypb.WendyComResult_WENDY_COM_RESULT_BAD_STATE:
		return errors.New("bad state")
	case wendypb.WendyComResult_WENDY_COM_RESULT_BUSY:
		return errors.New("device busy")
	case wendypb.WendyComResult_WENDY_COM_RESULT_BAD_APP_TYPE:
		return errors.New("bad app type")
	case wendypb.WendyComResult_WENDY_COM_RESULT_BAD_APP_SIZE:
		return errors.New("bad app size")
	case wendypb.WendyComResult_WENDY_COM_RESULT_BAD_CONF_SIZE:
		return errors.New("bad conf size")
	default: // includes WENDY_COM_RESULT_UNKNOWN_ERROR
		return fmt.Errorf("error %d", int32(result))
	}
}

// roundTrip sends a message and synchronously reads the peer's reply message.
// Only valid before readLoop is started (i.e. during the handshake); once the
// read loop owns the connection, use sendCommand/subscribe instead.
func (c *Client) roundTrip(req *wendypb.WendyComMessage, timeout time.Duration) (*wendypb.WendyComMessage, error) {
	if err := c.link.send(req); err != nil {
		return nil, err
	}
	return c.link.recv(timeout)
}

// subscribe registers a waiter with the read loop. Messages matching filter
// are delivered to the returned subscription's channel until unsubscribe.
// Subscribers are notified in reverse order of subscription: the most recent
// subscriber receives messages first.
func (c *Client) subscribe(filter func(*wendypb.WendyComMessage) bool, capacity int) (*subscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return nil, fmt.Errorf("connection lost: %w", c.readErr)
	}
	s := &subscription{filter: filter, ch: make(chan *wendypb.WendyComMessage, capacity)}
	c.subs = append(c.subs, s)
	return s, nil
}

// unsubscribe removes the subscriber and closes its channel. If failAll has
// already torn the subscription down (connection lost), the channel is already
// closed and must not be closed again — hence the channel is only closed here
// if the subscriber was actually removed.
func (c *Client) unsubscribe(s *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.subs)
	c.subs = slices.DeleteFunc(c.subs, func(x *subscription) bool { return x == s })
	if len(c.subs) < n {
		close(s.ch)
	}
}

// AddSensorDataListener registers fn for every SensorData the device sends —
// one chunk of a frame, which the caller reassembles — and returns a function
// that unregisters it, idempotently and safely after the connection has died.
//
// fn is called on the client's read loop — one chunk at a time, in arrival
// order, with no goroutine hop and no lock held. It therefore sits on the
// critical path of the whole connection: it must not block, and it must not
// call anything that waits for a device reply, since the read loop is busy
// running fn instead of reading that reply. The chunk and its payload belong
// to fn and stay valid after it returns.
func (c *Client) AddSensorDataListener(fn func(*wendypb.SensorData)) func() {
	l := &sensorDataListener{fn: fn}

	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()
	next := append(c.loadSensorDataListeners(), l)
	c.sensorDataListeners.Store(&next)

	return func() {
		c.listenerMu.Lock()
		defer c.listenerMu.Unlock()
		next := slices.DeleteFunc(c.loadSensorDataListeners(),
			func(x *sensorDataListener) bool { return x == l })
		c.sensorDataListeners.Store(&next)
	}
}

// loadSensorDataListeners returns a copy the caller may append to or prune:
// the published slice is shared with a dispatch that holds no lock, so it can
// never be modified in place. Callers hold listenerMu.
func (c *Client) loadSensorDataListeners() []*sensorDataListener {
	if listeners := c.sensorDataListeners.Load(); listeners != nil {
		return slices.Clone(*listeners)
	}
	return nil
}

// startReadLoop hands c.link to the read loop goroutine and registers it with
// readDone so Close can wait for it to exit.
func (c *Client) startReadLoop() {
	c.readDone.Add(1)
	go c.readLoop(c.link)
}

// readLoop receives every message from the device and hands it to the first
// subscriber whose filter matches. It runs from the end of the handshake
// until the connection dies. The link is a parameter rather than read from
// c.link so the loop cannot race with Close mutating the client.
func (c *Client) readLoop(link wcomLink) {
	defer c.readDone.Done()
	for {
		msg, err := link.recv(0)
		if err != nil {
			c.failAll(err)
			close(c.done) // after failAll, so readErr is set when Done fires
			return
		}
		c.dispatch(msg)
	}
}

func (c *Client) dispatch(msg *wendypb.WendyComMessage) {
	// Sensor data arrives at video rates and goes to its listeners right
	// here, on this goroutine: the subscription path below would put a mutex
	// and a blocking channel send in front of every chunk. Handling it before
	// the lock also keeps it away from the log line at the bottom, which
	// would print a whole payload per chunk.
	if data := msg.GetSensorData(); data != nil {
		if listeners := c.sensorDataListeners.Load(); listeners != nil {
			for _, l := range *listeners {
				l.fn(data)
			}
		}
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.subs) - 1; i >= 0; i-- {
		if s := c.subs[i]; s.filter(msg) {
			s.ch <- msg
			return
		}
	}
	// Events may legitimately trail their subscription (e.g. console chunks
	// after detach); logging them would dump raw payloads to stderr.
	if msg.GetEvent() != nil {
		return
	}
	log.Printf("wendycom: unhandled message from device: %v", msg)
}

// failAll marks the connection dead and wakes every pending subscriber by
// closing its channel.
func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readErr = err
	for _, s := range c.subs {
		close(s.ch)
	}
	c.subs = nil
}

// sendCommand subscribes for the matching response, sends the command, and
// waits for the response or the timeout. A timeout <= 0 means no deadline.
func (c *Client) sendCommand(cmd *wendypb.WendyComCommand, timeout time.Duration) (*wendypb.WendyComResponse, error) {
	// Subscribe before sending so a fast reply can't slip past us.
	sub, err := c.subscribe(func(m *wendypb.WendyComMessage) bool {
		r := m.GetResponse()
		return r != nil && r.RequestId == cmd.RequestId
	}, 1)
	if err != nil {
		return nil, err
	}
	defer c.unsubscribe(sub)

	if err := c.link.send(&wendypb.WendyComMessage{
		Msg: &wendypb.WendyComMessage_Command{Command: cmd},
	}); err != nil {
		return nil, err
	}

	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	select {
	case msg, ok := <-sub.ch:
		if !ok {
			return nil, fmt.Errorf("connection lost: %w", c.readErr)
		}
		return msg.GetResponse(), nil
	case <-timer:
		return nil, fmt.Errorf("timeout waiting for response to request %d", cmd.RequestId)
	}
}

func (c *Client) handshakeContext(ctx context.Context) error {
	if err := c.link.linkHandshake(); err != nil {
		return err
	}
	b, err := c.entropy.RandomBytes(ctx, 4)
	if err != nil {
		return fmt.Errorf("handshake id: %w", err)
	}
	if len(b) != 4 {
		return fmt.Errorf("handshake id: host returned invalid entropy length")
	}
	id := binary.BigEndian.Uint32(b)
	reply, err := c.roundTrip(&wendypb.WendyComMessage{
		Msg: &wendypb.WendyComMessage_Handshake{
			Handshake: &wendypb.WendyComHandshake{
				HandshakeId: id,
				Version: &wendypb.WendyComProtocolVersion{
					Major: versionMajor,
					Minor: versionMinor,
				},
			},
		},
	}, 3*time.Second)
	if err != nil {
		return fmt.Errorf("WendyCom protocol exchange: %w", err)
	}
	hs := reply.GetHandshake()
	switch {
	case hs == nil:
		return fmt.Errorf("unexpected reply message type")
	case hs.GetHandshakeId() != id:
		return fmt.Errorf("handshake ID mismatch")
	case hs.GetVersion() == nil:
		return fmt.Errorf("handshake missing version")
	case hs.GetVersion().GetMajor() != versionMajor:
		return fmt.Errorf("unsupported device protocol version %d.%d",
			hs.GetVersion().GetMajor(), hs.GetVersion().GetMinor())
	}
	ver := hs.GetVersion()
	c.peerProtocolVersion = protocolVersion{Major: ver.GetMajor(), Minor: ver.GetMinor()}
	return nil
}

// EnrollmentChallenge requests enrollment state over the active connection.
// Firmware controls which transports support bootstrap challenges.
// statusOnly observes completion without replacing the bootstrap nonce.
func (c *Client) EnrollmentChallenge(statusOnly bool) (*wendypb.WendyComEnrollmentChallenge, error) {
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_EnrollmentChallenge{
			EnrollmentChallenge: &wendypb.WendyComEnrollmentChallengeParams{StatusOnly: statusOnly},
		},
	}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := resultToError(resp.Result); err != nil {
		return nil, fmt.Errorf("firmware rejected the PKI enrollment challenge (%s): %w; the device needs PKI-enabled Wendy Lite firmware with enrollment support on the selected transport; for a simulator, use a PKI-enabled ESP32-C6 image", resp.Result.String(), err)
	}
	challenge := resp.GetEnrollmentChallenge()
	if challenge == nil {
		return nil, fmt.Errorf("device returned no enrollment challenge")
	}
	return challenge, nil
}

// ResetEnrollment clears enrollment over physical USB. Firmware validates the
// hardware ID and its current challenge, and preserves unrelated configuration.
// The caller must revoke Cloud credentials first and reboot after confirmation.
func (c *Client) ResetEnrollment(deviceID, nonceHex string) error {
	if strings.TrimSpace(deviceID) == "" {
		return fmt.Errorf("missing device ID for enrollment reset")
	}
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil || len(nonce) != 32 {
		return fmt.Errorf("invalid enrollment reset nonce")
	}
	resp, err := c.sendCommand(&wendypb.WendyComCommand{
		RequestId: c.requestIdGen.Add(1),
		Params: &wendypb.WendyComCommand_EnrollmentReset{
			EnrollmentReset: &wendypb.WendyComEnrollmentResetParams{ExpectedDeviceId: deviceID, NonceHex: nonceHex},
		},
	}, 2*time.Minute)
	if err != nil {
		return err
	}
	if err := resultToError(resp.Result); err != nil {
		return fmt.Errorf("firmware rejected enrollment reset (%s): %w", resp.Result.String(), err)
	}
	return nil
}

func (c *Client) handshake() error { return c.handshakeContext(context.Background()) }
