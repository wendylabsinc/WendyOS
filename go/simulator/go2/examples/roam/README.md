# Go2 roaming example

This app uses the [shared native Go2 interfaces](../README.md) on hardware and
in the simulator. The source manifest uses host discovery; managed VM deployment
normalizes it to guest loopback. The Docker launch enables the temporary clock and scan-gap options described below. Use a CLI built from this checkout.


This small application walks the virtual Go2 around its room, stops before
obstacles, and turns toward open space using lidar and odometry. **Start** and
**Stop** control the roaming behavior. The controller limits its velocity
requests and stops when observations become stale or unusable. Fresh sensor
data alone cannot restart a stopped controller.

Use the ROS app with physical Go2 robots or the Wendy Go2 simulator. It is a reactive obstacle-avoidance
example; it does not build a map or plan routes.

## Run beside the local browser preview

From this directory, with the simulator running at `http://127.0.0.1:8898`:

```sh
python3 app.py --simulator http://127.0.0.1:8898
```

Open `http://127.0.0.1:8901` for the application's Start/Stop controls, status,
and embedded 3D sandbox. Use **Follow robot** to keep it in view, or drag to
orbit and pan while it explores.
Start the application explicitly when ready to roam. Start acquires this local
application's simulator control lease; Stop requests zero velocity and releases
it, leaving the simulator available for inspection. `--port` changes the
application page's port if 8901 is already in use.

If manual controls already own the robot, choose **Release controls** in the
original sandbox tab before starting. If that tab is no longer available,
**Pause** then **Resume** in the sandbox releases the old owner. Then choose
**Start exploring** in the application.

## Deploy the ROS application

The ROS adapter reads `/utlidar/cloud_base` (`sensor_msgs/msg/PointCloud2`) and `/utlidar/robot_odom`
(`nav_msgs/msg/Odometry`), then publishes velocity requests on `/api/sport/request`
(`unitree_api/msg/Request`) at 20 Hz. It uses the same controller as the local
application. Its observations and commands travel through ROS; the deployed
image contains no simulator or physics implementation.

From this directory:

```sh
wendy run --device Woof --build-type docker --no-restart
# Or: wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

The supplied manifest selects ROS 2 Humble, CycloneDDS, domain 0 and the Go2 VM's
native Go2 ROS bus. The Docker image runs `ros_app.py --autostart --ignore-capture-age --allow-scan-gaps`, which starts the
controller once accepted lidar and odometry arrive. The managed Go2 simulator
automatically gives its new publisher control, replacing the previous driving
app or browser controller. Run the command with the world running to start
exploring. Open the sandbox to watch; no **Give app control** click is needed.

In a ROS-enabled shell on the same VM bus, use the Trigger services to control
the behavior and inspect its status:

```sh
ros2 service call /roam/start std_srvs/srv/Trigger '{}'
ros2 service call /roam/stop std_srvs/srv/Trigger '{}'
ros2 topic echo /roam/status std_msgs/msg/String
```

Without `--autostart`, `python3 ros_app.py` publishes zero velocity until a
successful `/roam/start` request. A start request needs current observations.
Status includes the controller state, stop reason, clearance, velocity requests
and sensor ages. `/roam/stop` immediately requests zero velocity; **Release app
control** in the simulator also removes the application's command ownership.

The adapter projects body-frame clouds in `base_link` and odometry from `odom` to
`base_link`. It rejects invalid poses, wrong frames, reordered observations and
invalid measurements. `--ignore-capture-age` uses local arrival time for the
350 ms timeout, so the board clock does not need synchronization. Replayed
captures still fail admission. Delayed captures arriving now cannot be detected.

`--allow-scan-gaps` permits missing returns within a sector, but requires at least
one measured front return before driving and measured side/sweep clearance before
turning. Empty sectors needed for motion still block that motion. Obstacle
thresholds use the closest measured returns. Obstacles in gaps can be missed.
`/roam/status` includes `allow_scan_gaps`, `ignore_capture_age`, `scan_coverage`
and `capture_age_seconds`. Remove either flag to restore its strict check.
The local preview also accepts `app.py --allow-scan-gaps`; its simulator freshness
checks remain active because they use the simulator's local observation contract.

After stale observations, issue a new Start request once sensors recover.
After a simulator pause, world reset or **Release app control**, resume the
world and then restart the ROS application. Its new publisher receives control
automatically. Starting the app while paused does not defer its grant until
resume. Existing publishers cannot automatically take control back from a
newer app; restart Roam to return control to it.

## Check the controller, local runner and ROS observation admission

From this directory, using the simulator's development environment:

```sh
../../.venv/bin/python -m pytest -q test_controller.py test_app.py test_ros_app.py
```

These checks run without ROS. The deployed adapter uses the Humble packages
installed by the Dockerfile and needs no additional pip dependencies.
