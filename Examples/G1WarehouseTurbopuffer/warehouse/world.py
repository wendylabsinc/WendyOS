"""Compose the MuJoCo world: the Unitree G1 (GR00T WBC model) plus a small warehouse."""

from __future__ import annotations

import math
import re
from dataclasses import dataclass
from pathlib import Path

import mujoco
import numpy as np

from .catalog import (BAY_PITCH, BAYS, CART, CART_SLOTS, INBOUND, SHELF_TOP, SHELVED, STOCK, STOCK_BAYS, TOP_SHELF,
                      ZONES, Item, Zone)

ROOT = Path(__file__).resolve().parents[1]
ROBOT_DIR = ROOT / "models" / "g1"
TIMESTEP = 0.0025
RACK_DEPTH = 0.44
RACK_WIDTH = BAYS * BAY_PITCH + 0.2
BOX_INSET = 0.13  # box centre behind the front edge of a shelf or cart
# The model's Dex3 hand joints are fixed at zero, which leaves each thumb sticking ~9 cm straight out
# of the palm. Pose the hands as the real joints can: thumb folded toward the fingers (thumb_1 at its
# limit), fingers a little curled. Left-hand angles; the right hand mirrors them.
HAND_POSE = {"thumb_1": 1.047, "thumb_2": 1.2, "index_0": -0.45, "middle_0": -0.45,
             "index_1": -0.40, "middle_1": -0.40}


@dataclass(frozen=True)
class Slot:
    """Where a box rests: centre of its bottom face, and the robot heading to reach it."""
    x: float
    y: float
    z: float
    facing: float
    zone: str
    bay: int


def rack_slot(zone: Zone, bay: int) -> Slot:
    depth = np.array([math.cos(zone.facing), math.sin(zone.facing)])
    right = np.array([depth[1], -depth[0]])   # bays count left to right, facing the rack
    xy = np.array([zone.x, zone.y]) + depth * BOX_INSET + right * (bay - (BAYS - 1) / 2) * BAY_PITCH
    return Slot(float(xy[0]), float(xy[1]), SHELF_TOP, zone.facing, zone.key, bay)


def cart_slot(index: int) -> Slot:
    facing = CART["facing"]
    depth = np.array([math.cos(facing), math.sin(facing)])
    lateral = np.array([-depth[1], depth[0]])
    xy = np.array([CART["x"], CART["y"]]) + depth * BOX_INSET + lateral * CART_SLOTS[index]
    return Slot(float(xy[0]), float(xy[1]), CART["top"], facing, "cart", index)


def _quat_z(angle: float) -> str:
    return f"{math.cos(angle / 2):.6f} 0 0 {math.sin(angle / 2):.6f}"


def _rack_xml(zone: Zone) -> str:
    # Local frame: x along the rack, y into the rack (the robot's facing direction).
    rotation = zone.facing - math.pi / 2
    hw, hd = RACK_WIDTH / 2, RACK_DEPTH / 2
    parts = []
    for level, z in (("work", SHELF_TOP), ("top", 1.30)):
        parts.append(f'<geom name="{zone.key}_{level}_board" type="box" size="{hw} {hd} 0.0125" pos="0 {hd} {z - 0.0125}" '
                     f'rgba="0.62 0.64 0.66 1" friction="1 0.01 0.001"/>')
    for index, (sx, sy) in enumerate((sx, sy) for sx in (-1, 1) for sy in (0, 1)):
        parts.append(f'<geom name="{zone.key}_post_{index}" type="box" size="0.03 0.03 0.85" '
                     f'pos="{sx * (hw - 0.03)} {0.03 + sy * (RACK_DEPTH - 0.06)} 0.85" rgba="0.85 0.45 0.12 1"/>')
    return f'<body name="rack_{zone.key}" pos="{zone.x} {zone.y} 0" quat="{_quat_z(rotation)}">{"".join(parts)}</body>'


