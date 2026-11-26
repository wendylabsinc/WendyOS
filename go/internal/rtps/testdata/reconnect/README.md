# Fast DDS publisher replacement regression

This opt-in test retains one Wendy RTPS participant while a real ROS 2
publisher process is replaced. It receives four raw image topics, unsubscribes
the old writers, and requires two samples from each of eight replacement raw
and RGB image topics. Each phase allows 45 seconds for discovery and delivery.
Normal `go test ./...` does not run this fixture.

Run in an isolated Linux environment with ROS 2 Jazzy, Fast DDS, `rclpy` and
`sensor_msgs` installed and sourced. Pass the Python executable and publisher
script as arguments. For example, from the WendyOS module root on ARM64:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/reconnect ./go/internal/rtps/testdata/reconnect
docker run --rm -e ROS_DOMAIN_ID=42 \
  -e ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST \
  -v /tmp/reconnect:/reconnect:ro \
  -v "$PWD/go/internal/rtps/testdata/reconnect/publisher.py:/publisher.py:ro" \
  ccr-thermal-color-preview /reconnect /app/.venv/bin/python /publisher.py
```

The tested image `ccr-thermal-color-preview` is a local build of
`wendylabsinc/ccr-thermal-demo` commit `a72f46d`, including its ROS environment
entrypoint. Substitute an equivalent ROS image and Python path as needed;
choose the binary architecture to match that image.

Before the subscription-history repair, the initial phase receives 4/4 topics
but replacement discovers 8/8 and receives 0/8. With the repair, both phases
receive every topic without recreating Wendy's participant.
