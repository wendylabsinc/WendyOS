"""Office hallway geometry layered onto the existing, pinned Go2 physics model."""
from pathlib import Path
import tempfile
import xml.etree.ElementTree as ET


def hallway_model(assets, layout="corner", width=1.4):
    import mujoco
    from go2_sim.sensors import instrumented_model
    if layout not in ("straight", "corner", "junction", "blocked"):
        raise ValueError("unknown hallway layout")
    if not 1.0 <= width <= 2.5:
        raise ValueError("hallway width must be between 1 and 2.5 metres")
    model = instrumented_model(assets, sandbox=False)
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "instrumented.xml"
        mujoco.mj_saveLastXML(str(path), model)
        scene = ET.parse(path).getroot()
    world = scene.find("worldbody")
    thickness = .05
    h = width / 2 + thickness  # The requested width is between inner wall faces.
    segments = []
    def wall(x1, y1, x2, y2):
        segments.append((x1, y1, x2, y2))
    wall(-1, -h, -1, h)
    if layout in ("straight", "blocked"):
        wall(-1, -h, 7, -h)
        wall(-1, h, 7, h)
        wall(7, -h, 7, h)
    elif layout == "corner":
        wall(-1, -h, 3+h, -h)
        wall(3+h, -h, 3+h, 5)
        wall(-1, h, 3-h, h)
        wall(3-h, h, 3-h, 5)
        wall(3-h, 5, 3+h, 5)
    else:
        wall(-1, -h, 3-h, -h)
        wall(-1, h, 3-h, h)
        wall(3-h, h, 3-h, 4)
        wall(3-h, -h, 3-h, -4)
        wall(3+h, -4, 3+h, 4)
        wall(3-h, 4, 3+h, 4)
        wall(3-h, -4, 3+h, -4)
    for i, (x1, y1, x2, y2) in enumerate(segments):
        ET.SubElement(world, "geom", name=f"hallway_wall_{i}", type="box", group="0",
                      pos=f"{(x1+x2)/2} {(y1+y2)/2} 1", size=f"{abs(x2-x1)/2+thickness} {abs(y2-y1)/2+thickness} 1",
                      rgba="0.65 0.70 0.76 1")
    if layout == "blocked":
        ET.SubElement(world, "geom", name="hallway_blocker", type="box", group="0",
                      pos="2.8 0 0.5", size=f"0.2 {h} 0.5", rgba="0.85 0.4 0.1 1")
    return mujoco.MjModel.from_xml_string(ET.tostring(scene, encoding="unicode"))