def _cart_xml() -> str:
    """A two-deck utility cart on casters, with a push handle at the end the robot uses least."""
    rotation = CART["facing"] - math.pi / 2
    top = CART["top"]
    # local x points opposite to the slots' lateral axis (see cart_slot), so mirror the slot centre
    slots_centre = -(CART_SLOTS[-1] + CART_SLOTS[0]) / 2
    extra = 0.14                                   # extra deck beyond the last slot, under the handle
    hl = (CART_SLOTS[-1] - CART_SLOTS[0] + 0.62 + extra) / 2
    cx = slots_centre - extra / 2                  # deck centre (the handle end is local -x)
    hd = 0.25
    near = 0.03   # the deck starts a little behind the line the robot squats at, and the lower
    #               shelf sits further back, so a crouching robot's hips and knees clear both
    parts = [
        f'<geom name="cart_top" type="box" size="{hl} {hd - near / 2} 0.01" pos="{cx} {hd + near / 2} {top - 0.01}" rgba="0.2 0.22 0.25 1" friction="1 0.01 0.001"/>',
        f'<geom name="cart_shelf" type="box" size="{hl - 0.03} 0.17 0.012" pos="{cx} {2 * hd - 0.19} 0.17" rgba="0.2 0.22 0.25 1"/>',
    ]
    corners = [(sx, sy) for sx in (-1, 1) for sy in (-1, 1)]
    for i, (sx, sy) in enumerate(corners):
        x, y = cx + sx * (hl - 0.03), hd + near / 2 + sy * (hd - near / 2 - 0.03)
        parts.append(f'<geom name="cart_post_{i}" type="box" size="0.02 0.02 {(top - 0.13) / 2}" pos="{x} {y} {(top + 0.13) / 2 - 0.015}" rgba="0.3 0.55 0.85 1"/>')
        parts.append(f'<geom name="cart_fork_{i}" type="box" size="0.022 0.03 0.025" pos="{x} {y} 0.105" rgba="0.25 0.27 0.3 1" contype="0" conaffinity="0"/>')
        parts.append(f'<geom name="cart_wheel_{i}" type="cylinder" size="0.05 0.018" pos="{x} {y} 0.05" quat="0.7071 0.7071 0 0" '
                     f'rgba="0.1 0.1 0.1 1" contype="0" conaffinity="0"/>')
    hx = cx - hl + 0.025                           # push handle: two uprights and a grip
    for i, sy in enumerate((-1, 1)):
        parts.append(f'<geom name="cart_handle_{i}" type="box" size="0.015 0.015 0.25" pos="{hx} {hd + sy * (hd - 0.05)} {top + 0.25}" rgba="0.3 0.55 0.85 1"/>')
    parts.append(f'<geom name="cart_handle_grip" type="cylinder" size="0.02 {hd - 0.05}" pos="{hx} {hd} {top + 0.5}" quat="0.7071 0.7071 0 0" rgba="0.08 0.08 0.09 1"/>')
    return f'<body name="cart" pos="{CART["x"]} {CART["y"]} 0" quat="{_quat_z(rotation)}">{"".join(parts)}</body>'


def _box_xml(name: str, item: Item, slot: Slot, fixed: bool = False) -> str:
    depth, width, height = item.size
    rotation = slot.facing
    joint = "" if fixed else f'<freejoint name="{name}_joint"/>'
    return (f'<body name="{name}" pos="{slot.x:.4f} {slot.y:.4f} {slot.z + height / 2 + 0.001:.4f}" quat="{_quat_z(rotation)}">'
            f'{joint}<geom name="{name}_geom" type="box" size="{depth / 2} {width / 2} {height / 2}" mass="{item.mass}" '
            f'friction="1.5 0.02 0.002" condim="6" rgba="0.78 0.6 0.38 1"/></body>')


