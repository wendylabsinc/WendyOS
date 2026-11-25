#ifndef WENDY_VISUAL_RECOGNITION_H
#define WENDY_VISUAL_RECOGNITION_H
#include <stdint.h>
#include <stddef.h>
#ifdef __cplusplus
extern "C" {
#endif
// The recognizer owns its templates. A caller serializes access to each handle.
void *WVRecognizerCreate(void);
void WVRecognizerDestroy(void *recognizer);
// gray is a top-left-origin 8-bit image. quad contains four clockwise corners,
// in pixels, of the physical reference plane. Zero physical size disables pose
// estimation, retaining appearance recognition without invented metric geometry.
int32_t WVRecognizerAdd(void *recognizer, const uint8_t *gray, int32_t width, int32_t height,
                       const double *quad, double physical_width, double physical_height);
typedef struct {
    int32_t reference_index, inliers, pose_valid;
    double score, rms_pixels;
    double corners[8]; // normalized top-left image coordinates
    double object_to_camera[16]; // column-major, optical right/down/forward
} WVDetection;
typedef struct {
    int32_t count, failed;
    int32_t reference_count, frame_keypoints, best_matches, best_inliers, stage;
    WVDetection items[8];
} WVDetections;
WVDetections WVRecognizerDetect(void *recognizer, const uint8_t *gray, int32_t width, int32_t height,
                               double fx, double fy, double cx, double cy);
#ifdef __cplusplus
}
#endif
#endif
