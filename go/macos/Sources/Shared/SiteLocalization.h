#ifndef WENDY_SITE_LOCALIZATION_H
#define WENDY_SITE_LOCALIZATION_H
#include <stdint.h>
#ifdef __cplusplus
extern "C" {
#endif
// Each landmark is xyz in the scan's metric world, then 128 SIFT descriptor floats.
#define WS_LANDMARK_FLOATS 131
#define WS_MAX_LANDMARKS 12000
#define WS_MIN_POSE_INLIERS 12
#define WS_MIN_ESTIMATED_INLIERS 8
#define WS_MIN_LENS_INLIERS 16
// Alignment tolerances are relative to a 640-pixel long image edge. Returned
// RMS values and intrinsics remain in the original capture's pixel units.
#define WS_ALIGNMENT_REFERENCE_SIZE 640
void *WSMapCreate(void);
void WSMapDestroy(void *map);
int32_t WSMapCount(void *map);
int32_t WSMapAdd(void *map, const uint8_t *gray, int32_t width, int32_t height,
    const float *depth, const uint8_t *confidence, int32_t depth_width, int32_t depth_height,
    const double *intrinsics4, const double *optical_to_world16);
int32_t WSMapExport(void *map, float *landmarks, int32_t capacity);
int32_t WSMapImport(void *map, const float *landmarks, int32_t count);
typedef struct {
    int32_t success, reason, matches, inliers, fitted_lens;
    double fx, fy, cx, cy, distortion[5], rms, world_from_camera[16];
} WSLocalization;
// fitted_lens: 0 = existing measured lens, 1 = measured joint fit, 2 = approximate fit.
// known_lens: fx,fy,cx,cy,k1,k2,p1,p2,k3, or null for joint calibration.
WSLocalization WSLocalize(void *map, const uint8_t *gray, int32_t width, int32_t height, const double *known_lens);
WSLocalization WSSolveCamera(const double *xyz, const double *uv, int32_t count, int32_t width, int32_t height, const double *known_lens);
WSLocalization WSLocalizeWithAssumedLens(void *map, const uint8_t *gray, int32_t width, int32_t height, const double *assumed_lens);
WSLocalization WSLocalizeEstimated(void *map, const uint8_t *gray, int32_t width, int32_t height);
WSLocalization WSSolveCameraEstimated(const double *xyz, const double *uv, int32_t count, int32_t width, int32_t height);
void *WSScreenLensCreate(void);
void WSScreenLensDestroy(void *calibration);
int32_t WSScreenLensAdd(void *calibration, const uint8_t *gray, int32_t width, int32_t height);
// Screen calibration failure reasons, separate from room-localization reasons.
enum {
 WS_SCREEN_LENS_VIEWS = 1, WS_SCREEN_LENS_COVERAGE = 3,
 WS_SCREEN_LENS_FIT = 4, WS_SCREEN_LENS_VALIDATION = 5,
 WS_SCREEN_LENS_INTERNAL = 6, WS_SCREEN_LENS_TILT = 7,
 WS_SCREEN_LENS_UNCERTAIN = 8, WS_SCREEN_LENS_DISAGREEMENT = 9
};
WSLocalization WSScreenLensSolve(void *calibration);
typedef struct {
 int32_t count, detection, coverage_ready;
 uint64_t revision;
 double x, y, width, height, coverage, min_x, max_x, min_y, max_y;
} WSScreenLensProgress;
WSScreenLensProgress WSScreenLensGetProgress(void *calibration);
#ifdef __cplusplus
}
#endif
#endif
