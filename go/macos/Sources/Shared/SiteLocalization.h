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
#define WS_MIN_LENS_INLIERS 16
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
WSLocalization WSScreenLensSolve(void *calibration);
#ifdef __cplusplus
}
#endif
#endif
