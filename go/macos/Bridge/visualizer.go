package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"os"
	"strings"
	"unsafe"
)

//export WDVisualEpisodes
func WDVisualEpisodes(session C.uint64_t) *C.WDVisualEpisodesResult {
	p := alloc[C.WDVisualEpisodesResult](1)
	rows, err := desktop.VisualEpisodes(uint64(session))
	p.status = status(err)
	p.items, p.count = alloc[C.WDVisualEpisode](len(rows)), C.size_t(len(rows))
	for i, r := range rows {
		unsafe.Slice(p.items, len(rows))[i] = C.WDVisualEpisode{id: C.CString(r.GetId()), name: C.CString(r.GetName()), state: C.CString(r.GetState()), started_ns: C.int64_t(r.GetStartedUnixNanos()), size_bytes: C.int64_t(r.GetSizeBytes())}
	}
	return p
}

//export WDVisualEpisodesFree
func WDVisualEpisodesFree(p *C.WDVisualEpisodesResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.id)
		free(r.name)
		free(r.state)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDVisualEpisodeLoad
func WDVisualEpisodeLoad(session C.uint64_t, episode *C.char) *C.WDVisualSnapshotResult {
	p := alloc[C.WDVisualSnapshotResult](1)
	snapshot, err := desktop.VisualEpisode(uint64(session), C.GoString(episode))
	p.status = status(err)
	if err != nil || snapshot == nil {
		return p
	}
	p.clock, p.boot_id, p.warnings = C.CString(snapshot.Clock), C.CString(snapshot.BootID), C.CString(strings.Join(snapshot.Warnings, "\n"))
	p.start_ns, p.end_ns = C.int64_t(snapshot.Start), C.int64_t(snapshot.End)
	p.tracks, p.track_count = alloc[C.WDVisualTrack](len(snapshot.Tracks)), C.size_t(len(snapshot.Tracks))
	for i, r := range snapshot.Tracks {
		unsafe.Slice(p.tracks, len(snapshot.Tracks))[i] = C.WDVisualTrack{id: C.CString(r.ID), name: C.CString(r.Name), kind: C.CString(r.Kind), detail: C.CString(r.Detail)}
	}
	p.samples, p.sample_count = alloc[C.WDVisualSample](len(snapshot.Samples)), C.size_t(len(snapshot.Samples))
	for i, r := range snapshot.Samples {
		s := &unsafe.Slice(p.samples, len(snapshot.Samples))[i]
		s.track, s.label, s.detail, s.unit = C.CString(r.Track), C.CString(r.Label), C.CString(r.Detail), C.CString(r.Unit)
		s.timestamp_ns, s.value = C.int64_t(r.Time), C.double(r.Value)
		if r.HasValue {
			s.has_value = 1
		}
		s.image, s.image_count = alloc[C.uint8_t](len(r.Image)), C.size_t(len(r.Image))
		copy(unsafe.Slice((*byte)(unsafe.Pointer(s.image)), len(r.Image)), r.Image)
		s.points, s.point_count = alloc[C.WDVisualPoint](len(r.Points)), C.size_t(len(r.Points))
		for j, point := range r.Points {
			unsafe.Slice(s.points, len(r.Points))[j] = C.WDVisualPoint{x: C.float(point.X), y: C.float(point.Y), z: C.float(point.Z)}
		}
	}
	p.audio, p.audio_count = alloc[C.WDVisualAudio](len(snapshot.Audio)), C.size_t(len(snapshot.Audio))
	for i, r := range snapshot.Audio {
		unsafe.Slice(p.audio, len(snapshot.Audio))[i] = C.WDVisualAudio{track: C.CString(r.Track), local_path: C.CString(r.Path), start_ns: C.int64_t(r.Start), end_ns: C.int64_t(r.End)}
	}
	p.videos, p.video_count = alloc[C.WDVisualVideo](len(snapshot.Videos)), C.size_t(len(snapshot.Videos))
	for i, r := range snapshot.Videos {
		v := &unsafe.Slice(p.videos, len(snapshot.Videos))[i]
		v.track = C.CString(r.Track)
		v.local_path = C.CString(r.Path)
		v.timestamps, v.timestamp_count = alloc[C.int64_t](len(r.Times)), C.size_t(len(r.Times))
		for j, t := range r.Times {
			unsafe.Slice(v.timestamps, len(r.Times))[j] = C.int64_t(t)
		}
	}
	return p
}

//export WDVisualSnapshotFree
func WDVisualSnapshotFree(p *C.WDVisualSnapshotResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.tracks, int(p.track_count)) {
		free(r.id)
		free(r.name)
		free(r.kind)
		free(r.detail)
	}
	for _, r := range unsafe.Slice(p.samples, int(p.sample_count)) {
		free(r.track)
		free(r.label)
		free(r.detail)
		free(r.unit)
		free(r.image)
		free(r.points)
	}
	for _, v := range unsafe.Slice(p.videos, int(p.video_count)) {
		free(v.track)
		if v.local_path != nil {
			_ = os.Remove(C.GoString(v.local_path))
		}
		free(v.local_path)
		free(v.timestamps)
	}
	for _, a := range unsafe.Slice(p.audio, int(p.audio_count)) {
		free(a.track)
		if a.local_path != nil {
			_ = os.Remove(C.GoString(a.local_path))
		}
		free(a.local_path)
	}
	free(p.audio)
	free(p.videos)
	free(p.tracks)
	free(p.samples)
	free(p.clock)
	free(p.boot_id)
	free(p.warnings)
	free(p.status.message)
	free(p)
}

//export WDVisualEpisodeExport
func WDVisualEpisodeExport(session C.uint64_t, episode *C.char) *C.WDVisualExportResult {
	p := alloc[C.WDVisualExportResult](1)
	filename, err := desktop.ExportVisualEpisode(uint64(session), C.GoString(episode))
	p.status = status(err)
	p.local_path = C.CString(filename)
	return p
}

//export WDVisualExportFree
func WDVisualExportFree(p *C.WDVisualExportResult) {
	if p == nil {
		return
	}
	if p.local_path != nil {
		_ = os.Remove(C.GoString(p.local_path))
	}
	free(p.local_path)
	free(p.status.message)
	free(p)
}

//export WDVisualRecording
func WDVisualRecording(session C.uint64_t, action C.WDVisualRecordingAction, name *C.char, sources **C.char, count C.size_t) *C.WDVisualEpisodesResult {
	p := alloc[C.WDVisualEpisodesResult](1)
	var row *pb.DataEpisode
	var err error
	switch action {
	case C.WD_RECORDING_STATUS:
		row, err = desktop.VisualRecordingStatus(uint64(session))
	case C.WD_RECORDING_STOP:
		row, err = desktop.StopVisualRecording(uint64(session))
	case C.WD_RECORDING_START:
		if count == 0 || count > 256 || sources == nil {
			err = errors.New("choose recording sources")
			break
		}
		ids := make([]string, 0, int(count))
		for _, id := range unsafe.Slice(sources, int(count)) {
			ids = append(ids, C.GoString(id))
		}
		row, err = desktop.StartVisualRecording(uint64(session), C.GoString(name), ids)
	default:
		err = errors.New("invalid recording action")
	}
	p.status = status(err)
	if row != nil {
		p.items = alloc[C.WDVisualEpisode](1)
		p.count = 1
		*p.items = C.WDVisualEpisode{id: C.CString(row.GetId()), name: C.CString(row.GetName()), state: C.CString(row.GetState()), started_ns: C.int64_t(row.GetStartedUnixNanos()), size_bytes: C.int64_t(row.GetSizeBytes())}
	}
	return p
}
