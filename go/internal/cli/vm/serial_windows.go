package vm

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var serialPortName = regexp.MustCompile(`^COM[1-9][0-9]*$`)

func validateHostSerialPort(port string) error {
	if !serialPortName.MatchString(port) {
		return fmt.Errorf("invalid Windows serial port %q: use COM followed by its number", port)
	}
	return nil
}

// NormalizeSerialPort accepts the casing Device Manager users type.
func NormalizeSerialPort(port string) string { return strings.ToUpper(strings.TrimSpace(port)) }

// Use explicit overlapped I/O rather than os.NewFile: Windows COM handles are
// not ordinary seekable files. Keep the handle immutable until all cancelled
// operations have completed; closing/reusing a handle during I/O is unsafe.
type windowsSerial struct {
	handle   windows.Handle
	mu       sync.Mutex
	closing  bool
	pending  sync.WaitGroup
	once     sync.Once
	closeErr error
	// The kernel writes completion status through the OVERLAPPED after
	// ReadFile/WriteFile return. On a goroutine stack, a stack move in that
	// window sends the write to freed memory and the result reads as zero
	// bytes, so each direction keeps its own here, in this heap object.
	reader, writer serialOp
}

// serialOp serializes one direction; io.Copy issues one Read and one Write at
// a time. ReadFile/WriteFile reset the reused event when each call begins.
type serialOp struct {
	mu    sync.Mutex
	event windows.Handle // immutable after open, so Close may read it unlocked
	ov    windows.Overlapped
}

func openHostSerial(c SerialConfig) (io.ReadWriteCloser, error) {
	path, err := windows.UTF16PtrFromString(`\\.\` + c.Port)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s exclusively: %w; check Device Manager and close other VMs or programs using this COM port", c.Port, err)
	}
	fail := func(err error) (io.ReadWriteCloser, error) { windows.CloseHandle(h); return nil, err }
	// Binary only: disable parity checking, software/hardware flow control,
	// null stripping, error replacement, DTR and RTS from the outset.
	dcb := windows.DCB{DCBlength: uint32(unsafe.Sizeof(windows.DCB{})), BaudRate: c.Baud, Flags: 1, ByteSize: 8, Parity: windows.NOPARITY, StopBits: windows.ONESTOPBIT, XonChar: 17, XoffChar: 19}
	if err := windows.SetCommState(h, &dcb); err != nil {
		return fail(fmt.Errorf("configuring %s at exact %d baud, 8N1: %w; choose a baud supported by the Windows driver (no nearest-baud fallback)", c.Port, c.Baud, err))
	}
	actual := windows.DCB{DCBlength: uint32(unsafe.Sizeof(windows.DCB{}))}
	if err := windows.GetCommState(h, &actual); err != nil {
		return fail(fmt.Errorf("verifying %s serial settings: %w", c.Port, err))
	}
	if actual.BaudRate != c.Baud || actual.ByteSize != 8 || actual.Parity != windows.NOPARITY || actual.StopBits != windows.ONESTOPBIT || actual.Flags&0x7f7e != 0 || actual.Flags&1 == 0 {
		return fail(fmt.Errorf("Windows driver did not retain exact %d baud, 8N1 with flow control/DTR/RTS disabled on %s (reported baud %d)", c.Baud, c.Port, actual.BaudRate))
	}
	// Bounded reads return zero during idle periods; the bridge retries those.
	// Allow the full 8N1 transmission duration plus five seconds of slack.
	// A stalled transmit fails without penalizing legitimate low-baud traffic.
	if err := windows.SetCommTimeouts(h, &windows.CommTimeouts{ReadIntervalTimeout: ^uint32(0), ReadTotalTimeoutMultiplier: ^uint32(0), ReadTotalTimeoutConstant: 100, WriteTotalTimeoutConstant: 5000, WriteTotalTimeoutMultiplier: serialWriteTimeoutMultiplier(c.Baud)}); err != nil {
		return fail(fmt.Errorf("configuring %s timeouts: %w", c.Port, err))
	}
	p := &windowsSerial{handle: h}
	for _, op := range []*serialOp{&p.reader, &p.writer} {
		if op.event, err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
			p.closeEvents()
			return fail(fmt.Errorf("creating %s I/O event: %w", c.Port, err))
		}
	}
	return p, nil
}
func (p *windowsSerial) closeEvents() {
	for _, op := range []*serialOp{&p.reader, &p.writer} {
		if op.event != 0 {
			windows.CloseHandle(op.event)
		}
	}
}
func (p *windowsSerial) transfer(b []byte, write bool) (int, error) {
	op := &p.reader
	if write {
		op = &p.writer
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	op.ov = windows.Overlapped{HEvent: op.event}
	// The kernel fills or drains b asynchronously as well. Pinning makes
	// every caller's buffer escape to the heap and holds it until completion.
	var pin runtime.Pinner
	pin.Pin(&b[0])
	defer pin.Unpin()
	var n uint32
	var err error
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	p.pending.Add(1)
	if write {
		err = windows.WriteFile(p.handle, b, &n, &op.ov)
	} else {
		err = windows.ReadFile(p.handle, b, &n, &op.ov)
	}
	p.mu.Unlock()
	defer p.pending.Done()
	if err == windows.ERROR_IO_PENDING {
		err = windows.GetOverlappedResult(p.handle, &op.ov, &n, true)
	}
	if write && err == nil && int(n) != len(b) {
		err = io.ErrShortWrite
	}
	return int(n), err
}
func (p *windowsSerial) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		n, err := p.transfer(b, false)
		if n != 0 || err != nil {
			return n, err
		}
	}
}
func (p *windowsSerial) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return p.transfer(b, true)
}
func (p *windowsSerial) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closing = true
		// No new I/O can be issued between cancellation and pending.Wait.
		_ = windows.CancelIoEx(p.handle, nil)
		p.mu.Unlock()
		p.pending.Wait()
		p.closeErr = windows.CloseHandle(p.handle)
		// Only after every operation has completed: none can reach an event now.
		p.closeEvents()
	})
	return p.closeErr
}

type serialJob windows.Handle

func (j serialJob) Close() error { return windows.CloseHandle(windows.Handle(j)) }
func guardSerialProcess(p *os.Process) (io.Closer, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("creating serial VM lifetime guard: %w", err)
	}
	fail := func(err error) (io.Closer, error) {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("guarding serial VM lifetime: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fail(err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return fail(err)
	}
	return serialJob(job), nil
}
