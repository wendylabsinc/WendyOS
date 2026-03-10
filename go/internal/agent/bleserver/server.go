//go:build linux

package bleserver

import (
	"context"
	"errors"
	"net"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/internal/agent/services"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

const (
	// l2capPSM is the L2CAP Protocol/Service Multiplexer used for BLE communication.
	l2capPSM = 128

	// commandTimeout is the read deadline for a single command from the BLE client.
	commandTimeout = 30 * time.Second
)

// Server manages BLE advertising and L2CAP command handling.
type Server struct {
	logger     *zap.Logger
	advertiser *Advertiser
	dispatcher *Dispatcher
}

// NewServer creates a new BLE server with the given service dependencies.
func NewServer(
	logger *zap.Logger,
	network services.NetworkManager,
	hardware services.HardwareDiscoverer,
	bluetooth services.BluetoothManager,
	container services.ContainerdClient,
) *Server {
	return &Server{
		logger:     logger.Named("ble-server"),
		advertiser: NewAdvertiser(logger.Named("ble-adv")),
		dispatcher: NewDispatcher(logger.Named("ble-dispatch"), network, hardware, bluetooth, container),
	}
}

// Run starts BLE advertising and the L2CAP accept loop. It blocks until
// ctx is cancelled, then cleans up advertising and closes the listener.
func (s *Server) Run(ctx context.Context) {
	// Start advertising (best-effort; log and continue if bluetoothctl unavailable).
	if err := s.advertiser.Start(); err != nil {
		s.logger.Warn("BLE advertising unavailable, BLE server will not be discoverable", zap.Error(err))
	}
	defer s.advertiser.Stop()

	listener, err := newL2CAPListener(l2capPSM)
	if err != nil {
		s.logger.Error("Failed to create L2CAP listener, BLE server disabled", zap.Error(err))
		return
	}
	defer listener.close()

	// Set a 1-second receive timeout so the accept loop checks ctx.Done() periodically.
	if err := listener.setRecvTimeout(unix.Timeval{Sec: 1}); err != nil {
		s.logger.Warn("Failed to set SO_RCVTIMEO on L2CAP socket", zap.Error(err))
	}

	s.logger.Info("BLE L2CAP server listening", zap.Uint16("psm", l2capPSM))

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("BLE server shutting down")
			return
		default:
		}

		conn, err := listener.accept()
		if err != nil {
			// EAGAIN/EWOULDBLOCK means the recv timeout expired.
			// EINTR means a signal interrupted the syscall.
			// Both are transient — just retry.
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("L2CAP accept error", zap.Error(err))
			continue
		}

		s.logger.Info("BLE client connected")
		s.handleConnection(ctx, conn)
		s.logger.Info("BLE client disconnected")
	}
}

// handleConnection processes commands from a single BLE client connection.
func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := conn.SetReadDeadline(time.Now().Add(commandTimeout)); err != nil {
			s.logger.Warn("Failed to set read deadline", zap.Error(err))
		}

		data, err := readMessage(conn)
		if err != nil {
			if !isTimeout(err) {
				s.logger.Debug("BLE read error (client likely disconnected)", zap.Error(err))
			}
			return
		}

		cmd := &agentpb.BluetoothCommand{}
		if err := proto.Unmarshal(data, cmd); err != nil {
			s.logger.Warn("Failed to unmarshal BLE command", zap.Error(err))
			resp := errorResponse("invalid command: " + err.Error())
			s.sendResponse(conn, resp)
			continue
		}

		s.logger.Debug("BLE command received", zap.String("type", describeCommand(cmd)))

		resp := s.dispatcher.Dispatch(ctx, cmd)
		if err := s.sendResponse(conn, resp); err != nil {
			s.logger.Debug("BLE write error", zap.Error(err))
			return
		}
	}
}

// sendResponse marshals and sends a BluetoothResponse over the connection.
func (s *Server) sendResponse(conn net.Conn, resp *agentpb.BluetoothResponse) error {
	data, err := proto.Marshal(resp)
	if err != nil {
		s.logger.Error("Failed to marshal BLE response", zap.Error(err))
		return err
	}
	return writeMessage(conn, data)
}

// isTimeout checks if an error is a timeout error.
func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

// describeCommand returns a human-readable name for a BluetoothCommand.
func describeCommand(cmd *agentpb.BluetoothCommand) string {
	switch cmd.GetCommand().(type) {
	case *agentpb.BluetoothCommand_WifiList:
		return "WifiList"
	case *agentpb.BluetoothCommand_WifiConnect:
		return "WifiConnect"
	case *agentpb.BluetoothCommand_WifiStatus:
		return "WifiStatus"
	case *agentpb.BluetoothCommand_WifiDisconnect:
		return "WifiDisconnect"
	case *agentpb.BluetoothCommand_AppsList:
		return "AppsList"
	case *agentpb.BluetoothCommand_AppsStop:
		return "AppsStop"
	case *agentpb.BluetoothCommand_AppsRemove:
		return "AppsRemove"
	case *agentpb.BluetoothCommand_AgentVersion:
		return "AgentVersion"
	case *agentpb.BluetoothCommand_HardwareList:
		return "HardwareList"
	case *agentpb.BluetoothCommand_BluetoothList:
		return "BluetoothList"
	case *agentpb.BluetoothCommand_BluetoothConnect:
		return "BluetoothConnect"
	case *agentpb.BluetoothCommand_BluetoothDisconnect:
		return "BluetoothDisconnect"
	case *agentpb.BluetoothCommand_BluetoothForget:
		return "BluetoothForget"
	default:
		return "unknown"
	}
}
