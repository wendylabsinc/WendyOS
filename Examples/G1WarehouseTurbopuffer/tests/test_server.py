import json
import threading
import urllib.request

from warehouse import server
from warehouse.simulation import Simulation


def test_server_contract_matches_wendy_json():
    config = json.loads((server.STATIC_ROOT.parents[1] / "wendy.json").read_text())
    ports = {e.get("port") for e in config["entitlements"] if e["type"] == "http"}
    assert server.PORT == 8880
    assert ports == {8880}
    assert config["readiness"]["tcpSocket"]["port"] == 8880
    # the key comes from the deploying shell at `wendy run` time and is never stored
    assert config["env"]["TURBOPUFFER_API_KEY"] == "${TURBOPUFFER_API_KEY}"
    for name, _ in server.STATIC_FILES.values():
        assert (server.STATIC_ROOT / name).is_file(), name


def test_scene_meshes_state_and_health_round_trip(monkeypatch):
    monkeypatch.delenv("TURBOPUFFER_API_KEY", raising=False)
    simulation = Simulation()
    simulation.run_headless(0.1)
    httpd = server.DemoServer(("127.0.0.1", 0), simulation)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{httpd.server_address[1]}"
    try:
        health = json.loads(urllib.request.urlopen(f"{base}/api/health", timeout=5).read())
        assert health["ready"] is True
        assert health["memory"] == "Keyword stand-in"
        scene = json.loads(urllib.request.urlopen(f"{base}/api/scene", timeout=5).read())
        blob = urllib.request.urlopen(f"{base}/api/meshes.bin", timeout=5).read()
        last = scene["meshes"][-1]
        assert len(blob) == last["indexOffset"] + 4 * last["indexCount"]
        assert len(scene["boxes"]) == 7 + 6   # the boxes the robot moves, then the top-shelf stock
        assert "pelvis" in scene["bodies"]
        state = json.loads(urllib.request.urlopen(f"{base}/api/state", timeout=5).read())
        assert len(state["positions"]) == 3 * len(scene["bodies"])
        assert len(state["quaternions"]) == 4 * len(scene["bodies"])
        page = urllib.request.urlopen(f"{base}/", timeout=5).read().decode()
        assert "viewer.js" in page
    finally:
        httpd.stopping.set()
        httpd.shutdown()
        httpd.server_close()
        simulation.close()
