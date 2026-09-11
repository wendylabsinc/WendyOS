package liteclient

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/seriallock"
	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"go.bug.st/serial"
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

type DeviceIdentity struct {
	ID          string
	Name        string
	DisplayName string
}

type DeviceInfo struct {
	OS               string
	OSVersion        string
	CPUArchitecture  string
	Board            string
	WasmAppSupport   bool
	NativeAppSupport bool
}

// ErrSerialPortUnavailable marks failures that happened before a serial port
// was successfully opened. Callers that probe for Wendy Lite firmware use it
// to distinguish a busy/missing/inaccessible port from a port that opened but
// failed the WendyCom handshake.
var ErrSerialPortUnavailable = errors.New("serial port unavailable")

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

// WendyLiteClient drives one connection to one device: connect once, then
// Close is terminal — create a new client to reconnect.
type WendyLiteClient struct {
	link                wcomLink
	serialLock          *seriallock.Lock
	requestIdGen        atomic.Uint32
	eventIdGen          atomic.Uint32
	peerProtocolVersion protocolVersion

	closeOnce sync.Once
	closeErr  error
	readDone  sync.WaitGroup // tracks the readLoop goroutine

	mu      sync.Mutex // guards subs and readErr
	subs    []*subscription
	readErr error // set once the read loop dies; refuses new subscriptions
}

func NewWendyLiteClient() *WendyLiteClient {
	return &WendyLiteClient{}
}

func (c *WendyLiteClient) ConnectInsecure(address string) error {
	conn, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec — device uses self-signed certs
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	c.link = newDirectLink(conn)
	err = c.handshake()
	if err != nil {
		conn.Close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.startReadLoop()
	return nil
}

func (c *WendyLiteClient) ConnectWithMutualAuthentication(address string, cert tls.Certificate, rootCAs x509.CertPool) error {
	// Verify the certificate chain against our root CAs but skip hostname
	// checking — devices on a local network don't have SANs.
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, len(rawCerts))
			for i, raw := range rawCerts {
				c, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parsing server certificate: %w", err)
				}
				certs[i] = c
			}
			opts := x509.VerifyOptions{
				Roots:         &rootCAs,
				Intermediates: x509.NewCertPool(),
			}
			for _, c := range certs[1:] {
				opts.Intermediates.AddCert(c)
			}
			if _, err := certs[0].Verify(opts); err != nil {
				return fmt.Errorf("server certificate verification failed: %w", err)
			}
			return nil
		},
	}
	conn, err := tls.Dial("tcp", address, tlsCfg)
	if err != nil {
		return fmt.Errorf("connect (mTLS): %w", err)
	}
	c.link = newDirectLink(conn)
	err = c.handshake()
	if err != nil {
		conn.Close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.startReadLoop()
	return nil
}

func (c *WendyLiteClient) ConnectToSerial(device string) error {
	lock, err := seriallock.Acquire(device)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSerialPortUnavailable, err)
	}
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(device, mode)
	if err != nil {
		lock.Release()
		return fmt.Errorf("%w: open serial: %w", ErrSerialPortUnavailable, err)
	}
	c.link = newSerialLink(port)
	c.serialLock = lock
	if err := c.handshake(); err != nil {
		port.Close()
		lock.Release()
		c.link = nil
		c.serialLock = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.startReadLoop()
	return nil
}

// ConnectViaCloudInsecure reaches a device through a cloud tunnel-broker
// server (dev server: self-signed cert, verification skipped). The WendyCom
// handshake runs end-to-end through the broker to the device identified by
// assetID.
// SECURITY: This should be used in development tools only. Warn if it's not
// the case.
func (c *WendyLiteClient) ConnectViaCloudInsecure(serverAddr string, assetID uint32) error {
	link, err := dialTunnelLinkInsecure(serverAddr, assetID)
	if err != nil {
		return err
	}
	c.link = link
	if err := c.handshake(); err != nil {
		link.close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.startReadLoop()
	return nil
}

func (c *WendyLiteClient) Close() error {
	if c.link == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.link.close()
		c.readDone.Wait()
		c.serialLock.Release()
	})
	return c.closeErr
}

