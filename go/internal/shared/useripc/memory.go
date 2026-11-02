package useripc

import (
	"context"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/test/bufconn"
)

// Embedded applications cannot spawn helpers, and iOS container paths exceed
// sockaddr_un's path limit. Keep the broker protocol but carry it over bounded
// in-process streams. Private on-disk markers retain normal unpublish semantics.
var inProcess atomic.Bool
var memoryListeners = struct {
	sync.Mutex
	items map[string]*memoryListener
}{items: make(map[string]*memoryListener)}

func EnableInProcess() { inProcess.Store(true) }

type memoryListener struct {
	*bufconn.Listener
	path string
	once sync.Once
}

func memoryListen(path string) (net.Listener, error) {
	marker, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = marker.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	l := &memoryListener{Listener: bufconn.Listen(256 << 10), path: path}
	memoryListeners.Lock()
	if memoryListeners.items[path] != nil {
		memoryListeners.Unlock()
		l.Listener.Close()
		_ = os.Remove(path)
		return nil, &net.OpError{Op: "listen", Net: "in-process", Err: os.ErrExist}
	}
	memoryListeners.items[path] = l
	memoryListeners.Unlock()
	return l, nil
}

func memoryExists(path string) bool {
	memoryListeners.Lock()
	defer memoryListeners.Unlock()
	return memoryListeners.items[path] != nil
}

func memoryDial(ctx context.Context, path string) (net.Conn, error) {
	if err := ValidateFile(path, true); err != nil {
		return nil, err
	}
	memoryListeners.Lock()
	l := memoryListeners.items[path]
	memoryListeners.Unlock()
	if l == nil {
		return nil, net.ErrClosed
	}
	return l.DialContext(ctx)
}

func (l *memoryListener) Close() error {
	var err error
	l.once.Do(func() {
		memoryListeners.Lock()
		if memoryListeners.items[l.path] == l {
			delete(memoryListeners.items, l.path)
		}
		memoryListeners.Unlock()
		err = l.Listener.Close()
	})
	return err
}
