# Paired Go2 data and commands (experimental)

Pairing by itself sends only validation and heartbeat messages, never movement. The
`Connection` methods send sport requests on `rt/api/sport/request`. Velocity
updates use `policy.noreply`, matching Unitree's SportClient.Move. Other requests
wait for a reply with a matching request ID, API ID and status code:

| Method | Sport API |
| --- | --- |
| `Move(forward, lateral, yaw)` | Move (1008); zero velocity sends StopMove |
| `Stop` | StopMove (1003) |
| `StandUp` | RecoveryStand (1006) |
| `LieDown` | StandDown (1005) |
| `Hello` | Hello (1016) |

`Move` accepts only finite velocities within the Go2 simulator's limits: forward ±0.8 m/s,
lateral ±0.5 m/s and yaw ±1.0 rad/s. The physical Go2 ignores forward requests below about 0.5 m/s.
Requests are limited to that list: no mode, gait or arbitrary request APIs. The wire format follows
`legion1581/unitree_webrtc_connect` at `e0abc5780761539eff89a382a79319e5cf6ad1f4`.

`Connection.Subscribe(topics, video)` streams robot topics (for example
`rt/lf/lowstate`, `rt/utlidar/voxel_map_compressed`) exactly as received, and
H.264 access units from the front camera. Text messages keep their JSON envelope;
binary LiDAR messages keep their framing. The robot is asked to send a topic
(`subscribe`), LiDAR (`rt/utlidar/switch` and traffic saving off) and video
(`vid on`) only while at least one subscriber wants it, and is told to stop
when the last one leaves. Slow subscribers lose messages instead of delaying
the connection.

`WendyUnitreeRobotService.Stream` exposes that subscription, and
`WendyUnitreeRobotService.Command` sends one of the commands above over the agent's
existing paired connection. It never logs in, never dials the robot and never exposes the robot
key. It is registered only on the authenticated mTLS listener and the local admin
socket, and it reuses the pairing-owner scope check. Each robot request has a 200 ms
budget. A MOVE stays in effect for 500 ms: unless another MOVE refreshes it, the agent sends
StopMove, so the robot stops when the caller disappears. Any other command cancels a
pending move. If a MOVE fails, the agent attempts StopMove.

These are software limits, not an emergency stop. If Stop cannot reach the robot
(host crash, Wi-Fi loss), the robot can keep moving, so keep the vendor remote at hand.

```sh
cd go
go test -mod=readonly -race ./internal/shared/unitree ./internal/agent/services \
  -run 'TestTeleop|TestUnitreeRobot|TestTopics|TestBinaryTopic|TestSubscribe' -count=1
```
