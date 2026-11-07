package episodeexport

import (
	"bytes"
	"errors"
)

// SnapshotMovie wraps one archived, self-contained Annex-B camera snapshot for
// native image generators. Its single display interval is not episode timing.
func SnapshotMovie(unit []byte) ([]byte, error) {
	if len(unit) == 0 || len(unit) > 4<<20 {
		return nil, errors.New("snapshot exceeds preview limit")
	}
	if bytes.Count(unit, []byte{0, 0, 1}) > 4096 {
		return nil, errors.New("too many snapshot NAL units")
	}
	var sps, pps, body []byte
	for _, nal := range splitAnnexB(unit) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 31 {
		case 7:
			sps = nal
		case 8:
			pps = nal
		case 5:
			body = append(body, u32(uint32(len(nal)))...)
			body = append(body, nal...)
		case 1:
			return nil, errors.New("snapshot is not a single keyframe")
		}
	}
	if len(body) == 0 || len(sps) < 4 || len(sps) > 65535 || len(pps) == 0 || len(pps) > 65535 {
		return nil, errors.New("incomplete H.264 snapshot")
	}
	w, h, err := spsDimensions(sps)
	if err != nil || w <= 0 || h <= 0 || w > 8192 || h > 8192 || w*h > 32_000_000 {
		return nil, errors.New("unsupported snapshot dimensions")
	}
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2avc1mp41"))
	moov, _, err := buildMoov([]sample{{offset: int64(len(ftyp) + 8), size: int64(len(body)), sync: true}}, sps, pps, w, h)
	if err != nil {
		return nil, err
	}
	return concat(ftyp, box("mdat", body), moov), nil
}
