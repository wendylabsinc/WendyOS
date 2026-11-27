package spatialsim

import (
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Floors of the Office scenario. The yard shares the ground level.
const (
	FloorGround spatial.FloorID = "ground"
	FloorYard   spatial.FloorID = "yard"
	FloorUpper  spatial.FloorID = "upper"

	StoreyHeight = 3.5 // metres between the ground and upper floors
)

// Devices of the Office scenario.
const (
	Rover1    spatial.DeviceID = "rover-1"   // ground: corridor north lane, lobby, kitchen, office A
	Rover2    spatial.DeviceID = "rover-2"   // ground: corridor south lane, lab, storage; its lidar mount slips
	Rover3    spatial.DeviceID = "rover-3"   // ground: corridor centre lane end to end, office B, lobby
	Climber   spatial.DeviceID = "climber"   // up the stairs, upper lounge, lab and storage, down the lift
	Ranger    spatial.DeviceID = "ranger"    // corridor east end, out into the yard and back
	Latecomer spatial.DeviceID = "latecomer" // ground lab; powers on at OfficeLateStart
	Handheld  spatial.DeviceID = "handheld"  // camera only, carried round the upper office and meeting room

	CamLobby        spatial.DeviceID = "cam-lobby"      // with IMU
	CamCorridorWest spatial.DeviceID = "cam-corridor-w" // with IMU; bumped at OfficeCameraBump
	CamCorridorEast spatial.DeviceID = "cam-corridor-e"
	CamKitchen      spatial.DeviceID = "cam-kitchen"
	CamLab          spatial.DeviceID = "cam-lab"
	CamStorage      spatial.DeviceID = "cam-storage"
	CamYard         spatial.DeviceID = "cam-yard"
	CamUpper        spatial.DeviceID = "cam-upper" // upper lab; with IMU
	CamLate         spatial.DeviceID = "cam-late"  // office A; appears at OfficeLateCamera
)

// Timeline of the Office scenario, as offsets from the epoch. A patrol
// before OfficeChangesFrom and one after OfficeChangesUntil see the
// environment before and after every scheduled change.
const (
	OfficeDuration     = 600 * time.Second
	OfficeLateStart    = 120 * time.Second // Latecomer powers on
	OfficeLateCamera   = 150 * time.Second // CamLate announces itself
	OfficeChangesFrom  = 240 * time.Second // the static environment is constant before this
	OfficeChangesUntil = 300 * time.Second // and after this
	OfficeCameraBump   = 400 * time.Second // CamCorridorWest is knocked
	OfficeMountShift   = 420 * time.Second // Rover2's lidar mount slips
)

// Office builds the standard site: a two-storey building of about 30 x 20 m
// per floor with a fenced yard, seven mobile devices, nine fixed cameras,
// five microphone arrays, fourteen UWB beacons, two sound sources, a gas
// source, four actors and a schedule of changes and disturbances.
//
// Ground floor: an east-west corridor between a lobby, a kitchen and two
// identical offices on one side and a lab and a storage room on the other,
// with a lift and a staircase opening off it and an exterior door to the
// yard at its east end. Upper floor: a north-south corridor with a short
// stub, between an office, a meeting room and a lounge on one side and a lab
// and a storage room on the other.
//
// The two ground offices are the ambiguous region: same shape, same
// furniture, same door position, and plain wall opposite both doors.
//
// What the layout does and does not exercise:
//   - The lobby joins the ground corridor through a 4 m opening, wider than
//     the 3 m corridor, so a distance-transform watershed finds no saddle
//     there. Truth counts lobby and corridor as two rooms; a segmentation
//     test must say how it treats that pair.
//   - A lab holds benches (class "bench"), which no office does.
//   - Upstairs only the climber carries a lidar. It enters the lounge, the
//     lab and the storage room. The upper office is seen by lidar only
//     through its door and the meeting room only through its two doors; they
//     are the handheld's camera-only territory, to be localized against the
//     landmark map. Upper-floor map coverage, room segmentation and
//     multi-robot overlap are scored on the ground floor and on the upper
//     corridor, lounge, lab and storage.
//   - Three UWB beacons are in line of sight for most rounds only in the
//     ground corridor and the ground lab (Rover2, Rover3, Latecomer; see
//     TestUwbLineOfSightCoverage). Elsewhere most ranges cross a wall, read
//     long and say so in Range.LineOfSight.
//   - Every fixed camera sees robot markers across at least 0.4 of its image
//     width and height (TestCamerasObserveWhatFeaturesNeed). With robots
//     placed at the default Reference accuracy, the least-squares pose of a
//     camera from those markers lands 1 to 5 cm from truth in the median and
//     up to 10 cm for an unlucky seed (TestMarkerGeometryConstrainsFixedCameras):
//     the 5 cm target of feature 1 leaves room for little more than the
//     robot pose error.
func Office(seed uint64, opt Options) (*Site, error) {
	b, err := newBuilder("office", seed, opt)
	if err != nil {
		return nil, err
	}
	s := b.s
	s.duration = OfficeDuration
	lv0, lv1 := b.level(0), b.level(StoreyHeight)
	ground := b.floor(FloorGround, "Ground floor", lv0, false)
	yard := b.floor(FloorYard, "Yard", lv0, true)
	upper := b.floor(FloorUpper, "Upper floor", lv1, false)
	const door, opening = spatial.PortalDoor, spatial.PortalOpening
	sec := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

	ground.room("g-corridor", "corridor", 0, 8.5, 30, 11.5)
	ground.room("g-lobby", "lobby", 0, 0, 8, 8.5)
	ground.room("g-kitchen", "kitchen", 8, 0, 16, 8.5)
	ground.room("g-office-a", "office", 18, 0, 24, 8.5)
	ground.room("g-office-b", "office", 24, 0, 30, 8.5)
	ground.room("g-lab", "lab", 0, 11.5, 16, 20)
	ground.room("g-storage", "storage", 18, 11.5, 30, 20)
	ground.shaft("g-lift", "lift", "lift", 16, 6.5, 18, 8.5)
	ground.shaft("g-stairs", "stairs", "stairs", 16, 11.5, 18, 19)
	yard.room("yard", "yard", 30, 0, 44, 20)

	b.door(ground, "g-lobby-opening", opening, 2, 8.5, 6, 8.5, true)
	b.door(ground, "g-kitchen-door", door, 11.5, 8.5, 12.5, 8.5, true)
	b.door(ground, "g-office-a-door", door, 20.5, 8.5, 21.5, 8.5, true)
	b.door(ground, "g-office-b-door", door, 26.5, 8.5, 27.5, 8.5, true)
	b.door(ground, "g-lab-door", door, 7.4, 11.5, 8.6, 11.5, true)
	b.door(ground, "g-lab-east-door", door, 12.5, 11.5, 13.5, 11.5, true, sec(255))
	b.door(ground, "g-storage-door", door, 23.5, 11.5, 24.5, 11.5, true)
	b.door(ground, "g-entrance", door, 0, 3.6, 0, 4.8, false, sec(280))
	b.door(ground, "g-exterior-door", door, 30, 9.4, 30, 10.6, true)
	b.door(ground, "g-lift-door", opening, 16.4, 8.5, 17.6, 8.5, true)
	b.door(ground, "g-stairs-foot", opening, 16, 11.5, 18, 11.5, true)

	upper.poly("u-corridor", "corridor", []geom.Vec2{{X: 13, Z: 0}, {X: 16, Z: 0}, {X: 16, Z: 8.5}, {X: 18, Z: 8.5}, {X: 18, Z: 11.5}, {X: 16, Z: 11.5}, {X: 16, Z: 20}, {X: 13, Z: 20}})
	upper.room("u-office", "office", 0, 0, 13, 7)
	upper.room("u-meeting", "meeting", 0, 7, 13, 13)
	upper.room("u-lounge", "lounge", 0, 13, 13, 20)
	upper.room("u-lab", "lab", 18, 0, 30, 10)
	upper.room("u-storage", "storage", 18, 10, 30, 20)
	upper.shaft("u-lift", "lift", "lift", 16, 6.5, 18, 8.5)
	upper.shaft("u-stairs", "stairs", "stairs", 16, 11.5, 18, 19)

	b.door(upper, "u-office-door", door, 13, 3, 13, 4, true)
	b.door(upper, "u-meeting-door", door, 13, 9.5, 13, 10.5, true)
	b.door(upper, "u-lounge-door", door, 13, 16, 13, 17, true)
	b.door(upper, "u-lounge-meeting-door", door, 5, 13, 6, 13, true, sec(275))
	b.door(upper, "u-lab-door", door, 18, 8.8, 18, 9.8, true)
	b.door(upper, "u-storage-door", door, 18, 10.2, 18, 11.2, true)
	b.door(upper, "u-lift-door", opening, 16, 6.9, 16, 8.1, true)
	b.door(upper, "u-stairs-head", opening, 16, 17.5, 16, 19, true)

	b.stairsOrLift("stairs", spatial.PortalStairs, ground, "g-stairs", geom.Vec2{X: 17, Z: 12}, upper, "u-stairs", geom.Vec2{X: 17, Z: 17.5})
	b.stairsOrLift("lift", spatial.PortalLift, ground, "g-lift", geom.Vec2{X: 17, Z: 7.5}, upper, "u-lift", geom.Vec2{X: 17, Z: 7.5})
	s.ambiguity = &Ambiguity{Floor: FloorGround, Rooms: []string{"g-office-a", "g-office-b"}, Shift: geom.Vec2{X: 6}}

	// Furniture, kept clear of every route (see TestRoutesStayInFreeSpace).
	// The reception desk stands against the south wall so that the east side
	// of the lobby is free for rover-1.
	b.box(ground, "g-sofa", "sofa", 0.6, 6.8, 0.9, 2.0, 0, 0.8)
	b.box(ground, "g-reception", "desk", 4, 0.4, 2.0, 0.8, 0, 1.0)
	b.round(ground, "g-plant", "plant", 0.6, 0.6, 0.3, 1.2)
	b.box(ground, "g-stove", "stove", 9.0, 0.5, 1.0, 0.7, 0, 0.9)
	b.box(ground, "g-fridge", "fridge", 8.5, 4.0, 0.8, 0.8, 0, 1.8)
	table := b.round(ground, "g-table", "table", 14.3, 6.8, 0.6, 0.75)
	for _, office := range []struct {
		id string
		x  float64
	}{{"a", 18}, {"b", 24}} {
		b.box(ground, "g-desk-"+office.id, "desk", office.x+1.2, 1.0, 1.6, 0.8, 0, 0.75)
		b.box(ground, "g-shelf-"+office.id, "shelf", office.x+5.6, 4.0, 0.5, 2.0, 0, 1.8)
		b.round(ground, "g-chair-"+office.id, "chair", office.x+1.2, 2.0, 0.25, 0.9)
	}
	b.box(ground, "g-bench-1", "bench", 4, 19.4, 3, 0.8, 0, 0.9)
	b.box(ground, "g-bench-2", "bench", 11, 19.4, 3, 0.8, 0, 0.9)
	b.box(ground, "g-lab-shelf", "shelf", 0.4, 15, 0.6, 2, 0, 1.8)
	labCrate := b.box(ground, "g-lab-crate", "crate", 10.5, 15.6, 0.8, 0.8, 0.4, 0.8)
	b.box(ground, "g-store-shelf-1", "shelf", 19, 14, 0.5, 3, 0, 1.8)
	b.box(ground, "g-store-shelf-2", "shelf", 29.5, 15, 0.6, 4, 0, 1.8)
	storeCrate := b.box(ground, "g-store-crate", "crate", 19, 19, 1, 1, 0, 1.0)
	b.round(ground, "g-barrel", "barrel", 28.8, 19.2, 0.4, 1.0)
	b.box(yard, "y-crate", "crate", 43, 9.5, 1, 1, 0, 1.0)
	b.round(yard, "y-bin", "bin", 31, 18.8, 0.4, 1.1)
	yardCrate := b.box(yard, "y-crate-new", "crate", 37.5, 15, 1, 1, 0.3, 1.0)
	b.box(upper, "u-desk-1", "desk", 2, 1.5, 1.6, 0.8, 0, 0.75)
	b.box(upper, "u-desk-2", "desk", 8, 1.5, 1.6, 0.8, 0, 0.75)
	b.box(upper, "u-office-shelf", "shelf", 0.4, 5, 0.6, 2, 0, 1.8)
	b.box(upper, "u-table", "table", 3, 10, 2.4, 1.2, 0, 0.75)
	b.box(upper, "u-sofa", "sofa", 6, 19.4, 2, 0.8, 0, 0.8)
	b.box(upper, "u-bench", "bench", 24, 0.6, 4, 0.8, 0, 0.9)
	b.box(upper, "u-lab-shelf", "shelf", 29.6, 5, 0.6, 3, 0, 1.8)
	b.box(upper, "u-store-crate", "crate", 29, 19, 1, 1, 0, 1.0)
	b.box(upper, "u-store-shelf", "shelf", 24, 19.6, 4, 0.6, 0, 1.8)
	upperCrate := b.box(upper, "u-crate-new", "crate", 24.5, 17.5, 0.8, 0.8, 0, 0.8)

	// Changes, all inside [OfficeChangesFrom, OfficeChangesUntil]; the door
	// toggles at 255, 275 and 280 s are declared with the doors above.
	b.appear(labCrate, sec(250))
	b.remove(storeCrate, sec(260))
	b.appear(yardCrate, sec(265))
	b.move(table, sec(270), 9.5, 7.0, 0)
	b.appear(upperCrate, sec(285))

	g := func(x, z float64) wp { return wp{geom.Vec2{X: x, Z: z}, 0} }
	u := func(x, z float64) wp { return wp{geom.Vec2{X: x, Z: z}, StoreyHeight} }

	// Three ground robots in three corridor lanes. Their loops overlap along
	// the corridor and each closes on itself. Rover-1 and rover-3 both enter
	// the lobby, north and east of where the two people walk, so cam-lobby
	// sees markers from two robots across its image.
	b.robot(Rover1, 0, 0, 1.2,
		g(4, 9.3), g(2.6, 7.3), g(6, 6.5), g(7.4, 6.2), g(7.4, 2.2), g(7.4, 6.2), g(4, 9.3), g(12, 9.3), g(12, 6), g(10, 3), g(14, 3), g(12, 6), g(12, 9.3),
		g(21, 9.3), g(21, 6), g(19.5, 3), g(22.5, 3), g(21, 6), g(21, 9.3))
	rover2 := b.robot(Rover2, 1, 0, 1.1,
		g(8, 10.7), g(8, 13.5), g(13, 14), g(13, 17.5), g(8.5, 17.5), g(8, 13.5), g(8, 10.7),
		g(24, 10.7), g(24, 13.5), g(21, 17), g(27, 17), g(24, 13.5), g(24, 10.7))
	b.shiftLidar(rover2, OfficeMountShift, geom.Pose2{X: rover2.lidarMount[0].X + 0.03, Z: rover2.lidarMount[0].Z - 0.02, Yaw: rover2.lidarMount[0].Yaw + 0.06})
	b.robot(Rover3, 2, 0, 1.0,
		g(2, 10), g(27, 10), g(27, 6), g(25.5, 3), g(28.5, 3), g(27, 6), g(27, 10),
		g(5, 10), g(5, 7.6), g(2.2, 6.6), g(2.2, 5), g(2.2, 6.6), g(3, 10))
	// Up the stairs, through the lounge, round the lab and the storage room,
	// down the lift: the closing leg from the last waypoint to the first is the
	// lift ride. The wide lab loop crosses the whole view of cam-upper.
	b.robot(Climber, 3, 0, 1.0,
		g(17, 7.5), g(17, 12), u(17, 17.5), u(17, 18.25), u(15.3, 18.25), u(15.3, 16.5),
		u(11, 16.5), u(6, 15), u(6, 18), u(9, 18), u(11, 16.5), u(15.3, 16.5), u(15.3, 10),
		u(17, 9.3), u(20, 9.3), u(20, 3), u(24, 2.5), u(28, 3), u(28, 8), u(23, 5.5), u(20, 9.3), u(17, 9.3),
		u(17, 10.7), u(20, 10.7), u(22, 15), u(27, 15), u(20, 10.7), u(17, 10.7),
		u(15.3, 10), u(15.3, 7.5), u(17, 7.5))
	// The ranger passes close under cam-yard along the west fence before its
	// loop round the far side of the yard.
	b.robot(Ranger, 4, 0, 1.1,
		g(28.7, 11), g(28.7, 10), g(32, 10), g(31, 8.5), g(31, 3.5), g(31, 8.5), g(33, 12), g(42, 12), g(42, 18), g(33, 18), g(33, 12), g(32, 10), g(28.7, 10), g(28.7, 9))
	b.robot(Latecomer, 5, OfficeLateStart, 1.0, g(2, 13), g(5.5, 13), g(5.5, 17.5), g(2, 17.5))
	// The handheld keeps to the office, the meeting room and the stretch of
	// corridor between their doors, where no lidar goes.
	b.handheld(Handheld, 0, 1.0,
		u(13.7, 2), u(13.7, 3.5), u(6, 3.5), u(6, 5.5), u(11, 5.5), u(11, 3.5), u(13.7, 3.5),
		u(13.7, 10), u(6, 10), u(6, 11.8), u(11, 11.8), u(11, 10), u(13.7, 10))

	v := func(x, y, z float64) geom.Vec3 { return geom.Vec3{X: x, Y: y, Z: z} }
	b.camera(CamLobby, ground, 0, true, v(0.3, 2.7, 8.2), v(5, 0.5, 3.5))
	west := b.camera(CamCorridorWest, ground, 0, true, v(0.3, 2.7, 10), v(10, 0.3, 10))
	b.bump(west, OfficeCameraBump, v(0.03, 0.045, -0.02), v(0.02, -0.015, 0.01))
	b.camera(CamCorridorEast, ground, 0, false, v(29.7, 2.7, 11.2), v(22, 0.3, 9.2))
	b.camera(CamKitchen, ground, 0, false, v(15.7, 2.6, 0.3), v(11, 0.4, 5))
	b.camera(CamLab, ground, 0, false, v(0.3, 2.7, 19.7), v(7, 0.4, 15))
	b.camera(CamStorage, ground, 0, false, v(18.3, 2.7, 19.7), v(22, 0.4, 15.5))
	b.camera(CamYard, yard, 0, false, v(30.3, 3, 0.4), v(37, 0.4, 8))
	b.camera(CamUpper, upper, 0, true, v(18.3, 2.7, 0.3), v(24, 0.3, 6))
	b.camera(CamLate, ground, OfficeLateCamera, false, v(18.3, 2.6, 0.3), v(21.5, 0.4, 4.5))

	b.mic("mic-1", ground, 1, 10, 0)
	b.mic("mic-2", ground, 15, 8.8, math.Pi/2)
	b.mic("mic-3", ground, 29, 11.2, math.Pi)
	b.mic("mic-4", ground, 0.5, 12, 0.6)
	b.mic("mic-5", ground, 15.5, 19.5, -2.4)
	b.sound("alarm", ground, 18.5, 2.0, 11.2, Interval{sec(30), sec(90)}, Interval{sec(330), sec(390)})
	b.sound("compressor", ground, 11, 1.0, 19.2, Interval{sec(100), sec(220)})
	s.gas = []GasSource{{ID: "stove-leak", Floor: FloorGround, Position: v(9, 0.9, 0.6), PeakPPM: 800, SpreadM: 4}}

	b.beacon("beacon-01", ground, 0.4, 8.9)
	b.beacon("beacon-02", ground, 29.6, 11.1)
	b.beacon("beacon-03", ground, 14, 8.8)
	b.beacon("beacon-04", ground, 0.4, 19.6)
	b.beacon("beacon-05", ground, 15.6, 0.4)
	b.beacon("beacon-06", ground, 29.6, 19.6)
	b.beacon("beacon-07", yard, 43.6, 0.4)
	b.beacon("beacon-08", yard, 43.6, 19.6)
	b.beacon("beacon-09", upper, 14.5, 0.4)
	b.beacon("beacon-10", upper, 14.5, 19.6)
	b.beacon("beacon-11", upper, 29.6, 9.6)
	b.beacon("beacon-12", ground, 15.6, 11.9)
	b.beacon("beacon-13", ground, 8, 19.6)
	b.beacon("beacon-14", ground, 8.4, 8.1)

	// Two people walking equal overlapping rectangles in opposite senses in
	// the lobby, and a person crossing a cart's loop in the yard. Each pair
	// shares a period, so the paths cross and occlude without collisions, and
	// the phases bring the bodies within a few decimetres of touching once per
	// period (see TestActorsPassClose), which is what makes identity switches
	// possible for a tracker.
	person := Shape{Radius: 0.25, Height: 1.7}
	xz := func(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }
	b.actor("person-1", "person", ground, 15, 0, person, xz(1.5, 1.2), xz(5, 1.2), xz(5, 4.2), xz(1.5, 4.2))
	b.actor("person-2", "person", ground, 15, 6.55, person, xz(3, 2.4), xz(3, 5.4), xz(6.5, 5.4), xz(6.5, 2.4))
	b.actor("person-3", "person", yard, 20, 7, person, xz(32, 5), xz(42, 5))
	b.actor("cart-1", "cart", yard, 40, 0.5, Shape{Half: geom.Vec2{X: 0.5, Z: 0.3}, Height: 1.0}, xz(33, 3), xz(41, 3), xz(41, 7), xz(33, 7))

	return b.build()
}
