package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/macos/backend"
)

//export WDBeginRecordingRead
func WDBeginRecordingRead() C.uint64_t { return C.uint64_t(desktop.BeginRecordingRead()) }

//export WDEndRecordingRead
func WDEndRecordingRead(request C.uint64_t) { desktop.EndRecordingRead(uint64(request)) }

//export WDRecordingLibrary
func WDRecordingLibrary(request C.uint64_t, selector *C.char, secure C.int32_t) *C.WDRecordingLibraryResult {
	return recordingLibraryResult(desktop.RecordingLibrary(uint64(request), C.GoString(selector), secure != 0))
}

//export WDRecordingDetails
func WDRecordingDetails(request C.uint64_t, selector *C.char, secure C.int32_t, episodes **C.char, count C.size_t) *C.WDRecordingLibraryResult {
	if episodes == nil || count == 0 || count > backend.RecordingDetailBatchLimit {
		return recordingLibraryResult(backend.RecordingLibrary{}, errors.New("invalid recording detail batch"))
	}
	ids := make([]string, count)
	for i, episode := range unsafe.Slice(episodes, int(count)) {
		ids[i] = C.GoString(episode)
	}
	return recordingLibraryResult(desktop.RecordingDetails(uint64(request), C.GoString(selector), secure != 0, ids))
}
func recordingLibraryResult(lib backend.RecordingLibrary, err error) *C.WDRecordingLibraryResult {
	p := alloc[C.WDRecordingLibraryResult](1)
	p.status = status(err)
	p.warning, p.model = C.CString(lib.Warning), C.CString(lib.Model)
	p.items, p.count = alloc[C.WDRecordingEpisode](len(lib.Episodes)), C.size_t(len(lib.Episodes))
	for i, r := range lib.Episodes {
		row := &unsafe.Slice(p.items, len(lib.Episodes))[i]
		*row = C.WDRecordingEpisode{id: C.CString(r.ID), title: C.CString(r.Title), trigger: C.CString(r.Trigger), kind: C.CString(r.Kind), upload_state: C.CString(r.UploadState), topics: C.CString(strings.Join(r.Topics, "\n")), started_ns: C.int64_t(r.Started), size_bytes: C.int64_t(r.Bytes), duration_seconds: C.double(r.Duration)}
		if r.Campaign != "" {
			row.campaign = C.CString(r.Campaign)
		}
		if r.PreviewFile != nil {
			descriptor, _ := json.Marshal(r.PreviewFile)
			row.preview_file = C.CString(string(descriptor))
		}
		if len(r.PreviewCounts) > 0 {
			row.preview_counts = alloc[C.uint64_t](len(r.PreviewCounts))
			row.preview_count = C.size_t(len(r.PreviewCounts))
			for j, count := range r.PreviewCounts {
				unsafe.Slice(row.preview_counts, len(r.PreviewCounts))[j] = C.uint64_t(count)
			}
		}
		if r.DetailsLoaded {
			row.details_loaded = 1
		}
	}
	return p
}

//export WDRecordingLibraryFree
func WDRecordingLibraryFree(p *C.WDRecordingLibraryResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.id)
		free(r.title)
		free(r.trigger)
		free(r.kind)
		free(r.upload_state)
		free(r.topics)
		free(r.preview_counts)
		free(r.preview_file)
		free(r.campaign)
	}
	free(p.warning)
	free(p.model)
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDRecordingThumbnail
func WDRecordingThumbnail(request C.uint64_t, selector *C.char, secure C.int32_t, episode, descriptor *C.char) *C.WDRecordingThumbnailResult {
	p := alloc[C.WDRecordingThumbnailResult](1)
	var file data.File
	if err := json.Unmarshal([]byte(C.GoString(descriptor)), &file); err != nil {
		p.status = status(err)
		return p
	}
	payload, err := desktop.RecordingThumbnail(uint64(request), C.GoString(selector), secure != 0, C.GoString(episode), file)
	p.status = status(err)
	if err == nil {
		p.format = C.CString(backend.RecordingPreviewFormat(file))
		p.bytes = (*C.uint8_t)(C.CBytes(payload))
		p.count = C.size_t(len(payload))
	}
	return p
}

//export WDRecordingThumbnailFree
func WDRecordingThumbnailFree(p *C.WDRecordingThumbnailResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.format)
	free(p.bytes)
	free(p)
}

//export WDRecordingPlay
func WDRecordingPlay(request C.uint64_t, selector *C.char, secure C.int32_t, episode *C.char) *C.WDVisualSnapshotResult {
	return visualSnapshotResult(desktop.PlayRecording(uint64(request), C.GoString(selector), secure != 0, C.GoString(episode)))
}

//export WDRecordingExport
func WDRecordingExport(request C.uint64_t, selector *C.char, secure C.int32_t, episodes **C.char, count C.size_t) *C.WDRecordingExportsResult {
	p := alloc[C.WDRecordingExportsResult](1)
	if count == 0 || episodes == nil {
		p.status = status(errors.New("choose recordings to export"))
		return p
	}
	ids := make([]string, 0, int(count))
	for _, e := range unsafe.Slice(episodes, int(count)) {
		ids = append(ids, C.GoString(e))
	}
	paths, err := desktop.ExportRecordings(uint64(request), C.GoString(selector), secure != 0, ids)
	p.status = status(err)
	p.paths, p.count = alloc[*C.char](len(paths)), C.size_t(len(paths))
	for i, file := range paths {
		unsafe.Slice(p.paths, len(paths))[i] = C.CString(file)
	}
	return p
}

//export WDRecordingExportsFree
func WDRecordingExportsFree(p *C.WDRecordingExportsResult) {
	if p == nil {
		return
	}
	for _, file := range unsafe.Slice(p.paths, int(p.count)) {
		_ = os.Remove(C.GoString(file))
		free(file)
	}
	free(p.paths)
	free(p.status.message)
	free(p)
}
