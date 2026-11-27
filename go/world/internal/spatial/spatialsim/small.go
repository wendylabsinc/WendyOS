package spatialsim

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// SmallDuration is the scripted length of the small scenarios.
const SmallDuration = 120 * time.Second

func small(scenario string, seed uint64, opt Options) (*builder, *floor, error) {
	b, err := newBuilder(scenario, seed, opt)
	if err != nil {
		return nil, nil, err
	}
	b.s.duration = SmallDuration
	return b, b.floor(FloorGround, "Ground floor", b.level(0), false), nil
}

// SingleRoom is one 8 x 6 m room "room" with a desk and a barrel, the robot
// "rover-1" circling in it, the fixed camera "cam-1" with an IMU in a corner
// and the UWB beacons "beacon-1" and "beacon-2".
func SingleRoom(seed uint64, opt Options) (*Site, error) {
	b, f, err := small("single-room", seed, opt)
	if err != nil {
		return nil, err
	}
	f.room("room", "office", 0, 0, 8, 6)
	b.box(f, "desk", "desk", 6.5, 0.9, 1.6, 0.8, 0, 0.75)
	b.round(f, "barrel", "barrel", 0.7, 5.3, 0.3, 1.0)
	b.robot(Rover1, 0, 0, 1.0, wp{geom.Vec2{X: 2, Z: 2.5}, 0}, wp{geom.Vec2{X: 6, Z: 2.5}, 0}, wp{geom.Vec2{X: 6, Z: 4.5}, 0}, wp{geom.Vec2{X: 2, Z: 4.5}, 0})
	b.camera("cam-1", f, 0, true, geom.Vec3{X: 0.3, Y: 2.6, Z: 0.3}, geom.Vec3{X: 4.5, Y: 0.4, Z: 3.5})
	b.beacon("beacon-1", f, 7.6, 0.4)
	b.beacon("beacon-2", f, 0.4, 5.6)
	return b.build()
}

// Corridor is a featureless 60 x 2.4 m corridor "corridor". The robot
// "rover-1" shuttles along its middle 16 m, where both ends stay beyond the
// default 20 m lidar range, so its position along the corridor is
// unobservable from lidar: Truth.Ambiguity is continuous along +X.
func Corridor(seed uint64, opt Options) (*Site, error) {
	b, f, err := small("corridor", seed, opt)
	if err != nil {
		return nil, err
	}
	f.room("corridor", "corridor", 0, 0, 60, 2.4)
	b.robot(Rover1, 0, 0, 1.0, wp{geom.Vec2{X: 22, Z: 1.2}, 0}, wp{geom.Vec2{X: 38, Z: 1.2}, 0})
	b.s.ambiguity = &Ambiguity{Floor: FloorGround, Rooms: []string{"corridor"}, Shift: geom.Vec2{X: 1}, Continuous: true}
	return b.build()
}

// TwoRooms is two identical 6 x 5 m rooms "room-a" and "room-b" side by
// side, with identical furniture and closed doors onto a shared corridor.
// "rover-1" circles in room A and "rover-2" follows the same loop in room B,
// six metres along +X, so their scans differ only by noise.
func TwoRooms(seed uint64, opt Options) (*Site, error) {
	b, f, err := small("two-rooms", seed, opt)
	if err != nil {
		return nil, err
	}
	f.room("corridor", "corridor", 0, 5, 12, 7.5)
	for i, name := range []string{"a", "b"} {
		x := 6 * float64(i)
		f.room("room-"+name, "office", x, 0, x+6, 5)
		b.door(f, "door-"+name, spatial.PortalDoor, x+2.5, 5, x+3.5, 5, false)
		b.box(f, "desk-"+name, "desk", x+1.2, 0.8, 1.6, 0.8, 0, 0.75)
		b.round(f, "chair-"+name, "chair", x+5.2, 4.2, 0.25, 0.9)
		b.robot(spatial.DeviceID([]string{"rover-1", "rover-2"}[i]), 0, 0, 1.0,
			wp{geom.Vec2{X: x + 1.5, Z: 2.2}, 0}, wp{geom.Vec2{X: x + 4.5, Z: 2.2}, 0}, wp{geom.Vec2{X: x + 4.5, Z: 3.8}, 0}, wp{geom.Vec2{X: x + 1.5, Z: 3.8}, 0})
	}
	b.s.ambiguity = &Ambiguity{Floor: FloorGround, Rooms: []string{"room-a", "room-b"}, Shift: geom.Vec2{X: 6}}
	return b.build()
}
