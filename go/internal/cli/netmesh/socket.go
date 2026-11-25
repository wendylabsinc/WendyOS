package netmesh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

type Request struct {
	Device string `json:"device,omitempty"`
	Port   uint16 `json:"port,omitempty"`
}
type Response struct {
	Status Status `json:"status"`
	Error  string `json:"error,omitempty"`
}

func SocketPath(dir string) string { return filepath.Join(dir, "mesh.sock") }
func PrivateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("mesh directory must be a private directory with mode 0700")
	}
	return nil
}

// Serve owns the private socket and the single routing instance. valid checks
// whether the operator certificate still belongs to the selected organization.
func Serve(ctx context.Context, dir string, create func(context.Context) (*Client, error), valid func() bool) error {
	if err := PrivateDirectory(dir); err != nil {
		return err
	}
	release, err := flock.Acquire(filepath.Join(dir, "session.lock"), 15*time.Second)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	path := SocketPath(dir)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("mesh socket path is not a socket")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	client, err := create(ctx)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx); cancel() }()
	var wg sync.WaitGroup
	defer func() { cancel(); listener.Close(); wg.Wait(); <-done }()
	var active atomic.Int32
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if !valid() || (active.Load() == 0 && time.Since(time.Unix(0, last.Load())) > 2*time.Minute) {
					cancel()
					return
				}
			}
		}
	}()
	slots := make(chan struct{}, 64)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			connection.Close()
			continue
		}
		active.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots; active.Add(-1); last.Store(time.Now().UnixNano()) }()
			defer connection.Close()
			stop := context.AfterFunc(ctx, func() { connection.Close() })
			defer stop()
			handleSocket(ctx, client, connection)
		}()
	}
}
func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var result []byte
	for {
		part, prefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(result)+len(part) > limit {
			return nil, errors.New("oversized mesh socket message")
		}
		result = append(result, part...)
		if !prefix {
			return result, nil
		}
	}
}
func handleSocket(ctx context.Context, client *Client, connection net.Conn) {
	connection.SetDeadline(time.Now().Add(35 * time.Second))
	reader := bufio.NewReader(connection)
	line, err := readLine(reader, 4096)
	if err != nil {
		return
	}
	var request Request
	if json.Unmarshal(line, &request) != nil {
		return
	}
	if request.Device == "" {
		json.NewEncoder(connection).Encode(Response{Status: client.Status()})
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	remote, err := client.Dial(dialCtx, request.Device, request.Port)
	cancel()
	if err != nil {
		json.NewEncoder(connection).Encode(Response{Error: err.Error()})
		return
	}
	defer remote.Close()
	if json.NewEncoder(connection).Encode(Response{}) != nil {
		return
	}
	connection.SetDeadline(time.Time{})
	// Preserve half-closes, allowing a request EOF to precede its response.
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(remote, reader)
		if c, ok := remote.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		} else {
			remote.Close()
		}
	}()
	io.Copy(connection, remote)
	connection.Close()
	remote.Close()
	<-done
}
func request(ctx context.Context, dir string, req Request) (Response, net.Conn, error) {
	var response Response
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(dir))
	if err != nil {
		return response, nil, err
	}
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	success := false
	defer func() {
		stop()
		if !success {
			connection.Close()
		}
	}()
	deadline := time.Now().Add(35 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	connection.SetDeadline(deadline)
	if err = json.NewEncoder(connection).Encode(req); err != nil {
		return response, nil, err
	}
	reader := bufio.NewReader(connection)
	line, err := readLine(reader, 1<<20)
	if err != nil {
		return response, nil, err
	}
	if err = json.Unmarshal(line, &response); err != nil {
		return response, nil, err
	}
	if response.Error != "" {
		return response, nil, fmt.Errorf("mesh: %s", response.Error)
	}
	if !stop() {
		return response, nil, ctx.Err()
	}
	if err = ctx.Err(); err != nil {
		return response, nil, err
	}
	connection.SetDeadline(time.Time{})
	success = true
	return response, &bufferedConnection{Conn: connection, reader: reader}, nil
}
func ReadStatus(ctx context.Context, dir string) (Status, error) {
	response, connection, err := request(ctx, dir, Request{})
	if connection != nil {
		connection.Close()
	}
	return response.Status, err
}
func Dial(ctx context.Context, dir, id string, port uint16) (net.Conn, error) {
	_, connection, err := request(ctx, dir, Request{Device: id, Port: port})
	return connection, err
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *bufferedConnection) CloseWrite() error {
	if raw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return raw.CloseWrite()
	}
	return c.Close()
}
