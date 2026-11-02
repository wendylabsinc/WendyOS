#ifndef WENDY_SIMULATOR_TYPES_H
#define WENDY_SIMULATOR_TYPES_H
#include "BridgeTypes.h"
typedef struct { char *name, *profile, *state, *address, *version, *error; int64_t disk_bytes; } WDSimulator;
typedef struct { WDStatus status; WDSimulator *items; size_t count; } WDSimulatorsResult;
typedef struct { int32_t id; float *positions; size_t position_count; uint32_t *indices; size_t index_count; } WDSimulatorMesh;
typedef enum { WDSimPlane, WDSimBox, WDSimSphere, WDSimEllipsoid, WDSimCylinder, WDSimCapsule, WDSimMesh } WDSimulatorShape;
typedef struct { int32_t body, mesh; WDSimulatorShape shape; float size[3], position[3], quaternion[4], rgba[4]; } WDSimulatorGeom;
typedef struct { WDStatus status; char *id; int32_t robot_body; size_t body_count; WDSimulatorGeom *geoms; size_t geom_count; WDSimulatorMesh *meshes; size_t mesh_count; } WDSimulatorSceneResult;
typedef struct { WDStatus status; char *scene_id, *mode; float *positions, *quaternions; size_t body_count; double time; uint64_t epoch, generation; int32_t valid; } WDSimulatorPoseResult;
typedef enum { WDSimStart, WDSimStop, WDSimDelete, WDSimPause, WDSimResume, WDSimReset, WDSimUpdateRuntime, WDSimUpdateAgent } WDSimulatorAction;
typedef enum { WDSimRecoveryNone, WDSimRecoveryRuntime, WDSimRecoveryAgent } WDSimulatorRecovery;
typedef struct { WDStatus status; WDSimulatorRecovery recovery; } WDSimulatorOperationResult;
WDSimulatorsResult *WDSimulators(void);
void WDSimulatorsFree(WDSimulatorsResult *value);
WDStatus *WDSimulatorCreate(char *name, char *profile, char *image, int32_t disk_gib);
WDSimulatorOperationResult *WDSimulatorPerform(char *name, WDSimulatorAction action);
void WDSimulatorOperationFree(WDSimulatorOperationResult *value);
WDSimulatorSceneResult *WDSimulatorScene(char *name);
void WDSimulatorSceneFree(WDSimulatorSceneResult *value);
WDSimulatorPoseResult *WDSimulatorPose(char *name);
void WDSimulatorPoseFree(WDSimulatorPoseResult *value);
#endif
