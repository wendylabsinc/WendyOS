package go2nav

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// These cases and their full-output fingerprints are a frozen evaluator, not
// candidate-owned training data. Existing independent geometry and refusal tests
// remain part of the gate. Floats use nine decimal places to tolerate platform
// rounding; integer provenance, ring masks, occupancy and reasons remain exact.
type autoresearchNavigationCase struct {
	name        string
	observation CloudObservation
	profile     Profile
	bounds      Bounds
	want        string
}

func autoresearchNavigationCases() []autoresearchNavigationCase {
	cases := make([]autoresearchNavigationCase, 0, 6)
	add := func(name string, configure func(*CloudObservation, *Profile), bounds Bounds, want string) {
		o, p := cloudFixture()
		configure(&o, &p)
		copy(o.Scan.Ranges, o.Ranges[12*CloudAzimuths:13*CloudAzimuths])
		cases = append(cases, autoresearchNavigationCase{name, o, p, bounds, want})
	}
	add("maximum_grid", func(*CloudObservation, *Profile) {}, Bounds{Point{-4, -4}, Point{4, 4}}, "2e1709dff3071bbb1d9242b917fcf6fd19df22d70cd4a0ce364d3125d5cb9f3b")
	add("partial_rings", func(o *CloudObservation, _ *Profile) {
		cloudOnlyRings(o, 11, 13)
		o.Ranges[11*CloudAzimuths+175] = math.Inf(1)
		o.Ranges[13*CloudAzimuths+185] = math.Inf(1)
		o.Ranges[10*CloudAzimuths+180] = 2.77
	}, area(), "7808a1a3f2f033669969bb2394fa1d44b2a7730327da0213f34393f56e886058")
	add("clutter", func(o *CloudObservation, _ *Profile) {
		for i := range o.Ranges {
			o.Ranges[i] = .7 + float64((i*37+i/CloudAzimuths*13)%570)/100
			if i%29 == 0 {
				o.Ranges[i] = math.Inf(1)
			}
		}
	}, area(), "00d124397180d0ad59e864f67df8865a6aaba5b8f238804c7aa7d61dd916d4c4")
	add("tilted_uncertain", func(o *CloudObservation, _ *Profile) {
		o.Scan.Robot = Pose{X: .3, Y: -.2, Yaw: .7}
		o.Scan.TiltedPose.OdomPositionXYZ = [3]float64{.3, -.2, .33}
		o.Scan.TiltedPose.QuaternionWXYZ = tiltQuaternion(.12, -.11, .7)
		o.Scan.PositionUncertaintyM = .12
		o.Scan.YawUncertaintyRad = .09
		o.Scan.Sequence = 83
		o.Scan.WorldEpoch = 17
		for i := range o.Ranges {
			o.Ranges[i] = 2 + float64((i*17)%450)/100
		}
	}, area(), "524f464ae910ffeebf5272373a522e5e0cad353dad67c616d565d10e4bce7d49")
	add("missing", func(o *CloudObservation, _ *Profile) {
		for i := range o.Ranges {
			o.Ranges[i] = math.Inf(1)
		}
	}, area(), "a55230a44b1cc6c5476754eddc1fb6113117944518411ea6682efa36ef87e6ed")
	add("boundary_returns", func(o *CloudObservation, _ *Profile) {
		for i := range o.Ranges {
			switch i % 5 {
			case 0:
				o.Ranges[i] = .1
			case 1:
				o.Ranges[i] = 12
			case 2:
				o.Ranges[i] = .7
			case 3:
				o.Ranges[i] = 2.77
			case 4:
				o.Ranges[i] = math.Inf(1)
			}
		}
	}, Bounds{Point{-1.05, -.95}, Point{2.15, 1.25}}, "82b2ada20ba9b2e6d5086fd8e975afef969a0f56ac63a2657fba9df28a658398")
	return cases
}

