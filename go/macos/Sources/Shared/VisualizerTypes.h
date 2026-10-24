#ifndef WENDY_VISUALIZER_TYPES_H
#define WENDY_VISUALIZER_TYPES_H

typedef struct { char *id, *name, *state; int64_t started_ns, size_bytes; } WDVisualEpisode;
typedef struct { WDStatus status; WDVisualEpisode *items; size_t count; } WDVisualEpisodesResult;
typedef struct { char *id, *name, *kind, *detail; } WDVisualTrack;
// local_path is owned by the result and removed on free. Move the file to
// retain it beyond WDVisualSnapshotFree; the original path is never reused.
typedef struct { char *track; char *local_path; int64_t *timestamps; size_t timestamp_count; } WDVisualVideo;
typedef struct {
    char *track, *label, *detail, *unit;
    int64_t timestamp_ns;
    double value;
    int32_t has_value;
    uint8_t *image; size_t image_count;
} WDVisualSample;
typedef struct {
    WDStatus status;
    char *clock, *boot_id, *warnings;
    int64_t start_ns, end_ns;
    WDVisualTrack *tracks; size_t track_count;
    WDVisualSample *samples; size_t sample_count;
    WDVisualVideo *videos; size_t video_count;
} WDVisualSnapshotResult;

WDVisualEpisodesResult *WDVisualEpisodes(uint64_t session);
void WDVisualEpisodesFree(WDVisualEpisodesResult *value);
WDVisualSnapshotResult *WDVisualEpisodeLoad(uint64_t session, char *episode);
void WDVisualSnapshotFree(WDVisualSnapshotResult *value);
#endif
