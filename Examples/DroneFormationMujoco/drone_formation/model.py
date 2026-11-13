"""Build a MuJoCo swarm from the vendored Menagerie Crazyflie 2 model.

The upstream model drives the airframe with one thrust actuator and three
body-torque actuators. Here each copy keeps the upstream body, inertia, meshes,
collision hulls and IMU, but flies on four rotor actuators instead: a thrust
along the rotor axis, the rotor's drag torque about it, and a first-order motor
lag. The body-torque actuators are removed.
"""

from __future__ import annotations

import copy
import xml.etree.ElementTree as ET
from pathlib import Path

import mujoco
import numpy as np


ROOT = Path(__file__).resolve().parents[1]
CF2_XML = ROOT / "models" / "bitcraze_crazyflie_2" / "cf2.xml"

N_DRONES = 12
MASS = 0.027  # kg, from the upstream inertial
INERTIA = np.array([2.3951e-5, 2.3951e-5, 3.2347e-5])  # kg m^2, from the upstream inertial
GRAVITY = 9.81

# Rotor hubs, measured from the propeller mesh (x forward, y left).
ARM = 0.0325
ROTOR_HEIGHT = 0.0129
# Front right, rear right, rear left, front left. Diagonal pairs share a spin
# direction: +1 is clockwise seen from above, whose drag torque yaws the body
# counter-clockwise.
ROTOR_XY = np.array([[ARM, -ARM], [-ARM, -ARM], [-ARM, ARM], [ARM, ARM]])
ROTOR_SPIN = np.array([1.0, -1.0, 1.0, -1.0])
ROTOR_MAX_THRUST = 0.15  # N per rotor
ROTOR_TORQUE_COEFF = 0.025  # rotor drag torque per newton of thrust (m)
MOTOR_TIME_CONSTANT = 0.02  # s
PROP_RADIUS = 0.0232

PHYSICS_HZ = 500
TIMESTEP = 1.0 / PHYSICS_HZ


def _rename_body(body: ET.Element, index: int) -> ET.Element:
    body = copy.deepcopy(body)
    prefix = f"cf{index}"
    body.set("name", prefix)
    for joint in body.iter("freejoint"):
        joint.set("name", f"{prefix}_joint")
    for camera in list(body.iter("camera")):
        camera.set("name", f"{prefix}_{camera.get('name')}")
    for site in list(body.findall("site")):
        if site.get("name") == "actuation":
            body.remove(site)
        else:
            site.set("name", f"{prefix}_{site.get('name')}")
    for rotor, (x, y) in enumerate(ROTOR_XY):
        ET.SubElement(body, "site", {
            "name": f"{prefix}_rotor{rotor}",
            "pos": f"{x:.4f} {y:.4f} {ROTOR_HEIGHT:.4f}",
        })
    return body


def build_xml(spawn: np.ndarray) -> str:
    """Return MJCF for len(spawn) Crazyflies parked at the given positions."""
    upstream = ET.parse(CF2_XML).getroot()
    body = upstream.find("worldbody/body[@name='cf2']")
    if body is None:
        raise ValueError(f"{CF2_XML} has no cf2 body")

    root = ET.Element("mujoco", {"model": "drone_formation"})
    ET.SubElement(root, "compiler", {
        "inertiafromgeom": "false",
        "meshdir": str(CF2_XML.parent / "assets"),
        "autolimits": "true",
    })
    # Aerodynamic drag comes from the wind model, so MuJoCo's own fluid forces
    # stay off to avoid counting drag twice.
    ET.SubElement(root, "option", {
        "timestep": f"{TIMESTEP}", "integrator": "RK4", "density": "0", "viscosity": "0",
    })
    ET.SubElement(root, "size", {"memory": "32M"})
    root.append(copy.deepcopy(upstream.find("default")))
    root.append(copy.deepcopy(upstream.find("asset")))

    world = ET.SubElement(root, "worldbody")
    ET.SubElement(world, "light", {"pos": "0 0 6", "dir": "0 0 -1", "directional": "true"})
    ET.SubElement(world, "geom", {
        "name": "floor", "type": "plane", "size": "0 0 0.05", "rgba": "0.9 0.89 0.86 1",
    })
    for index, position in enumerate(spawn):
        drone = _rename_body(body, index)
        drone.set("pos", " ".join(f"{value:.4f}" for value in position))
        world.append(drone)

    actuators = ET.SubElement(root, "actuator")
    sensors = ET.SubElement(root, "sensor")
    for index in range(len(spawn)):
        prefix = f"cf{index}"
        for rotor in range(4):
            ET.SubElement(actuators, "general", {
                "name": f"{prefix}_rotor{rotor}",
                "site": f"{prefix}_rotor{rotor}",
                "gear": f"0 0 1 0 0 {ROTOR_SPIN[rotor] * ROTOR_TORQUE_COEFF:.4f}",
                "dyntype": "filter", "dynprm": f"{MOTOR_TIME_CONSTANT}",
                "gaintype": "fixed", "gainprm": "1", "biastype": "none",
                "ctrlrange": f"0 {ROTOR_MAX_THRUST}",
            })
        site = f"{prefix}_imu"
        ET.SubElement(sensors, "framepos", {"name": f"{prefix}_pos", "objtype": "site", "objname": site})
        ET.SubElement(sensors, "framelinvel", {"name": f"{prefix}_vel", "objtype": "site", "objname": site})
        ET.SubElement(sensors, "framequat", {"name": f"{prefix}_quat", "objtype": "site", "objname": site})
        ET.SubElement(sensors, "gyro", {"name": f"{prefix}_gyro", "site": site})
    return ET.tostring(root, encoding="unicode")


def build_model(spawn: np.ndarray) -> mujoco.MjModel:
    return mujoco.MjModel.from_xml_string(build_xml(spawn))


# Sensor layout per drone in data.sensordata: pos(3) vel(3) quat(4) gyro(3).
SENSOR_WIDTH = 13
