from pathlib import Path
from http.client import HTTPConnection
import json
import threading

import pytest

from coke_demo.service import DemoRuntime, make_server


def ready(ros=False):
    runtime = DemoRuntime(Path(__file__).resolve().parents[1], ros_enabled=ros)
    runtime.state["phase"] = "idle"
    return runtime


def test_duplicate_run_does_not_restart_episode():
    runtime = ready()
    assert runtime.request("run", {"mode": "policy", "steps": 40})["accepted"]
    with pytest.raises(ValueError, match="already active"):
        runtime.request("run", {"mode": "expert", "steps": 2400})
    assert runtime.commands.qsize() == 1


def test_run_reservation_survives_worker_snapshot_race():
    runtime = ready()
    runtime.request("run", {"steps": 40})
    # The worker can finish a previous snapshot after HTTP queues a start.
    runtime._update(phase="idle")
    with pytest.raises(ValueError, match="already active"):
        runtime.request("run", {"steps": 40})


@pytest.mark.parametrize("steps", [0, -1, 8000, True, 3.5, "40"])
def test_invalid_run_length_never_enqueues(steps):
    runtime = ready()
    with pytest.raises(ValueError, match="integer"):
        runtime.request("run", {"steps": steps})
    assert runtime.commands.empty()
    assert runtime.state["phase"] == "idle"


def test_local_preview_does_not_claim_ros_control():
    runtime = ready()
    with pytest.raises(ValueError, match="ROS"):
        runtime.request("run", {"mode": "ros"})


def test_policy_and_expert_keep_their_own_timing():
    policy, expert = ready(), ready()
    policy.request("run", {"mode": "policy"})
    expert.request("run", {"mode": "expert"})
    assert policy.commands.get_nowait()[1]["steps"] == 7518
    assert expert.commands.get_nowait()[1]["steps"] == 2400


def test_hil_requires_configuration_and_uses_original_policy_timing():
    runtime = ready()
    with pytest.raises(ValueError, match="COKE_POLICY_URL"):
        runtime.request("run", {"mode": "hil"})
    runtime = DemoRuntime(Path(__file__).resolve().parents[1], ros_enabled=False,
                          policy_url="http://jetson:8098")
    runtime.state["phase"] = "idle"
    assert runtime.status()["hil_enabled"]
    runtime.request("run", {"mode": "hil"})
    assert runtime.commands.get_nowait()[1] == {"mode": "hil", "steps": 7518}


def test_resume_requires_paused_episode():
    runtime = ready()
    with pytest.raises(ValueError, match="paused"):
        runtime.request("resume", {})
    runtime.state["phase"] = "paused"
    assert runtime.request("resume", {})["accepted"]


def test_http_polling_reuses_connection_and_rejects_unread_command_body():
    runtime = ready()
    server = make_server(runtime, port=0)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    client = HTTPConnection(*server.server_address, timeout=2)
    try:
        client.request("GET", "/api/status")
        response = client.getresponse()
        assert response.status == 200
        assert response.version == 11
        assert json.loads(response.read())["phase"] == "idle"
        connection = client.sock
        assert connection is not None
        client.request("GET", "/api/session")
        assert json.loads(client.getresponse().read())["token"] == runtime.token
        assert client.sock is connection
        # An unauthorized body must not become the next keep-alive request.
        client.request("POST", "/api/run", body='{"steps":40}')
        response = client.getresponse()
        assert response.status == 403
        assert response.getheader("Connection") == "close"
        response.read()
        assert client.sock is None
        assert runtime.commands.empty()
    finally:
        client.close()
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)