def boxes() -> list[tuple[str, Item, Slot]]:
    """Every dynamic box with its starting slot: shelved items in bay 0, inbound on the cart."""
    result = []
    for zone in ZONES:
        result.append((f"box_{SHELVED[zone.key].key}", SHELVED[zone.key], rack_slot(zone, 0)))
    for index, item in enumerate(INBOUND):
        result.append((f"box_{item.key}", item, cart_slot(index)))
    return result


def stock() -> list[tuple[str, Item, Slot]]:
    """Boxes fixed on the top shelves: scenery the robot never moves, but its memory knows."""
    result = []
    for zone in ZONES:
        for bay, item in zip(STOCK_BAYS, STOCK[zone.key]):
            slot = rack_slot(zone, bay)
            result.append((f"stock_{item.key}", item, Slot(slot.x, slot.y, TOP_SHELF, slot.facing, zone.key, bay)))
    return result


def build_xml() -> str:
    xml = (ROBOT_DIR / "g1_gear_wbc.xml").read_text()
    for side, y in (("left", -0.012), ("right", 0.012)):
        xml = re.sub(rf'(<body name="{side}_wrist_yaw_link"[^>]*>)',
                     rf'\1<site name="{side}_palm" pos="0.115 {y} 0" size="0.01"/>', xml, count=1)
    for side, sign in (("left", 1.0), ("right", -1.0)):
        for link, angle in HAND_POSE.items():   # each of these joints turns about its body's z axis
            half = sign * angle / 2
            xml = re.sub(rf'(<body name="{side}_hand_{link}_link" pos="[^"]*")',
                         rf'\1 quat="{math.cos(half):.6f} 0 0 {math.sin(half):.6f}"', xml, count=1)
    xml = xml.replace("<option>", '<option cone="elliptic" impratio="10">', 1)
    xml = xml.replace('<compiler angle="radian" meshdir="meshes"/>',
                      f'<compiler angle="radian" meshdir="{ROBOT_DIR / "meshes"}"/>', 1)
    world = _cart_xml() + "".join(_rack_xml(zone) for zone in ZONES)
    world += "".join(_box_xml(name, item, slot) for name, item, slot in boxes())
    world += "".join(_box_xml(name, item, slot, fixed=True) for name, item, slot in stock())
    idx = xml.rfind("</worldbody>")
    xml = xml[:idx] + world + xml[idx:]
    # Holding a box: welded to the left hand and pinned to the right palm, so both arms carry it.
    # Both start inactive; the robot sets their anchors when it grips.
    welds = "".join(f'<weld name="grasp_{name}" body1="left_wrist_yaw_link" body2="{name}" active="false" solref="0.01 1"/>'
                    f'<connect name="grasp_right_{name}" body1="right_wrist_yaw_link" body2="{name}" anchor="0.115 0.012 0" '
                    f'active="false" solref="0.01 1"/>'
                    for name, _, _ in boxes())
    # The upper-arm and elbow collision hulls overlap the torso hull in carrying poses.
    excludes = "".join(f'<exclude body1="torso_link" body2="{s}_{link}_link"/>'
                       for s in ("left", "right") for link in ("shoulder_roll", "shoulder_yaw", "elbow"))
    end = xml.rfind("</mujoco>")
    return xml[:end] + f"<equality>{welds}</equality><contact>{excludes}</contact>" + xml[end:]


def build_model() -> mujoco.MjModel:
    model = mujoco.MjModel.from_xml_string(build_xml())
    model.opt.timestep = TIMESTEP
    # Even folded, a thumb stands ~3 cm proud of the palm. The thumbs don't collide; the palms stop
    # short of the box (Skills.GRIP_GAP) so the curled fingertips and the thumb meet its side.
    for geom in range(model.ngeom):
        if "_hand_thumb_" in model.body(model.geom_bodyid[geom]).name:
            model.geom_contype[geom] = 0
            model.geom_conaffinity[geom] = 0
    return model
