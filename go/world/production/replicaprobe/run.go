package replicaprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"google.golang.org/grpc"
)

func Dial(path string) (*grpc.ClientConn, error) {
	if path != "/run/wendy/world/world.sock" {
		return nil, ErrConfig
	}
	return grpc.NewClient("passthrough:///world-replica-probe", grpc.WithTransportCredentials(appservice.ClientCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}), grpc.WithDisableRetry(), grpc.WithDefaultCallOptions(grpc.MaxRetryRPCBufferSize(0), grpc.MaxCallRecvMsgSize(MaxEnvelopeBytes), grpc.MaxCallSendMsgSize(64<<10)))
}

func encoded(v any, limit int) ([]byte, error) {
	raw, e := json.Marshal(v)
	if e != nil || len(raw)+1 > limit {
		return nil, ErrResponse
	}
	return append(raw, '\n'), nil
}

// Run watches only the next numbered command. It writes a session marker before
// any RPC, then one exclusive receipt per attempted command. Existing receipts
// prohibit replay after a process crash. A fresh deployment uses a fresh volume.
func Run(ctx context.Context, c Config, client appv1.WorldAppServiceClient, commands, receipts string) (out Result, err error) {
	out = Result{Version: 1, RunID: c.RunID, Status: "failed"}
	if ctx == nil || !filepath.IsAbs(commands) || !filepath.IsAbs(receipts) || filepath.Clean(commands) != commands || filepath.Clean(receipts) != receipts || commands == receipts || strings.HasPrefix(commands, receipts+"/") || strings.HasPrefix(receipts, commands+"/") {
		return out, ErrConfig
	}
	c, e := c.normalized()
	if e != nil {
		return out, e
	}
	p, e := New(c, client)
	if e != nil {
		return out, e
	}
	defer p.Close()
	control, e := openDirectory(commands)
	if e != nil {
		return out, e
	}
	defer control.close()
	proofs, e := openDirectory(receipts)
	if e != nil {
		return out, e
	}
	defer proofs.close()
	entries, e := proofs.file.ReadDir(1)
	if e != nil && !errors.Is(e, io.EOF) {
		return out, e
	}
	if len(entries) != 0 {
		return out, ErrConfig
	}
	configJSON, _ := json.Marshal(c)
	session := struct {
		Version      int    `json:"version"`
		RunID        string `json:"run_id"`
		Device       string `json:"device"`
		PeerDevice   string `json:"peer_device"`
		ConfigSHA256 string `json:"config_sha256"`
		Status       string `json:"status"`
	}{1, c.RunID, c.Device, c.PeerDevice, digest(configJSON), "awaiting_command"}
	raw, e := encoded(session, MaxConfigBytes)
	if e != nil {
		return out, e
	}
	if e = proofs.write("session.json", raw, MaxConfigBytes); e != nil {
		return out, e
	}
	defer func() {
		if err != nil {
			out.ErrorCode = code(err)
			if errors.Is(err, ErrUncertain) {
				out.Status = "unknown_outcome"
			}
		}
		raw, e := encoded(out, MaxConfigBytes)
		if e == nil {
			e = proofs.write("result.json", raw, MaxConfigBytes)
		}
		if e != nil {
			err = errors.Join(err, e)
			out.Status = "failed"
			out.ErrorCode = "receipt_write_failed"
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.DurationMS)*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(time.Duration(c.PollIntervalMS) * time.Millisecond)
	defer ticker.Stop()
	for sequence := uint32(1); sequence <= uint32(c.MaxCommands); sequence++ {
		name := fmt.Sprintf("%06d.json", sequence)
		var command []byte
		for {
			command, e = control.read(name, MaxCommandBytes)
			if e == nil {
				break
			}
			if !os.IsNotExist(e) {
				return out, ErrCommand
			}
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-ticker.C:
			}
		}
		receipt, operationErr := p.Execute(ctx, command)
		raw, e = encoded(receipt, MaxReceiptBytes)
		if e == nil {
			e = proofs.write(name, raw, MaxReceiptBytes)
		}
		if e != nil {
			if receipt.Action == "publish" || receipt.Action == "tombstone" {
				return out, errors.Join(ErrUncertain, e)
			}
			return out, e
		}
		if operationErr != nil {
			return out, operationErr
		}
		out.Completed = sequence
		if receipt.Action == "finish" {
			out.Status = "complete"
			return out, nil
		}
	}
	return out, ErrCommand
}
