package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// Exercise Run and the real WendyCom client together. StopApp unloads the old
// WASM instance on firmware; replacement must instead begin with AppPushBegin.
func TestMicroWendyRunReplacesWasmWithoutStop(t *testing.T) {
	for _, rejectPush := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejectPush=%v", rejectPush), func(t *testing.T) {
			certServer := httptest.NewTLSServer(nil)
			certificates := certServer.TLS.Certificates
			certServer.Close()
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			payload := bytes.Repeat([]byte("replacement wasm bytes\x00"), 4096)
			path := filepath.Join(t.TempDir(), "replacement.wasm")
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() {
				serverDone <- func() error {
					conn, err := listener.Accept()
					if err != nil {
						return err
					}
					defer conn.Close()
					if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
						return err
					}
					handshake, err := readConsoleTestMessage(conn)
					if err != nil {
						return err
					}
					if handshake.GetHandshake() == nil {
						return fmt.Errorf("missing handshake: %v", handshake)
					}
					if err := writeConsoleTestMessage(conn, handshake); err != nil {
						return err
					}
					state := "begin"
					var received []byte
					for {
						message, err := readConsoleTestMessage(conn)
						if err != nil {
							if errors.Is(err, io.EOF) && state == "done" {
								return nil
							}
							return fmt.Errorf("waiting for %s: %w", state, err)
						}
						command := message.GetCommand()
						result := pb.WendyComResult_WENDY_COM_RESULT_OK
						switch state {
						case "begin":
							begin := command.GetAppPushBegin()
							if begin == nil || begin.Size != uint32(len(payload)) || begin.AppType != pb.WendyComAppType_WENDY_COM_APP_TYPE_WASM {
								return fmt.Errorf("replacement must start with WASM PushBegin, got %v", command)
							}
							state = "data"
						case "data":
							if chunk := command.GetAppPushData(); chunk != nil {
								if chunk.Offset != uint32(len(received)) {
									return fmt.Errorf("non-contiguous push: %v", chunk)
								}
								received = append(received, chunk.Data...)
							} else if command.GetAppPushEnd() != nil {
								if !bytes.Equal(received, payload) {
									return fmt.Errorf("replacement payload differs")
								}
								state = "start"
								if rejectPush {
									result = pb.WendyComResult_WENDY_COM_RESULT_BAD_STATE
									state = "done"
								}
							} else {
								return fmt.Errorf("expected push data/end, got %v", command)
							}
						case "start":
							if command.GetAppStart() == nil {
								return fmt.Errorf("expected StartApp, got %v", command)
							}
							state = "done"
						default:
							return fmt.Errorf("unexpected command after replacement: %v", command)
						}
						if err := writeConsoleTestMessage(conn, &pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{
							Response: &pb.WendyComResponse{RequestId: command.RequestId, Result: result},
						}}); err != nil {
							return err
						}
					}
				}()
			}()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			output := make(chan RunOutput, 8)
			err = (&MicroWendyProvider{}).Run(ctx, &BuiltApp{
				Device:  models.ExternalDevice{ConnectionInfo: map[string]string{"type": "LAN", "ip": host, "port": port}},
				Context: &microWendyBuildContext{AppPath: path, AppType: liteclient.AppTypeWasm},
			}, true, output)
			if rejectPush {
				if err == nil {
					t.Fatalf("rejected push: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			started := 0
			for event := range output {
				if event.Type == RunOutputStarted {
					started++
				}
			}
			want := 1
			if rejectPush {
				want = 0
			}
			if started != want {
				t.Fatalf("started events = %d, want %d", started, want)
			}
			select {
			case err := <-serverDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
