package anchors

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// patch is one view of an anchor's surroundings: a keyframe scan and the
// anchor's pose in that scan's frame. It needs nothing else to be matched.
type patch struct {
	anchor geom.Pose2
	scan   spatial.CompactScan
}

// A signature is version, patch count, then per patch: beam count (uint16),
// the anchor pose in the scan frame and AngleMin, AngleIncrement, RangeMaxM
// (six float64), and the ranges in the CompactScan encoding (uint16
// millimetres). Little-endian.
const (
	signatureVersion = 1
	patchHeaderBytes = 2 + 6*8
)

// encodeSignature packs the patches into at most spatial.MaxSignatureBytes,
// keeping every k-th beam of a scan that would not fit its share.
func encodeSignature(patches []patch) ([]byte, error) {
	if len(patches) == 0 || len(patches) > 255 {
		return nil, fmt.Errorf("%w: %d patches", ErrSignature, len(patches))
	}
	maxBeams := ((spatial.MaxSignatureBytes-2)/len(patches) - patchHeaderBytes) / 2
	if maxBeams < 1 {
		return nil, fmt.Errorf("%w: %d patches do not fit", ErrSignature, len(patches))
	}
	out := []byte{signatureVersion, byte(len(patches))}
	for _, p := range patches {
		stride := (len(p.scan.RangesMM) + maxBeams - 1) / maxBeams
		if stride < 1 {
			return nil, fmt.Errorf("%w: empty scan", ErrSignature)
		}
		beams := (len(p.scan.RangesMM) + stride - 1) / stride
		out = binary.LittleEndian.AppendUint16(out, uint16(beams))
		for _, v := range []float64{p.anchor.X, p.anchor.Z, p.anchor.Yaw, p.scan.AngleMin, p.scan.AngleIncrement * float64(stride), p.scan.RangeMaxM} {
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v))
		}
		for i := 0; i < len(p.scan.RangesMM); i += stride {
			out = binary.LittleEndian.AppendUint16(out, p.scan.RangesMM[i])
		}
	}
	return out, nil
}

func decodeSignature(b []byte) ([]patch, error) {
	if len(b) < 2 || b[0] != signatureVersion || b[1] == 0 {
		return nil, fmt.Errorf("%w: header", ErrSignature)
	}
	patches := make([]patch, 0, b[1])
	b = b[2:]
	for range cap(patches) {
		if len(b) < patchHeaderBytes {
			return nil, fmt.Errorf("%w: truncated", ErrSignature)
		}
		beams := int(binary.LittleEndian.Uint16(b))
		var f [6]float64
		for i := range f {
			f[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[2+8*i:]))
			if math.IsNaN(f[i]) || math.IsInf(f[i], 0) {
				return nil, fmt.Errorf("%w: non-finite value", ErrSignature)
			}
		}
		b = b[patchHeaderBytes:]
		if beams == 0 || len(b) < 2*beams || f[4] == 0 || !(f[5] > 0) {
			return nil, fmt.Errorf("%w: scan", ErrSignature)
		}
		p := patch{anchor: geom.Pose2{X: f[0], Z: f[1], Yaw: f[2]}, scan: spatial.CompactScan{AngleMin: f[3], AngleIncrement: f[4], RangeMaxM: f[5], RangesMM: make([]uint16, beams)}}
		for i := range p.scan.RangesMM {
			p.scan.RangesMM[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
		b = b[2*beams:]
		patches = append(patches, p)
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", ErrSignature)
	}
	return patches, nil
}
