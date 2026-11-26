// Package occupancy is the distributed occupancy map of the spatial layer:
// tiled log-odds maps per floor that many devices build and exchange without
// coordination.
//
// State. A Replica holds layers. A layer is the evidence one source (a
// device) has for one tile of one floor: Config.TileSide squared cells of
// quantized log-odds, a version, the floor elevation and the scans it rests
// on. A source only ever rewrites its own layers, with a higher version each
// time.
//
// Merge. Layers of one key are totally ordered by (version, content tag) and
// merging keeps the greatest, so merge is a join: commutative, associative
// and idempotent. Any two replicas that have received the same set of layers,
// in any order and any number of times, hold the same state and report the
// same Digest. The content tag only matters when a source breaks the contract
// and writes two different layers with one version; replicas still converge.
//
// Fusion. The map a reader sees is, cell by cell, the sum of the layers of
// every source, clamped once to the range of Config.Grid. The sum is exact
// integer arithmetic, so the fused tile is a function of the state alone.
// A fused cell whose evidence cancels exactly is unknown, as in package grid.
// Each source's own map forgets what it later sees gone, but the sum keeps
// the last word of a source that never returns (see Replica.Tile).
//
// Sync. Replica.Summary lists the version of every layer; Replica.Delta
// answers a peer's summary with exactly the layers the peer lacks or holds
// older. Because applying a layer is idempotent and order-free, duplicated,
// reordered and dropped messages are harmless: a lost layer is in the next
// answer. On the wire a layer is a spatial.MapTile component whose version
// vector names one source (Layer.Component, Replica.ApplyComponent); it is a
// source-owned register that world.Replica orders the same way.
//
// Mapping. A Mapper is one source. It integrates the lidar scans of its
// device at poses another feature estimated, with the device's calibrated
// lidar mount and clock offset, and Commit writes the tiles that changed as
// new layer versions. It refuses scans it cannot place: no calibration, no
// pose bracketing the scan, a pose in another frame or one too uncertain.
// Stats reports what was refused.
//
// Tiles are spatial.MapTile at grid.TileLogOddsScale. With side at most
// spatial.MaxTileSide the raw cells are within spatial.MaxPayloadBytes, so
// they are not compressed. Conventions are those of package geom: the map is
// World (x, z), positive yaw is clockwise seen from above.
//
// Replica and Mapper are not safe for concurrent use.
package occupancy