// Hash every field, including private Grid state, with canonical map ordering.
// This deliberately does not call production geometry/classification helpers.
func autoresearchNavigationFingerprint(value any) string {
	var encode func(*bytes.Buffer, reflect.Value)
	encode = func(out *bytes.Buffer, v reflect.Value) {
		fmt.Fprintf(out, "%s{", v.Type())
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				out.WriteString("nil")
			} else {
				encode(out, v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				out.WriteString(v.Type().Field(i).Name)
				encode(out, v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			fmt.Fprintf(out, "%d:", v.Len())
			for i := 0; i < v.Len(); i++ {
				encode(out, v.Index(i))
			}
		case reflect.Map:
			type entry struct {
				key   []byte
				value reflect.Value
			}
			entries := make([]entry, 0, v.Len())
			for _, key := range v.MapKeys() {
				var b bytes.Buffer
				encode(&b, key)
				entries = append(entries, entry{b.Bytes(), v.MapIndex(key)})
			}
			sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].key, entries[j].key) < 0 })
			for _, e := range entries {
				out.Write(e.key)
				encode(out, e.value)
			}
		case reflect.Float64:
			f := v.Float()
			if math.Abs(f) < .5e-9 {
				f = 0
			}
			out.WriteString(strconv.FormatFloat(f, 'f', 9, 64))
		case reflect.Int, reflect.Int64:
			out.WriteString(strconv.FormatInt(v.Int(), 10))
		case reflect.Uint, reflect.Uint8, reflect.Uint32, reflect.Uint64:
			out.WriteString(strconv.FormatUint(v.Uint(), 10))
		case reflect.String:
			out.WriteString(strconv.Quote(v.String()))
		case reflect.Bool:
			out.WriteString(strconv.FormatBool(v.Bool()))
		default:
			panic("unsupported fingerprint kind: " + v.Kind().String())
		}
		out.WriteByte('}')
	}
	var b bytes.Buffer
	encode(&b, reflect.ValueOf(value))
	return fmt.Sprintf("%x", sha256.Sum256(b.Bytes()))
}

func autoresearchNavigationCheck(tb testing.TB, c autoresearchNavigationCase) {
	tb.Helper()
	input := autoresearchNavigationFingerprint(struct {
		O CloudObservation
		P Profile
	}{c.observation, c.profile})
	g, e, err := BuildCloud(c.observation, c.bounds, c.profile)
	if err != nil {
		tb.Fatal(err)
	}
	if after := autoresearchNavigationFingerprint(struct {
		O CloudObservation
		P Profile
	}{c.observation, c.profile}); after != input {
		tb.Fatal("BuildCloud mutated observation or calibration")
	}
	got := autoresearchNavigationFingerprint(struct {
		Grid     *Grid
		Evidence CloudEvidence
	}{g, e})
	if got != c.want {
		tb.Fatalf("full navigation output fingerprint: got %s, want %s", got, c.want)
	}
}

func TestAutoresearchNavigationGolden(t *testing.T) {
	for _, c := range autoresearchNavigationCases() {
		t.Run(c.name, func(t *testing.T) { autoresearchNavigationCheck(t, c) })
	}
}

func TestAutoresearchNavigationRefusals(t *testing.T) {
	for name, corrupt := range map[string]func(*CloudObservation, *Profile){
		"source":                  func(o *CloudObservation, _ *Profile) { o.SourceDigest = "unverified" },
		"stamp":                   func(o *CloudObservation, _ *Profile) { o.CapturedUnixNS++ },
		"epoch":                   func(o *CloudObservation, _ *Profile) { o.Scan.WorldEpoch = 0 },
		"ray_count":               func(o *CloudObservation, _ *Profile) { o.Ranges = o.Ranges[:len(o.Ranges)-1] },
		"nan":                     func(o *CloudObservation, _ *Profile) { o.Ranges[0] = math.NaN() },
		"negative_infinity":       func(o *CloudObservation, _ *Profile) { o.Ranges[0] = math.Inf(-1) },
		"horizontal_disagreement": func(o *CloudObservation, _ *Profile) { o.Scan.Ranges[0] = 3 },
		"missing_pose":            func(o *CloudObservation, _ *Profile) { o.Scan.TiltedPose = nil },
		"missing_slab":            func(_ *CloudObservation, p *Profile) { p.TiltedScan = nil },
	} {
		t.Run(name, func(t *testing.T) {
			o, p := cloudFixture()
			corrupt(&o, &p)
			g, e, err := BuildCloud(o, area(), p)
			if !errors.Is(err, ErrInvalid) || g != nil || !reflect.DeepEqual(e, CloudEvidence{}) {
				t.Fatalf("invalid observation must return ErrInvalid and no usable grid/evidence: %v", err)
			}
		})
	}
}

var autoresearchNavigationGrid *Grid
var autoresearchNavigationEvidence CloudEvidence

func BenchmarkAutoresearchNavigation(b *testing.B) {
	for _, c := range autoresearchNavigationCases() {
		b.Run(c.name, func(b *testing.B) {
			autoresearchNavigationCheck(b, c)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				g, e, err := BuildCloud(c.observation, c.bounds, c.profile)
				if err != nil {
					b.Fatal(err)
				}
				autoresearchNavigationGrid, autoresearchNavigationEvidence = g, e
			}
		})
	}
}
