package commands

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// The pinned Ollama image includes bash and coreutils, but not curl or socat.
// Send exactly one HTTP request, then replace the shell with the response
// reader. There are no background processes left behind when exec is canceled.
// Only port and byte count appear in argv; prompts travel over stdin.
const desktopModelHTTPCommand = `exec 3<>/dev/tcp/127.0.0.1/"$1" || exit
head -c "$2" >&3 || exit
exec cat <&3`

// DesktopModelRoundTrip reaches a managed Ollama runtime through the selected
// Agent's authenticated exec stream on LAN or when Cloud lacks TCP port services.
func DesktopModelRoundTrip(req *http.Request, conn *grpcclient.AgentConnection, port uint32, headerTimeout time.Duration) (*http.Response, error) {
	if port == 0 || port > 65535 || req.URL.Scheme != "http" || req.URL.Host != "wendy-device" {
		return nil, fmt.Errorf("model requests must stay on the selected device")
	}
	ports, err := DesktopModelPorts(req.Context(), conn)
	if err != nil {
		return nil, fmt.Errorf("checking model runtime: %w", err)
	}
	if !slices.Contains(ports, port) {
		return nil, fmt.Errorf("no running Wendy-managed Ollama runtime on port %d", port)
	}
	return desktopModelRoundTrip(req, conn, port, headerTimeout)
}

func desktopModelRoundTrip(req *http.Request, conn *grpcclient.AgentConnection, port uint32, headerTimeout time.Duration) (*http.Response, error) {
	request := req.Clone(req.Context())
	// Ollama's loopback listener validates Host. The synthetic routing name
	// belongs to the desktop transport, not the device's HTTP server.
	request.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	request.Close = true
	var wire bytes.Buffer
	if err := request.Write(&wire); err != nil {
		return nil, err
	}
	stream, err := openContainerConnection(req.Context(), conn.ContainerService,
		fmt.Sprintf("sh.wendy.models.ollama.%d", port),
		[]string{"bash", "-c", desktopModelHTTPCommand, "wendy-model-http", strconv.Itoa(int(port)), strconv.Itoa(wire.Len())}, "model connection")
	if err != nil {
		return nil, err
	}
	// Bound the response-header wait just like the direct HTTP transport. The
	// request context and response Body.Close own cancellation after headers.
	var timer *time.Timer
	if headerTimeout > 0 {
		timer = time.AfterFunc(headerTimeout, func() { _ = stream.Close() })
		defer timer.Stop()
	}
	if _, err := io.Copy(stream, &wire); err != nil {
		_ = stream.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(stream), request)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	response.Body = &modelResponseBody{ReadCloser: response.Body, conn: stream}
	return response, nil
}

type modelResponseBody struct {
	io.ReadCloser
	conn net.Conn
}

func (b *modelResponseBody) Close() error {
	// Close the stream first: an HTTP body close may otherwise drain a still
	// generating response before allowing Stop to cancel the remote request.
	_ = b.conn.Close()
	return b.ReadCloser.Close()
}
