#ifndef WENDY_MODEL_LIBRARY_TYPES_H
#define WENDY_MODEL_LIBRARY_TYPES_H
typedef struct { char *id, *name, *description, *license; int64_t downloads; } WDModelRepository;
typedef struct { WDStatus status; WDModelRepository *items; size_t count; } WDModelSearchResult;
typedef struct { char *name, *quantization, *unavailable_reason; int64_t bytes; } WDModelFile;
typedef struct { WDStatus status; char *revision, *license; WDModelFile *items; size_t count; } WDModelFilesResult;
typedef enum { WD_MODEL_CPU, WD_MODEL_NVIDIA, WD_MODEL_JETPACK5, WD_MODEL_JETPACK6 } WDModelAccelerator;
typedef struct { WDStatus status; char *model, *progress, *error; int32_t done; uint32_t port; } WDModelRunResult;
typedef struct { WDStatus status; char *os, *architecture, *gpu, *jetpack, *warning; int64_t memory_total, memory_available, disk_free, gpu_total, gpu_available; int32_t disk_known, cuda, shared_memory; } WDModelHardwareResult;
WDModelHardwareResult *WDModelHardware(uint64_t session);
void WDModelHardwareFree(WDModelHardwareResult *value);
WDModelSearchResult *WDSearchModels(char *query);
void WDModelSearchFree(WDModelSearchResult *value);
WDModelFilesResult *WDModelFiles(char *repository);
void WDModelFilesFree(WDModelFilesResult *value);
WDStatus *WDRunModel(uint64_t session, uint64_t job, uint32_t port, WDModelAccelerator accelerator, char *repository, char *file, char *revision, int32_t install_runtime, int32_t accept_capacity_warning);
WDModelRunResult *WDModelRunStatus(uint64_t session, uint64_t job);
void WDModelRunFree(WDModelRunResult *value);
WDStatus *WDCancelModelRun(uint64_t session, uint64_t job);
#endif
