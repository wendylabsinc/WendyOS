"""What is in the warehouse: zones, items already on the shelves, and the inbound boxes.

The robot never sees a zone for an inbound box. It asks turbopuffer which stored
items and zone descriptions are most similar to the box's label, and shelves the
box in that zone. Item and zone text is embedded by turbopuffer.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Zone:
    key: str
    name: str
    description: str
    # Rack placement: centre (x, y) of the rack front edge and the direction the
    # robot faces when working at it (radians, world frame).
    x: float
    y: float
    facing: float


@dataclass(frozen=True)
class Item:
    key: str
    label: str
    size: tuple[float, float, float]  # full box size (depth, width, height) in metres
    mass: float
    color: str  # sRGB hex for the cardboard tint


ZONES = (
    Zone("power", "Power & batteries",
         "Power and batteries: battery packs, rechargeable cells, chargers, power supplies and power banks.",
         -3.0, 0.0, 3.14159265),
    Zone("cables", "Cables & adapters",
         "Cables and adapters: HDMI, DisplayPort and USB cables, network patch cords, dongles and adapters.",
         0.0, 2.6, 1.57079633),
    Zone("tools", "Tools & fasteners",
         "Tools and fasteners: screwdrivers, hex keys, screws, nuts, bolts, zip ties and clamps.",
         3.0, 0.0, 0.0),
)

# One item already stored in bay 1 of each rack's work shelf. It is written to turbopuffer at
# start-up, with the stock below, so the memory starts with examples of what each zone holds.
SHELVED = {
    "power": Item("9v", "9V batteries, 10 pack", (0.20, 0.26, 0.18), 1.0, "#c9a36b"),
    "cables": Item("dp", "DisplayPort cable, 1 m", (0.20, 0.26, 0.18), 0.6, "#b8966a"),
    "tools": Item("nuts", "M4 lock nuts, box of 200", (0.20, 0.26, 0.18), 1.4, "#c49a62"),
}

# Two more items on each rack's top shelf, out of the robot's reach (it never moves them).
STOCK = {
    "power": (Item("powerbank", "USB-C power bank, 10,000 mAh", (0.20, 0.24, 0.14), 0.5, "#c2a06e"),
              Item("phonecharger", "Phone charger, 20 W", (0.18, 0.22, 0.12), 0.3, "#cbab78")),
    "cables": (Item("ethernet", "Ethernet patch cord, 2 m", (0.20, 0.24, 0.12), 0.3, "#bf9d6c"),
               Item("usba", "USB-C to USB-A adapter", (0.18, 0.22, 0.12), 0.2, "#c8a674")),
    "tools": (Item("hexkeys", "Hex key set, metric", (0.20, 0.24, 0.12), 0.6, "#c09a66"),
              Item("zipties", "Cable ties, 100 pack", (0.18, 0.22, 0.14), 0.4, "#cda977")),
}
STOCK_BAYS = (0, 2)  # top-shelf positions of the two stocked items

# Arrive on the inbound cart in this order.
INBOUND = (
    Item("aa", "AA batteries, 24 pack", (0.22, 0.28, 0.20), 1.2, "#cfa872"),
    Item("hdmi", "USB-C to HDMI adapter", (0.22, 0.28, 0.20), 0.7, "#bd9b6e"),
    Item("torx", "Torx screwdriver set", (0.22, 0.28, 0.20), 1.1, "#c6a06a"),
    Item("charger", "65 W laptop charger", (0.22, 0.28, 0.20), 0.9, "#caa574"),
)

# Asked once everything is shelved; the robot fetches the best match back to the cart. Its
# answer was stocked before the shipment arrived, on a rack the robot has to walk across to.
QUESTION = "I need a cable for my second monitor"

BAYS = 3
BAY_PITCH = 0.62
SHELF_TOP = 0.75
TOP_SHELF = 1.30
CART = dict(x=0.0, y=-1.7, top=0.45, facing=-1.57079633)
CART_SLOTS = (-0.62, 0.0, 0.62, 1.24)  # along x; the fourth sits at the cart's far end


def zone(key: str) -> Zone:
    return next(z for z in ZONES if z.key == key)