func (c *WendyLiteClient) Ping() error {
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
func (c *WendyLiteClient) ResetTargetDevice(appAutoStart bool, delay time.Duration) error {
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

func (c *WendyLiteClient) PushApp(path string, appType AppType, onProgress func(written, total uint32)) error {
	var pbAppType wendypb.WendyComAppType
	switch appType {
	case AppTypeWasm:
		pbAppType = wendypb.WendyComAppType_WENDY_COM_APP_TYPE_WASM
	case AppTypeNative:
		pbAppType = wendypb.WendyComAppType_WENDY_COM_APP_TYPE_NATIVE
	default:
		return fmt.Errorf("unknown app type %d", appType)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() > math.MaxUint32 {
		return fmt.Errorf("app file too large: %d bytes exceeds 4 GiB limit", info.Size())
	}
	size := uint32(info.Size())

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
func (c *WendyLiteClient) PushConf(conf *wendypb.WendyConf, mode ConfPushMode, onProgress func(written, total uint32)) error {
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

func (c *WendyLiteClient) StopApp() error {
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

func (c *WendyLiteClient) StartApp() error {
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

func (c *WendyLiteClient) GetDeviceIdentity(timeout time.Duration) (*DeviceIdentity, error) {
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

func (c *WendyLiteClient) GetDeviceInfo(timeout time.Duration) (*DeviceInfo, error) {
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
		Board:            di.GetBoard(),
		WasmAppSupport:   di.GetWasmAppSupport(),
		NativeAppSupport: di.GetNativeAppSupport(),
	}, nil
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
func (c *WendyLiteClient) ConsoleAttach(rollingMode bool, blockingMode bool) (<-chan ConsoleChunk, func(abrupt bool) error, error) {
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

func (c *WendyLiteClient) sendConsoleAttach(eventID uint32, duration time.Duration, blocking bool) error {
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

func (c *WendyLiteClient) sendConsoleDetach(eventID uint32, abrupt bool) error {
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
func (c *WendyLiteClient) ConsolePushStdinData(data []byte) error {
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
func (c *WendyLiteClient) roundTrip(req *wendypb.WendyComMessage, timeout time.Duration) (*wendypb.WendyComMessage, error) {
	if err := c.link.send(req); err != nil {
		return nil, err
	}
	return c.link.recv(timeout)
}

// subscribe registers a waiter with the read loop. Messages matching filter
// are delivered to the returned subscription's channel until unsubscribe.
// Subscribers are notified in reverse order of subscription: the most recent
// subscriber receives messages first.
func (c *WendyLiteClient) subscribe(filter func(*wendypb.WendyComMessage) bool, capacity int) (*subscription, error) {
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
func (c *WendyLiteClient) unsubscribe(s *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.subs)
	c.subs = slices.DeleteFunc(c.subs, func(x *subscription) bool { return x == s })
	if len(c.subs) < n {
		close(s.ch)
	}
}

// startReadLoop hands c.link to the read loop goroutine and registers it with
// readDone so Close can wait for it to exit.
func (c *WendyLiteClient) startReadLoop() {
	c.readDone.Add(1)
	go c.readLoop(c.link)
}

// readLoop receives every message from the device and hands it to the first
// subscriber whose filter matches. It runs from the end of the handshake
// until the connection dies. The link is a parameter rather than read from
// c.link so the loop cannot race with Close mutating the client.
func (c *WendyLiteClient) readLoop(link wcomLink) {
	defer c.readDone.Done()
	for {
		msg, err := link.recv(0)
		if err != nil {
			c.failAll(err)
			return
		}
		c.dispatch(msg)
	}
}

func (c *WendyLiteClient) dispatch(msg *wendypb.WendyComMessage) {
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
func (c *WendyLiteClient) failAll(err error) {
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
func (c *WendyLiteClient) sendCommand(cmd *wendypb.WendyComCommand, timeout time.Duration) (*wendypb.WendyComResponse, error) {
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

func (c *WendyLiteClient) handshake() error {
	if err := c.link.linkHandshake(); err != nil {
		return err
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("handshake id: %w", err)
	}
	id := binary.BigEndian.Uint32(b[:])
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
		return fmt.Errorf("device may not support protocol v%d: %w", versionMajor, err)
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
