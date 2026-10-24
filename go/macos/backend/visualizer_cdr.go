package backend

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

type visualROSValue struct {
	Name, Unit string
	Value      float64
}
type visualROSDecoded struct {
	Points []VisualPoint
	Values []visualROSValue
}

// CDR alignment starts after the four-byte encapsulation header. Bounds are
// checked before every read, including sequence lengths from remote messages.
type visualCDR struct {
	data   []byte
	offset int
	order  binary.ByteOrder
	err    error
}

func newVisualCDR(raw []byte) *visualCDR {
	r := &visualCDR{order: binary.LittleEndian}
	if len(raw) < 4 || raw[0] != 0 || raw[1] > 1 {
		r.err = errors.New("unsupported CDR encapsulation")
		return r
	}
	if raw[1] == 0 {
		r.order = binary.BigEndian
	}
	r.data = raw[4:]
	return r
}
func (r *visualCDR) take(n, align int) []byte {
	if r.err != nil {
		return make([]byte, max(0, min(n, 8)))
	}
	offset := (r.offset + align - 1) & ^(align - 1)
	if n < 0 || offset > len(r.data) || n > len(r.data)-offset {
		r.err = errors.New("truncated CDR message")
		return make([]byte, max(0, min(n, 8)))
	}
	r.offset = offset + n
	return r.data[offset:r.offset]
}
func (r *visualCDR) u8() byte     { b := r.take(1, 1); return b[0] }
func (r *visualCDR) u32() uint32  { return r.order.Uint32(r.take(4, 4)) }
func (r *visualCDR) f32() float64 { return float64(math.Float32frombits(r.u32())) }
func (r *visualCDR) f64() float64 { return math.Float64frombits(r.order.Uint64(r.take(8, 8))) }
func (r *visualCDR) str() string {
	n := r.u32()
	if n == 0 || n > 1<<20 {
		r.err = errors.New("invalid CDR string length")
		return ""
	}
	b := r.take(int(n), 1)
	if r.err != nil {
		return ""
	}
	if b[len(b)-1] != 0 {
		r.err = errors.New("unterminated CDR string")
		return ""
	}
	return string(b[:len(b)-1])
}
func visualFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func decodeVisualROS(typ string, payload []byte) (visualROSDecoded, string, error) {
	var out visualROSDecoded
	r := newVisualCDR(payload)
	add := func(name, unit string, value float64) {
		if visualFinite(value) {
			out.Values = append(out.Values, visualROSValue{name, unit, value})
		}
	}
	vector := func(prefix, unit string) {
		for _, axis := range []string{"x", "y", "z"} {
			add(prefix+axis, unit, r.f64())
		}
	}
	frame := ""
	header := func() {
		sec := int32(r.u32())
		ns := r.u32()
		frame = fmt.Sprintf("Frame: %s · Header time: %d.%09d", r.str(), sec, ns)
	}
	switch typ {
	case "sensor_msgs/msg/LaserScan":
		header()
		angle := r.f32()
		_ = r.f32()
		step := r.f32()
		_ = r.f32()
		_ = r.f32()
		low, high := r.f32(), r.f32()
		n := r.u32()
		if n > 4_000_000 || !visualFinite(angle) || !visualFinite(step) || !visualFinite(low) || !visualFinite(high) || low < 0 || high < low {
			return out, frame, errors.New("invalid LaserScan ranges")
		}
		stride := max(1, (int(n)+visualPointLimit-1)/visualPointLimit)
		for i := 0; i < int(n); i++ {
			distance := r.f32()
			if r.err != nil {
				break
			}
			if i%stride == 0 && visualFinite(distance) && distance >= low && distance <= high {
				a := angle + float64(i)*step
				out.Points = append(out.Points, VisualPoint{float32(distance * math.Cos(a)), float32(distance * math.Sin(a)), 0})
			}
		}
		intensities := r.u32()
		if intensities > 4_000_000 {
			return out, frame, errors.New("LaserScan intensity array exceeds preview limit")
		}
		_ = r.take(int(intensities)*4, 4)
	case "sensor_msgs/msg/PointCloud2":
		header()
		height, width := r.u32(), r.u32()
		n := r.u32()
		if n > 256 {
			return out, frame, errors.New("too many point fields")
		}
		type field struct{ offset, kind, count uint32 }
		fields := map[string]field{}
		for i := uint32(0); i < n; i++ {
			name := r.str()
			offset := r.u32()
			kind := r.u8()
			count := r.u32()
			fields[name] = field{offset, uint32(kind), count}
		}
		big := r.u8() != 0
		pointStep, rowStep := r.u32(), r.u32()
		size := r.u32()
		if size > visualFileLimit {
			return out, frame, errors.New("point cloud exceeds preview limit")
		}
		blob := r.take(int(size), 1)
		_ = r.u8() // is_dense
		if r.err != nil {
			return out, frame, r.err
		}
		if pointStep == 0 || uint64(width)*uint64(pointStep) > uint64(rowStep) || uint64(height)*uint64(rowStep) > uint64(len(blob)) {
			return out, frame, errors.New("invalid point cloud strides")
		}
		var order binary.ByteOrder = binary.LittleEndian
		if big {
			order = binary.BigEndian
		}
		for _, axis := range []string{"x", "y", "z"} {
			f, ok := fields[axis]
			size := uint32(4)
			if f.kind == 8 {
				size = 8
			}
			if !ok || (f.kind != 7 && f.kind != 8) || f.count != 1 || uint64(f.offset)+uint64(size) > uint64(pointStep) {
				return out, frame, errors.New("point cloud needs scalar float32 or float64 x, y, z fields")
			}
		}
		total := uint64(height) * uint64(width)
		stride := max(uint64(1), (total+visualPointLimit-1)/visualPointLimit)
		for i := uint64(0); i < total; i += stride {
			base := (i/uint64(width))*uint64(rowStep) + (i%uint64(width))*uint64(pointStep)
			var xyz [3]float32
			valid := true
			for a, axis := range []string{"x", "y", "z"} {
				f := fields[axis]
				pos := int(base + uint64(f.offset))
				var value float64
				if f.kind == 8 {
					value = math.Float64frombits(order.Uint64(blob[pos:]))
				} else {
					value = float64(math.Float32frombits(order.Uint32(blob[pos:])))
				}
				if !visualFinite(value) || math.Abs(value) > math.MaxFloat32 {
					valid = false
				}
				xyz[a] = float32(value)
			}
			if valid {
				out.Points = append(out.Points, VisualPoint{xyz[0], xyz[1], xyz[2]})
			}
		}
	case "sensor_msgs/msg/Imu":
		header()
		for _, axis := range []string{"x", "y", "z", "w"} {
			add("orientation/"+axis, "", r.f64())
		}
		for i := 0; i < 9; i++ {
			_ = r.f64()
		}
		vector("angular_velocity/", "rad/s")
		for i := 0; i < 9; i++ {
			_ = r.f64()
		}
		vector("linear_acceleration/", "m/s²")
		for i := 0; i < 9; i++ {
			_ = r.f64()
		}
	case "sensor_msgs/msg/NavSatFix":
		header()
		_ = r.u8()
		_ = r.take(2, 2)
		add("latitude", "°", r.f64())
		add("longitude", "°", r.f64())
		add("altitude", "m", r.f64())
	case "geometry_msgs/msg/Vector3":
		vector("", "")
	case "geometry_msgs/msg/Point":
		vector("", "m")
	case "geometry_msgs/msg/Vector3Stamped":
		header()
		vector("", "")
	case "geometry_msgs/msg/PointStamped":
		header()
		vector("", "m")
	case "geometry_msgs/msg/Twist", "geometry_msgs/msg/TwistStamped":
		if strings.HasSuffix(typ, "Stamped") {
			header()
		}
		vector("linear/", "m/s")
		vector("angular/", "rad/s")
	case "std_msgs/msg/Float32":
		add("value", "", r.f32())
	case "std_msgs/msg/Float64":
		add("value", "", r.f64())
	case "std_msgs/msg/Int32":
		add("value", "", float64(int32(r.u32())))
	case "std_msgs/msg/UInt32":
		add("value", "", float64(r.u32()))
	case "std_msgs/msg/Bool":
		add("value", "", float64(r.u8()))
	case "std_msgs/msg/String":
		frame = r.str()
	default:
		return out, frame, errors.New("no preview decoder for this message type; export preserves the original payload")
	}
	if r.err != nil {
		return visualROSDecoded{}, frame, r.err
	}
	return out, frame, nil
}
