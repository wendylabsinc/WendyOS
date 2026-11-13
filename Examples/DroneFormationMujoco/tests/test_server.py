import json
import threading
import urllib.request

from drone_formation import server
from drone_formation.simulation import Simulation


def test_server_contract_matches_wendy_json():
    config = json.loads((server.STATIC_ROOT.parents[1] / "wendy.json").read_text())
    ports = {e.get("port") for e in config["entitlements"] if e["type"] == "http"}
    assert server.PORT == 8879
    assert ports == {8879}
    assert config["readiness"]["tcpSocket"]["port"] == 8879
    for name, _ in server.STATIC_FILES.values():
        assert (server.STATIC_ROOT / name).is_file(), name


def test_scene_state_and_health_round_trip():
    simulation = Simulation()
    simulation.run_headless(0.1)
    httpd = server.DemoServer(("127.0.0.1", 0), simulation)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{httpd.server_address[1]}"
    try:
        health = json.loads(urllib.request.urlopen(f"{base}/api/health", timeout=5).read())
        assert health["ready"] is True
        assert health["drones"] == 12
        scene = json.loads(urllib.request.urlopen(f"{base}/api/scene", timeout=5).read())
        rotors = sorted(part["rotor"] for part in scene["parts"] if "rotor" in part)
        assert rotors == [0, 1, 2, 3]
        state = json.loads(urllib.request.urlopen(f"{base}/api/state", timeout=5).read())
        assert len(state["positions"]) == 36
        assert len(state["quaternions"]) == 48
        assert len(state["rotors"]) == 48
        assert state["wind"]["pattern"] == "Calm"
        page = urllib.request.urlopen(f"{base}/", timeout=5).read().decode()
        assert "viewer.js" in page
    finally:
        httpd.stopping.set()
        httpd.shutdown()
        httpd.server_close()
