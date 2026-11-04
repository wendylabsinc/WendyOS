//go:build darwin && !ios

package main

/*
#include <stdlib.h>
#include "../Sources/Shared/SimulatorTypes.h"
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/macos/backend"
)

// Lifecycle calls cannot overlap. Pose reads use their own short-lived contexts.
var simulatorMutation sync.Mutex

//export WDSimulators
func WDSimulators() *C.WDSimulatorsResult {
	p := alloc[C.WDSimulatorsResult](1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	items, err := commands.DesktopSimulators().List(ctx)
	p.status = status(err)
	if err != nil {
		return p
	}
	p.count = C.size_t(len(items))
	p.items = alloc[C.WDSimulator](len(items))
	for i, item := range items {
		out := &unsafe.Slice(p.items, len(items))[i]
		out.name = C.CString(item.Name)
		out.profile = C.CString(item.Profile)
		out.state = C.CString(item.State)
		out.address = C.CString(item.Address)
		out.version = C.CString(item.Version)
		out.error = C.CString(item.Error)
		out.disk_bytes = C.int64_t(item.DiskBytes)
	}
	return p
}

//export WDSimulatorsFree
func WDSimulatorsFree(p *C.WDSimulatorsResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	for _, item := range unsafe.Slice(p.items, int(p.count)) {
		free(item.name)
		free(item.profile)
		free(item.state)
		free(item.address)
		free(item.version)
		free(item.error)
	}
	free(p.items)
	free(p)
}

//export WDSimulatorCreate
func WDSimulatorCreate(name, profile, image *C.char, disk C.int32_t) *C.WDStatus {
	if !simulatorMutation.TryLock() {
		return statusResult(fmt.Errorf("another simulator operation is in progress"))
	}
	defer simulatorMutation.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	_, err := commands.DesktopSimulators().Create(ctx, wendymcp.SimulatorCreateOptions{Name: C.GoString(name), Profile: C.GoString(profile), Image: C.GoString(image), DiskGiB: int(disk)})
	return statusResult(err)
}

//export WDSimulatorPerform
func WDSimulatorPerform(name *C.char, action C.WDSimulatorAction) *C.WDSimulatorOperationResult {
	p := alloc[C.WDSimulatorOperationResult](1)
	if !simulatorMutation.TryLock() {
		p.status = status(fmt.Errorf("another simulator operation is in progress"))
		return p
	}
	defer simulatorMutation.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	b := commands.DesktopSimulators()
	n := C.GoString(name)
	var err error
	switch action {
	case C.WDSimStart:
		_, err = b.Start(ctx, n)
	case C.WDSimStop:
		_, err = b.Stop(ctx, n, false, 60*time.Second)
	case C.WDSimDelete:
		err = b.Delete(ctx, n)
	case C.WDSimPause:
		err = backend.ControlSimulator(ctx, n, "pause")
	case C.WDSimResume:
		err = backend.ControlSimulator(ctx, n, "resume")
	case C.WDSimReset:
		err = backend.ControlSimulator(ctx, n, "reset")
	case C.WDSimUpdateRuntime:
		err = commands.DesktopUpdateSimulatorRuntime(ctx, n)
	case C.WDSimUpdateAgent:
		_, err = b.UpdateAgent(ctx, n)
	default:
		err = fmt.Errorf("unknown simulator action")
	}
	p.status = status(err)
	p.recovery = C.WDSimulatorRecovery(commands.DesktopSimulatorRecovery(err))
	return p
}

//export WDSimulatorOperationFree
func WDSimulatorOperationFree(p *C.WDSimulatorOperationResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p)
}

//export WDSimulatorScene
func WDSimulatorScene(name *C.char) *C.WDSimulatorSceneResult {
	p := alloc[C.WDSimulatorSceneResult](1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scene, err := backend.ReadSimulatorScene(ctx, C.GoString(name))
	p.status = status(err)
	if err != nil {
		return p
	}
	p.id = C.CString(scene.ID)
	p.robot_body = C.int32_t(scene.RobotBody)
	p.body_count = C.size_t(len(scene.Bodies))
	p.geom_count = C.size_t(len(scene.Geoms))
	p.geoms = alloc[C.WDSimulatorGeom](len(scene.Geoms))
	shapes := map[string]C.WDSimulatorShape{"plane": C.WDSimPlane, "box": C.WDSimBox, "sphere": C.WDSimSphere, "ellipsoid": C.WDSimEllipsoid, "cylinder": C.WDSimCylinder, "capsule": C.WDSimCapsule, "mesh": C.WDSimMesh}
	for i, g := range scene.Geoms {
		o := &unsafe.Slice(p.geoms, len(scene.Geoms))[i]
		o.body = C.int32_t(g.Body)
		o.mesh = C.int32_t(g.Mesh)
		o.shape = shapes[g.Type]
		for j := 0; j < 3; j++ {
			o.size[j] = C.float(g.Size[j])
			o.position[j] = C.float(g.Position[j])
		}
		for j := 0; j < 4; j++ {
			o.quaternion[j] = C.float(g.Quaternion[j])
			o.rgba[j] = C.float(g.RGBA[j])
		}
	}
	p.mesh_count = C.size_t(len(scene.Meshes))
	p.meshes = alloc[C.WDSimulatorMesh](len(scene.Meshes))
	for i, m := range scene.Meshes {
		o := &unsafe.Slice(p.meshes, len(scene.Meshes))[i]
		o.id = C.int32_t(m.ID)
		o.position_count = C.size_t(len(m.Positions))
		o.positions = alloc[C.float](len(m.Positions))
		for j, v := range m.Positions {
			unsafe.Slice(o.positions, len(m.Positions))[j] = C.float(v)
		}
		o.index_count = C.size_t(len(m.Indices))
		o.indices = alloc[C.uint32_t](len(m.Indices))
		for j, v := range m.Indices {
			unsafe.Slice(o.indices, len(m.Indices))[j] = C.uint32_t(v)
		}
	}
	return p
}

//export WDSimulatorSceneFree
func WDSimulatorSceneFree(p *C.WDSimulatorSceneResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.id)
	free(p.geoms)
	for _, m := range unsafe.Slice(p.meshes, int(p.mesh_count)) {
		free(m.positions)
		free(m.indices)
	}
	free(p.meshes)
	free(p)
}

//export WDSimulatorPose
func WDSimulatorPose(name *C.char) *C.WDSimulatorPoseResult {
	p := alloc[C.WDSimulatorPoseResult](1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pose, err := backend.ReadSimulatorPose(ctx, C.GoString(name))
	p.status = status(err)
	if err != nil {
		return p
	}
	p.scene_id = C.CString(pose.SceneID)
	p.mode = C.CString(pose.Mode)
	p.time = C.double(pose.Time)
	p.epoch = C.uint64_t(pose.Epoch)
	p.generation = C.uint64_t(pose.Generation)
	if pose.Valid {
		p.valid = 1
	}
	p.body_count = C.size_t(len(pose.Positions) / 3)
	p.positions = alloc[C.float](len(pose.Positions))
	p.quaternions = alloc[C.float](len(pose.Quaternions))
	for i, v := range pose.Positions {
		unsafe.Slice(p.positions, len(pose.Positions))[i] = C.float(v)
	}
	for i, v := range pose.Quaternions {
		unsafe.Slice(p.quaternions, len(pose.Quaternions))[i] = C.float(v)
	}
	return p
}

//export WDSimulatorPoseFree
func WDSimulatorPoseFree(p *C.WDSimulatorPoseResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.scene_id)
	free(p.mode)
	free(p.positions)
	free(p.quaternions)
	free(p)
}
