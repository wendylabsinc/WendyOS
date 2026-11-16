#ifndef WENDY_VISUALIZER_TYPES_H
#define WENDY_VISUALIZER_TYPES_H

typedef struct { char *id, *name, *state; int64_t started_ns, size_bytes; } WDVisualEpisode;
typedef struct { WDStatus status; WDVisualEpisode *items; size_t count; } WDVisualEpisodesResult;
typedef struct { char *id, *name, *kind, *detail; } WDVisualTrack;
// local_path is owned by the result and removed on free. Move the file to
// retain it beyond WDVisualSnapshotFree; the original path is never reused.
typedef struct { char *track; char *local_path; int64_t *timestamps; size_t timestamp_count; } WDVisualVideo;
typedef struct { float x, y, z; } WDVisualPoint;
typedef struct { char *track, *local_path; int64_t start_ns, end_ns; } WDVisualAudio;
typedef struct {
    char *track, *label, *detail, *unit;
    int64_t timestamp_ns;
    double value;
    int32_t has_value;
    uint8_t *image; size_t image_count;
    WDVisualPoint *points; size_t point_count;
} WDVisualSample;
typedef struct {
    WDStatus status;
    char *clock, *boot_id, *warnings;
    int64_t start_ns, end_ns;
    WDVisualTrack *tracks; size_t track_count;
    WDVisualSample *samples; size_t sample_count;
    WDVisualVideo *videos; size_t video_count;
    WDVisualAudio *audio; size_t audio_count;
} WDVisualSnapshotResult;

typedef struct { WDStatus status; char *local_path; } WDVisualExportResult;
WDVisualExportResult *WDVisualEpisodeExport(uint64_t session, char *episode);
void WDVisualExportFree(WDVisualExportResult *value);
typedef struct { WDStatus status; char *local_path, *reason; double maximum_celsius, duration; int32_t has_maximum; } WDVisualPreviewResult;
WDVisualPreviewResult *WDVisualPreviewAt(char *selector, char *episode);
void WDVisualPreviewFree(WDVisualPreviewResult *value);

typedef enum { WD_RECORDING_STATUS, WD_RECORDING_START, WD_RECORDING_STOP } WDVisualRecordingAction;
WDVisualEpisodesResult *WDHomeCampaign(uint64_t session, char *name, int32_t trigger);
WDVisualEpisodesResult *WDVisualRecording(uint64_t session, WDVisualRecordingAction action, char *name, char **sources, size_t source_count);
WDVisualEpisodesResult *WDVisualEpisodes(uint64_t session);
WDVisualEpisodesResult *WDVisualEpisodesAt(char *selector);
void WDVisualEpisodesFree(WDVisualEpisodesResult *value);
WDVisualSnapshotResult *WDVisualEpisodeLoad(uint64_t session, char *episode);
WDVisualSnapshotResult *WDVisualEpisodeLoadAt(char *selector, char *episode);
void WDVisualSnapshotFree(WDVisualSnapshotResult *value);
typedef struct {
    char *id, *title, *trigger, *kind, *upload_state, *topics, *preview_file, *campaign;
    uint64_t *preview_counts; size_t preview_count;
    int64_t started_ns, size_bytes;
    double duration_seconds;
    int32_t details_loaded;
} WDRecordingEpisode;
typedef struct {
    WDStatus status;
    WDRecordingEpisode *items; size_t count;
    char *warning, *model;
} WDRecordingLibraryResult;
typedef struct { WDStatus status; char **paths; size_t count; } WDRecordingExportsResult;
uint64_t WDBeginRecordingRead(void);
void WDEndRecordingRead(uint64_t request);
WDRecordingLibraryResult *WDRecordingLibrary(uint64_t request, char *selector, int32_t secure);
WDRecordingLibraryResult *WDRecordingDetails(uint64_t request, char *selector, int32_t secure, char **episodes, size_t count);
void WDRecordingLibraryFree(WDRecordingLibraryResult *value);
typedef struct { WDStatus status; char *format; uint8_t *bytes; size_t count; } WDRecordingThumbnailResult;
WDRecordingThumbnailResult *WDRecordingThumbnail(uint64_t request, char *selector, int32_t secure, char *episode, char *file);
void WDRecordingThumbnailFree(WDRecordingThumbnailResult *value);
WDVisualSnapshotResult *WDRecordingPlay(uint64_t request, char *selector, int32_t secure, char *episode);
WDRecordingExportsResult *WDRecordingExport(uint64_t request, char *selector, int32_t secure, char **episodes, size_t count);
void WDRecordingExportsFree(WDRecordingExportsResult *value);

typedef struct { WDStatus status; char *json; } WDGalleryMetadataResult;
WDGalleryMetadataResult *WDGalleryMetadataAt(char *selector, char *episode);
void WDGalleryMetadataFree(WDGalleryMetadataResult *value);

WDVisualSnapshotResult *WDVisualGalleryDataAt(char *selector, char *episode, int summary);

WDVisualSnapshotResult *WDRecordingData(uint64_t request, char *selector, int32_t secure, char *episode, int32_t mode);
#endif
