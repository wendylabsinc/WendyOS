"""Loopback MCP endpoint; Wendy's authenticated agent proxies access to this app."""
import json
import signal
import threading


def create_server(node):
    from mcp.server.fastmcp import FastMCP
    from mcp.types import ToolAnnotations, CallToolResult, TextContent, ImageContent
    import base64
    mcp = FastMCP("RobotNavigation", host="127.0.0.1", port=8128, stateless_http=True,
        instructions="Use robot_status before navigation. Select people only by fresh robot_observe target IDs. "
        "Navigation runs locally with a finite lease. Poll navigation_status; renew explicitly only while "
        "supervising the requested goal. Cancellation remains stopping until fresh odometry confirms rest "
        "and the navigation action has ended. Never substitute raw velocity commands for a rejected goal.")
    readonly = ToolAnnotations(readOnlyHint=True, destructiveHint=False, openWorldHint=False)
    motion = ToolAnnotations(readOnlyHint=False, destructiveHint=True, idempotentHint=True, openWorldHint=True)

    @mcp.tool(annotations=readonly)
    def robot_status() -> dict:
        """Report sensor freshness, coverage, pose, motion feedback, guard and navigation blockers."""
        return node.robot_status()

    @mcp.tool(annotations=readonly)
    def robot_targets() -> dict:
        """List expiring person target IDs grounded using synchronized calibrated RGB-D and TF."""
        return node.perception.robot_targets()

    @mcp.tool(annotations=readonly, structured_output=False)
    def robot_observe() -> CallToolResult:
        """Return person targets and their matching labeled camera image, when fresh and available."""
        metadata, jpeg = node.perception.observation()
        content = [TextContent(type="text", text=json.dumps(metadata, allow_nan=False))]
        if jpeg is not None:
            content.append(ImageContent(type="image", mimeType="image/jpeg", data=base64.b64encode(jpeg).decode("ascii")))
        return CallToolResult(content=content)

    @mcp.tool(annotations=motion)
    def navigation_goal(request_id: str, x: float, y: float, yaw: float, frame_id: str,
                        max_speed: float = .15, lease_seconds: float = 10.0,
                        timeout_seconds: float = 120.0) -> dict:
        """Submit an observed floor position to Nav2. Reuse request_id only for identical retries.

        Coordinates are metres/radians in robot_status's navigation frame. The local planner
        must find a route through observed free space; submission does not imply acceptance
        or arrival. Expired leases stop the goal. Device speed/distance limits cannot be raised.
        """
        return node.runtime.navigate(request_id, {"x": x, "y": y, "yaw": yaw, "frame_id": frame_id},
            max_speed, lease_seconds=lease_seconds, timeout_seconds=timeout_seconds)

    @mcp.tool(annotations=readonly)
    def navigation_status(goal_id: str) -> dict:
        """Read goal progress and confirmed stopping. This does not renew the supervision lease."""
        return node.runtime.status(goal_id)

    @mcp.tool(annotations=ToolAnnotations(readOnlyHint=False, destructiveHint=True, openWorldHint=True))
    def navigation_renew(goal_id: str) -> dict:
        """Explicitly extend supervision of an active requested goal within its original timeout."""
        return node.runtime.renew(goal_id)

    @mcp.tool(annotations=motion)
    def navigation_cancel(goal_id: str) -> dict:
        """Cancel locally and inhibit velocity; poll until stopped_confirmed is true."""
        return node.runtime.cancel(goal_id)

    @mcp.tool(annotations=motion)
    def robot_stop() -> dict:
        """Cancel the goal and latch the local velocity guard. Reset requires local operator action."""
        node.runtime.stop()
        node.backend.latch_stop()
        return {"stop_requested": True, **node.runtime.status()}

    @mcp.tool(annotations=motion)
    def approach_person(request_id: str, target_id: str, standoff: float = 1.5,
                        max_speed: float = .15, lease_seconds: float = 10.0) -> dict:
        """Approach a selected fresh target with at least the requested physical standoff in metres.

        The endpoint includes footprint, pose/depth uncertainty, target drift and goal tolerance.
        Unknown calibration/depth, ambiguous or stale targets reject the request. Target loss
        or excess movement stops the goal; it never switches to another person automatically.
        """
        return node.approach(request_id, target_id, standoff, max_speed, lease_seconds)

    return mcp


def main():
    import rclpy
    import uvicorn
    from .settings import load_settings
    from .ros import create_node
    rclpy.init()
    node = create_node(load_settings())
    mcp = create_server(node)
    server = uvicorn.Server(uvicorn.Config(mcp.streamable_http_app(), host="127.0.0.1", port=8128, log_level="info"))
    worker = threading.Thread(target=server.run, daemon=True, name="mcp-http")
    worker.start()
    stopping = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stopping.set())
    signal.signal(signal.SIGINT, lambda *_: stopping.set())
    try:
        while rclpy.ok() and not stopping.is_set():
            if not worker.is_alive():
                raise RuntimeError("MCP server exited")
            rclpy.spin_once(node, timeout_sec=.1)
    finally:
        node.runtime.close()
        node.enabled = False
        # Independent guard and motor expiry also cover executor/process failure.
        from std_msgs.msg import String
        node.permit_pub.publish(String(data='{"enabled":false,"max_speed":0.0}'))
        node.stop_pub.publish(String(data='{"latch":false}'))
        server.should_exit = True
        worker.join(timeout=2)
        node.destroy_node()
        rclpy.try_shutdown()


if __name__ == "__main__":
    main()
