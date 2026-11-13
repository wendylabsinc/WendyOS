import numpy as np

from warehouse.catalog import INBOUND, zone
from warehouse.simulation import Simulation
from warehouse.world import rack_slot


def test_g1_shelves_the_first_box_where_memory_says(monkeypatch):
    monkeypatch.delenv("TURBOPUFFER_API_KEY", raising=False)
    sim = Simulation()
    try:
        while sim.elapsed < 70.0 and sim.location.get("box_aa", ("cart",))[0] == "cart":
            sim.step()
        zone_key, bay = sim.location["box_aa"]
        assert zone_key == "power"
        slot = rack_slot(zone(zone_key), bay)
        position = sim.data.xpos[sim.box_body["box_aa"]]
        assert np.hypot(position[0] - slot.x, position[1] - slot.y) < 0.06
        assert abs(position[2] - (slot.z + INBOUND[0].size[2] / 2)) < 0.02
        assert sim.falls == 0
        assert sim.error is None
    finally:
        sim.close()
