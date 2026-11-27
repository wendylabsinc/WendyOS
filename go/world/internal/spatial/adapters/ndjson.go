package adapters

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ErrOutOfOrder: a recorded measurement is older than the one before it from
// the same device and sensor.
var ErrOutOfOrder = errors.New("adapters: measurement older than its predecessor")

// RecordError locates a bad record in a measurement log.
type RecordError struct {
	Line int // 1-based
	Err  error
}

func (e *RecordError) Error() string { return fmt.Sprintf("adapters: line %d: %v", e.Line, e.Err) }
func (e *RecordError) Unwrap() error { return e.Err }

// WriteRecord appends m to a measurement log: one line as encoded by
// spatial.EncodeMeasurement, written with a single Write call.
func WriteRecord(w io.Writer, m spatial.Measurement) error {
	line, err := spatial.EncodeMeasurement(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

// ReaderOptions tune a Reader. The zero value is the strict default.
type ReaderOptions struct {
	// MaxLineBytes bounds one line. Default spatial.MaxMeasurementBytes,
	// the most one record may hold; a longer line ends the stream with
	// bufio.ErrTooLong.
	MaxLineBytes int
	// AllowOutOfOrder yields a record that is older than its predecessor
	// from the same device and sensor, counting it in OutOfOrder, instead
	// of ending the stream with ErrOutOfOrder. Equal times are always in
	// order: one ranging round stamps several records alike.
	AllowOutOfOrder bool
}

// Reader streams measurements from a recorded log: one record per line, in
// the order written. It is strict: the first undecodable, invalid or
// overlong record ends the stream with a *RecordError naming its line, and so
// does a record out of time order unless AllowOutOfOrder is set. Blank lines
// are skipped.
//
// Order is checked per device and sensor on the recorded stamps, which are on
// each sensor's own clock. Records of different sensors are not compared, and
// a recorder that interleaves them late is not an error.
//
// ponytail: no reorder buffer. Ceiling: a log whose single sensor stream is
// out of order can be inspected (AllowOutOfOrder) but not repaired. Upgrade
// path: a bounded per-stream reorder window.
type Reader struct {
	lines      *bufio.Scanner
	limit      int
	allow      bool
	line       int
	outOfOrder int
	last       map[[2]string]time.Time
	err        error
}

// NewReader reads a measurement log from r.
func NewReader(r io.Reader, opt ReaderOptions) *Reader {
	limit := opt.MaxLineBytes
	if limit <= 0 {
		limit = spatial.MaxMeasurementBytes
	}
	lines := bufio.NewScanner(r)
	// The scanner's limit includes the line terminator ("\r\n" at most).
	lines.Buffer(make([]byte, 0, min(limit+2, 64<<10)), limit+2)
	return &Reader{lines: lines, limit: limit, allow: opt.AllowOutOfOrder, last: map[[2]string]time.Time{}}
}

// Next returns the next measurement, or io.EOF at the end of the log. After
// any error every further call returns the same error.
func (r *Reader) Next() (spatial.Measurement, error) {
	if r.err != nil {
		return nil, r.err
	}
	for r.lines.Scan() {
		r.line++
		if len(r.lines.Bytes()) > r.limit {
			return nil, r.fail(&RecordError{r.line, bufio.ErrTooLong})
		}
		text := bytes.TrimSpace(r.lines.Bytes())
		if len(text) == 0 {
			continue
		}
		m, err := spatial.DecodeMeasurement(text)
		if err != nil {
			return nil, r.fail(&RecordError{r.line, err})
		}
		device, sensor := m.From()
		stream := [2]string{string(device), string(sensor)}
		if last, seen := r.last[stream]; seen && m.When().Before(last) {
			if !r.allow {
				return nil, r.fail(&RecordError{r.line, fmt.Errorf("%w: %s/%s at %s after %s", ErrOutOfOrder, device, sensor, m.When().Format(time.RFC3339Nano), last.Format(time.RFC3339Nano))})
			}
			r.outOfOrder++
		} else {
			r.last[stream] = m.When()
		}
		return m, nil
	}
	if err := r.lines.Err(); err != nil {
		return nil, r.fail(&RecordError{r.line + 1, err})
	}
	return nil, r.fail(io.EOF)
}

func (r *Reader) fail(err error) error { r.err = err; return err }

// Line is the 1-based number of the line the last call to Next consumed.
func (r *Reader) Line() int { return r.line }

// OutOfOrder counts the records yielded under AllowOutOfOrder that were older
// than their stream's latest record.
func (r *Reader) OutOfOrder() int { return r.outOfOrder }

// MeasuredInput is the ancestry entry of a measurement that real hardware
// produced live: origin "measured", never "simulated". Pass the entries of an
// estimate's inputs to spatial.InferredProvenance.
func MeasuredInput(m spatial.Measurement) world.Input {
	return world.Input{ID: spatial.InputID(m), Origin: world.Measured}
}

// ReplayedInput is the ancestry entry of a measurement of real hardware read
// back from a recording: origin "replayed", with the recorded stamp as the
// original capture time and Config.ClockDomain as the clock it is on. An
// estimate built on it is reported as synthetic by world.Provenance, as a
// replay must be. A log does not record where it came from: do not use this
// for a recording of a simulation, whose entries must keep their
// SimulationContext.
func (c Config) ReplayedInput(m spatial.Measurement) world.Input {
	at, domain := m.When(), c.ClockDomain
	if domain == "" {
		domain = DefaultClockDomain
	}
	return world.Input{ID: spatial.InputID(m), Origin: world.Replayed, OriginalCaptureTime: &at, OriginalClockDomain: domain}
}

// InputFrom is the ancestry entry of a measurement converted from a World
// component, such as the one a sensing.Sample arrived in: it carries over that
// component's own origin (measured, simulated with its context, or replayed
// with its capture time and clock) instead of asserting one. A component that
// is itself inferred or authored is not a leaf input and is refused.
func InputFrom(m spatial.Measurement, source world.Provenance) (world.Input, error) {
	if err := source.Validate(); err != nil {
		return world.Input{}, invalid("source provenance: %v", err)
	}
	switch source.Origin {
	case world.Measured, world.Simulated, world.Replayed:
		return world.Input{ID: spatial.InputID(m), Origin: source.Origin, Simulation: source.Simulation, OriginalCaptureTime: source.OriginalCaptureTime, OriginalClockDomain: source.OriginalClockDomain}, nil
	}
	return world.Input{}, fmt.Errorf("%w: a component of origin %q is not a leaf input", ErrUnmapped, source.Origin)
}
