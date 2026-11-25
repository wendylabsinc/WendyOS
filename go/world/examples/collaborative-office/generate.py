"""Build a synthetic three-robot office and a conservative compiled-geometry grid."""
from collections import deque
import copy
import hashlib
import heapq
import json
import math
from pathlib import Path
import xml.etree.ElementTree as ET

HERE = Path(__file__).resolve().parent
SOURCE = HERE.parent / "demo-office/office.xml"
ORIGIN = (-8.25, -6.15)
RESOLUTION = .3
WIDTH, HEIGHT = 55, 41
INFLATION = .32
ROBOTS = [
    {"id": "atlas", "name": "Atlas", "color": [1., .64, .12], "desired": [-4.2, -1.8]},
    {"id": "iris", "name": "Iris", "color": [.08, .77, .69], "desired": [0, -4.8]},
    {"id": "echo", "name": "Echo", "color": [.65, .42, .96], "desired": [3.9, -2.7]},
]

# These are design dimensions for a synthetic office, not a survey or a scan.
SPACES = [
    {"id": "workspace", "label": "Studio", "purpose": "Four-person shared workspace", "center": [-5.3, 3.75], "size": [5.0, 4.2]},
    {"id": "meeting", "label": "Meeting room", "purpose": "Six-person project room", "center": [0, 3.75], "size": [5.0, 4.2]},
    {"id": "private-office", "label": "Focus room", "purpose": "Quiet work and reference library", "center": [5.3, 3.75], "size": [5.0, 4.2]},
    {"id": "lounge", "label": "Lounge", "purpose": "Informal collaboration", "center": [-4.7, -3.75], "size": [6.0, 4.2]},
    {"id": "reception", "label": "Reception", "purpose": "Arrival and site operations", "center": [1.1, -3.65], "size": [4.4, 4.3]},
    {"id": "kitchen", "label": "Kitchen", "purpose": "Coffee and shared amenities", "center": [5.5, -3.75], "size": [4.3, 4.2]},
]


def finish_office(root):
    """Apply local, primitive-only design details without editing the source twin."""
    world, assets = root.find("worldbody"), root.find("asset")
    # The source plinth and main floor both ended at z=0. Keep their visible top
    # surfaces separate: floor top remains 0, plinth top meets floor underside -.01.
    world.find("geom[@name='building-plinth']").set("pos", "0 0 -0.13")
    materials = {
        "wall": (".82 .81 .76 1", .08), "edge": (".19 .21 .20 1", .18),
        "wood": (".48 .32 .19 1", .16), "birch": (".76 .61 .42 1", .12),
        "metal": (".14 .17 .18 1", .5), "white": (".88 .87 .82 1", .12),
        "seat": (".20 .28 .28 1", .05), "sofa": (".36 .44 .39 1", .03),
        "pot": (".62 .56 .46 1", .06), "glass": (".57 .74 .76 .19", .65),
        "leaf": (".13 .28 .16 1", .08), "leaf-light": (".28 .41 .23 1", .07),
        "blue": (".24 .38 .43 1", .08), "paper": (".94 .92 .85 1", .04),
        "stone": (".58 .59 .56 1", .18), "felt": (".47 .51 .47 1", .02),
        "linen": (".63 .60 .51 1", .02), "clay": (".64 .37 .26 1", .03),
        "carpet": (".42 .46 .43 1", .02), "floor": (".66 .65 .60 1", .12),
        "bronze": (".35 .29 .19 1", .45), "rubber": (".055 .065 .067 1", .03),
        "lens": (".025 .06 .075 1", .75), "light-diffuser": ("1 .91 .73 1", .05),
    }
    for name, (rgba, specular) in materials.items():
        material = assets.find(f"material[@name='{name}']")
        if material is None:
            material = ET.SubElement(assets, "material", name=name)
        material.set("rgba", rgba); material.set("specular", str(specular))
        material.set("shininess", ".25" if name in ("metal", "lens", "glass") else ".08")
    assets.find("material[@name='light-diffuser']").set("emission", ".45")
    # Native primitive export uses the same explicit material colors; no implicit texture tint.
    for name in ("carpet", "floor"):
        material = assets.find(f"material[@name='{name}']")
        material.attrib.pop("texture", None); material.attrib.pop("texrepeat", None)
    for texture in assets.findall("texture"):
        assets.remove(texture)
    root.find("visual/headlight").set("ambient", ".30 .31 .33")
    root.find("visual/headlight").set("diffuse", ".50 .49 .46")
    root.find("visual/quality").set("shadowsize", "2048")
    root.find("visual/quality").set("offsamples", "4")
    for light in world.findall("light"):
        world.remove(light)
    ET.SubElement(world, "light", name="daylight", pos="-4 5 9", dir=".35 -.6 -1",
                  directional="true", diffuse=".70 .73 .76", ambient=".10 .10 .10", castshadow="true")
    ET.SubElement(world, "light", name="interior-fill", pos="3 -3 6", dir="0 0 -1",
                  diffuse=".38 .35 .30", castshadow="false")

    def geom(parent, name, pos, size, material, kind="box", collision=False, group="0", **extra):
        if "fromto" not in extra:
            extra["pos"] = " ".join(map(str, pos))
        return ET.SubElement(parent, "geom", name=name, type=kind,
                             size=" ".join(map(str, size)),
                             material=material, group=group,
                             contype="1" if collision else "0",
                             conaffinity="1" if collision else "0", **extra)

    # Architectural finish, not route overlays: staggered oak boards in the hall.
    for index in range(4, 21):
        world.remove(world.find(f"geom[@name='detail-{index}']"))
    world.find("geom[@name='central-corridor']").set("material", "wood")
    for index, rgba in enumerate((".64 .49 .32 1", ".68 .53 .36 1", ".72 .57 .39 1", ".66 .51 .34 1")):
        ET.SubElement(assets, "material", name=f"oak-board-{index}", rgba=rgba, specular=".13", shininess=".07")
    for row in range(12):
        y = -1.2 + (row + .5) * .2
        x = -7.9
        board = 0
        while x < 7.89:
            length = min(1.55 if board else (1.05 if row % 2 else 1.55), 7.9 - x)
            geom(world, f"finish-oak-{row}-{board}", [x + length/2, y, .014],
                 [length/2-.005, .097, .004], f"oak-board-{(row+board)%4}", group="2")
            x += length; board += 1
    # Real openings in the rear wall; the low wall remains an intact collision boundary.
    world.remove(world.find("geom[@name='detail-21']"))
    geom(world, "architecture-rear-sill-wall", [0, 6, .46], [8, .1, .46], "wall", collision=True)
    geom(world, "architecture-rear-header", [0, 6, 2.51], [8, .1, .19], "wall", collision=True)
    for index, (left, right) in enumerate(((-8,-7.05),(-4.95,-4.05),(-1.95,-1.05),(1.05,1.95),(4.05,4.95),(7.05,8))):
        geom(world, f"architecture-window-pier-{index}", [(left+right)/2,6,1.62],
             [(right-left)/2,.1,.70], "wall", collision=True)
    for index in range(33,53):
        world.remove(world.find(f"geom[@name='detail-{index}']"))
    for index,x in enumerate((-6,-3,0,3,6)):
        geom(world, f"window-{index}-glass", [x,6,1.62], [1.025,.015,.67], "glass", collision=True)
        for side,dx in (("left",-1.035),("right",1.035),("mullion",0)):
            geom(world,f"window-{index}-{side}",[x+dx,5.965,1.62],[.022,.045,.70],"metal",collision=True)
        for name,z in (("bottom",.94),("top",2.30)):
            geom(world,f"window-{index}-{name}",[x,5.965,z],[1.055,.045,.025],"metal",collision=True)
        geom(world,f"window-{index}-sill",[x,5.83,.925],[1.12,.20,.025],"stone")
        geom(world,f"window-{index}-blind",[x,5.88,2.24],[1.00,.025,.055],"linen")
    # Quiet acoustic detailing and material changes separate the rooms visually.
    for x in (-2.55,2.55):
        for index in range(9):
            geom(world,f"acoustic-{x}-{index}",[x,2.15+index*.36,1.31],[.025,.14,.37],"felt")
    world.find("geom[@name='detail-94']").set("material", "felt")
    world.find("geom[@name='detail-95']").set("material", "linen")
    world.find("geom[@name='detail-96']").set("material", "stone")
    # Lamps are wall/desk attached so the cutaway does not have floating ceiling slabs.
    for index,x in enumerate((-6,-3,0,3,6)):
        geom(world,f"lighting-rear-{index}",[x,5.76,2.48],[.76,.05,.018],"light-diffuser")
    for room,x in (("studio",-6.62),("meeting",-1.65),("focus",6.62)):
        geom(world,f"sign-{room}",[x,1.413,.87],[.25,.014,.10],"metal")
        for bar in range(3):
            geom(world,f"sign-{room}-mark-{bar}",[x-.13+bar*.13,1.395,.87],[.035,.008,.035],"birch")
    for body in world.iter("body"):
        name = body.get("name", "")
        if name.startswith("workstation-") or name == "private-desk":
            geom(body,f"desk-{name}-deskmat",[.03,-.12,.804],[.36,.19,.003],"felt")
            geom(body,f"desk-{name}-mouse",[.37,-.18,.827],[.035,.05,.013],"metal",kind="ellipsoid")
            geom(body,f"desk-{name}-lamp-base",[-.61,.19,.815],[.08,.012],"bronze",kind="cylinder")
            geom(body,f"desk-{name}-lamp-arm",[-.61,.19,.98],[.012,.16],"bronze",kind="cylinder")
            geom(body,f"desk-{name}-lamp-shade",[-.54,.18,1.14],[.14,.07,.018],"metal")
            geom(body,f"desk-{name}-lamp-light",[-.54,.18,1.119],[.12,.055,.004],"light-diffuser")
        if name.startswith("lounge-sofa"):
            for g in body.findall("geom"):
                if g.get("material") == "birch":
                    g.set("material","linen")
                    g.set("size",".32 .30 .055")
            geom(body,f"textile-{name}-pillow",[-.73,.22,.80],[.22,.065,.20],"clay",kind="ellipsoid",euler="12 0 -8")
        # Identify the existing wheeled office-chair seat, independent of body names.
        chair = next((g for g in body.findall("geom") if g.get("material") == "seat" and
                      g.get("pos") == "0 0 0.48"), None)
        if chair is not None:
            label = chair.get("name")
            for side in (-1,1):
                geom(body,f"chair-{label}-arm-{side}",[side*.265,0,.66],[.028,.18,.025],"rubber")
                geom(body,f"chair-{label}-support-{side}",[side*.265,.1,.56],[.014,.02,.08],"metal")
    # Table accessories remain above the robot-height collision envelope.
    meeting = world.find("body[@name='meeting-room']")
    geom(meeting,"meeting-speaker",[0,0,.86],[.13,.035],"metal",kind="cylinder")
    for index,x in enumerate((-.72,.72)):
        geom(meeting,f"meeting-laptop-{index}",[x,.15,.845],[.19,.14,.012],"metal")
        geom(meeting,f"meeting-laptop-screen-{index}",[x,.28,.99],[.19,.012,.14],"screen",euler="-12 0 0")
        geom(meeting,f"meeting-glass-{index}",[x,-.35,.91],[.04,.07],"glass",kind="cylinder")
    # Cabinet doors face the room, with sink, mixer, coffee spouts and a small tray.
    for index,x in enumerate((4.5,5.7,6.9)):
        geom(world,f"kitchen-door-{index}",[x,-5.035,.48],[.55,.012,.38],"felt")
        geom(world,f"kitchen-handle-{index}",[x,-5.01,.72],[.17,.018,.012],"bronze")
    geom(world,"kitchen-sink-rim",[6.9,-5.40,.956],[.29,.24,.008],"metal")
    geom(world,"kitchen-sink-basin",[6.9,-5.40,.966],[.255,.205,.006],"lens")
    geom(world,"kitchen-mixer-upright",[6.9,-5.68,1.08],[.018,.13],"metal",kind="cylinder")
    geom(world,"kitchen-mixer-spout",[6.9,-5.59,1.20],[.018],"metal",kind="capsule",
         fromto="6.9 -5.68 1.2 6.9 -5.48 1.2")
    geom(world,"kitchen-coffee-tray",[4.55,-5.12,.97],[.20,.09,.014],"metal")
    for index,x in enumerate((4.46,4.64)):
        geom(world,f"kitchen-coffee-spout-{index}",[x,-5.17,1.10],[.012,.022],"metal",kind="cylinder")
        geom(world,f"kitchen-cup-{index}",[x,-5.10,1.01],[.037,.04],"white",kind="cylinder")
    geom(world,"kitchen-serving-tray",[5.7,-5.4,.96],[.34,.23,.012],"wood")
    geom(world,"kitchen-water-carafe",[5.62,-5.4,1.08],[.055,.12],"glass",kind="cylinder")
    # Retain the existing plant footprints; leaf details are cosmetic and above their pots.
    for body in world.findall("body"):
        name=body.get("name","")
        if "plant" not in name:
            continue
        for index,g in enumerate(list(body.findall("geom"))):
            if g.get("material") not in ("leaf","leaf-light"):
                continue
            pos=list(map(float,g.get("pos").split()))
            geom(body,f"plant-{name}-vein-{index}",pos,[.009,.008,.16],"leaf",kind="ellipsoid",
                 euler=g.get("euler","0 0 0"))
    # Small camera and environmental sensor housings mark commissioned *simulated* poses.
    sensors = []
    for sid,pos in (("corridor-camera-west",[-7.7,1.1,2.1]),("corridor-camera-east",[7.7,-.7,2.1])):
        body=ET.SubElement(world,"body",name=sid,pos=" ".join(map(str,pos)))
        geom(body,f"sensor-{sid}-mount",[0,0,0],[.055,.07,.035],"white")
        geom(body,f"sensor-{sid}-dome",[0,0,-.055],[.065,.065,.045],"lens",kind="ellipsoid")
        sensors.append({"id":sid,"kind":"camera-housing","body":sid,"position":pos,
                        "status":"visual placement only; no new image stream"})
    geom(world,"sensor-gas-housing",[7.78,-4.7,1.30],[.065,.08,.11],"white")
    geom(world,"sensor-gas-status",[7.70,-4.7,1.35],[.006,.028,.014],"ui")
    sensors.append({"id":"kitchen-gas-housing","kind":"gas-housing","position":[7.78,-4.7,1.3],
                    "status":"visual placement only; existing analytic gas field is unchanged"})
    return {"name":"Wendy Studio", "subtitle":"Synthetic office · 192 m² · MuJoCo 4",
            "labels":[{"text":space["label"],"position":space["center"]+[.04]} for space in SPACES],
            "design":"Warm contemporary studio / synthetic architectural cutaway",
            "footprint_m":[16,12],"wall_height_m":2.7,"spaces":SPACES,"sensors":sensors,
            "materials":[{"name":name,"rgba":list(map(float,value[0].split())),
                          "specular":value[1]} for name,value in materials.items()],
            "assets":"Procedural MuJoCo primitives only; no downloaded or external assets",
            "ceiling":"Omitted for inspection; wall lights and daylight are illustrative",
            "not_established":["surveyed office geometry","sensor calibration","physical lighting measurements"]}


def geometry_categories(world):
    categories = {name: [] for name in
                  ("architecture", "glazing", "furniture", "plants", "lighting", "sensors", "floor", "robots")}
    parents = {child: parent for parent in world.iter() for child in parent}
    for geom in world.iter("geom"):
        name, material = geom.get("name"), geom.get("material", "")
        ancestors, parent = [], parents[geom]
        while parent is not world:
            ancestors.append(parent.get("name", "")); parent = parents[parent]
        if geom.get("group") == "1": category = "robots"
        elif geom.get("group") == "2": category = "floor"
        elif name.startswith("sensor-"): category = "sensors"
        elif name.startswith("lighting-") or material == "light-diffuser": category = "lighting"
        elif any("plant" in ancestor for ancestor in ancestors): category = "plants"
        elif name.startswith("window-") or material == "glass" and name.startswith("detail-"): category = "glazing"
        elif name.startswith(("architecture-", "acoustic-", "sign-")) or material in ("wall", "edge") and not ancestors: category = "architecture"
        else: category = "furniture"
        categories[category].append(name)
    return categories


def center(cell):
    return [round(ORIGIN[i] + (cell[i]+.5)*RESOLUTION, 6) for i in (0, 1)]


def neighbors(cell):
    x, y = cell
    return ((x+1,y), (x-1,y), (x,y+1), (x,y-1))


def path_to(start, goal, free):
    pending = [(0, start)]
    distance, parent = {start: 0}, {}
    while pending:
        _, current = heapq.heappop(pending)
        if current == goal:
            path = [goal]
            while path[-1] != start:
                path.append(parent[path[-1]])
            return list(reversed(path))
        for nxt in neighbors(current):
            cost = distance[current]+1
            if nxt in free and cost < distance.get(nxt, float("inf")):
                distance[nxt], parent[nxt] = cost, current
                heapq.heappush(pending, (cost+abs(nxt[0]-goal[0])+abs(nxt[1]-goal[1]), nxt))
    raise ValueError(f"No conservative route from {start} to {goal}")


def collision_bounds(model, data, mujoco):
    """Conservative world AABBs of every supported static collision geom."""
    import numpy as np
    types = mujoco.mjtGeom
    bounds = []
    for i in range(model.ngeom):
        body = int(model.geom_bodyid[i])
        moving = False
        while body:
            if model.body_dofnum[body]:
                moving = True
                break
            body = int(model.body_parentid[body])
        if moving or not (model.geom_contype[i] or model.geom_conaffinity[i]):
            continue
        kind = int(model.geom_type[i])
        name = mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_GEOM, i)
        if kind == int(types.mjGEOM_PLANE) and name == "original-ground-plane":
            continue  # Supporting ground; building boundary is handled explicitly.
        sizes = model.geom_size[i]
        if kind == int(types.mjGEOM_SPHERE):
            half = np.array([sizes[0]]*3)
        elif kind == int(types.mjGEOM_CAPSULE):
            half = np.array([sizes[0], sizes[0], sizes[0]+sizes[1]])
        elif kind == int(types.mjGEOM_CYLINDER):
            half = np.array([sizes[0], sizes[0], sizes[1]])
        elif kind in (int(types.mjGEOM_BOX), int(types.mjGEOM_ELLIPSOID)):
            half = sizes
        else:
            raise ValueError(f"Unsupported collision geometry {name}: {kind}")
        ext = np.abs(data.geom_xmat[i].reshape(3,3)) @ half
        low, high = data.geom_xpos[i]-ext, data.geom_xpos[i]+ext
        # Body collision sphere reaches .48m; cosmetic lidar reaches .51m.
        if high[2] > .001 and low[2] <= .56:
            bounds.append({"geom": name, "min": low.tolist(), "max": high.tolist()})
    return bounds


def occupied(bounds):
    blocked = []
    half = RESOLUTION/2
    for y in range(HEIGHT):
        for x in range(WIDTH):
            px, py = center((x,y))
            if abs(px)+half+INFLATION >= 7.9 or abs(py)+half+INFLATION >= 5.9:
                blocked.append((x,y)); continue
            if any(px+half >= b["min"][0]-INFLATION and px-half <= b["max"][0]+INFLATION
                   and py+half >= b["min"][1]-INFLATION and py-half <= b["max"][1]+INFLATION for b in bounds):
                blocked.append((x,y))
    return blocked


def nearest(desired, cells, region=None):
    candidates = []
    for cell in cells:
        p = center(cell)
        if region and not (region[0] <= p[0] <= region[1] and region[2] <= p[1] <= region[3]):
            continue
        candidates.append((sum((p[i]-desired[i])**2 for i in (0,1)), cell))
    if not candidates:
        raise ValueError(f"No reachable point in {region}")
    return min(candidates)[1]


def add_robot(world, actuator, assets, robot):
    rid = robot["id"]
    color = " ".join(map(str, robot["color"])) + " 1"
    ET.SubElement(assets, "material", name=f"robot-{rid}-color", rgba=color, specular="0.25")
    b = ET.SubElement(world, "body", name=f"robot-{rid}", pos=f'{robot["start"][0]} {robot["start"][1]} .24')
    ET.SubElement(b,"inertial",pos="0 0 0",mass="4",diaginertia="0.08 0.08 0.08")
    for axis, vector in (("x","1 0 0"),("y","0 1 0")):
        name = f"robot-{rid}-{axis}"
        ET.SubElement(b,"joint",name=name,type="slide",axis=vector,damping="1",armature="0")
        ET.SubElement(actuator,"velocity",name=name,joint=name,kv="80",ctrlrange="-.65 .65",ctrllimited="true")
    ET.SubElement(b,"geom",name=f"robot-{rid}-collision",type="sphere",size=".24",rgba="0 0 0 0",mass="0",group="1")
    def g(name,kind,pos,size,material,**extra):
        ET.SubElement(b,"geom",name=f"robot-{rid}-{name}",type=kind,pos=pos,size=size,
                      material=material,mass="0",contype="0",conaffinity="0",group="1",**extra)
    g("chassis","box","0 0 -.035",".20 .15 .09","white")
    g("body","box","0 0 .065",".20 .15 .045",f"robot-{rid}-color")
    g("front","box",".207 0 0",".008 .13 .03","metal")
    g("status","box",".217 0 .04",".005 .08 .012","ui")
    for side in (-1,1):
        g(f"bumper-{side}","capsule","0 0 0",".025","rubber",
          fromto=f"-.17 {side*.15} -.04 .17 {side*.15} -.04")
        g(f"service-panel-{side}","box",f"0 {side*.152} .025",".12 .004 .025","metal")
        for bolt in (-.1,.1):
            g(f"panel-fastener-{side}-{bolt}","sphere",f"{bolt} {side*.158} .025",".007","white")
    for x in (-.13,.13):
        for y in (-.17,.17):
            g(f"wheel-{x}-{y}","cylinder",f"{x} {y} -.15",".085 .025","rubber",euler="90 0 0")
            g(f"hub-{x}-{y}","cylinder",f"{x} {y+(.026 if y>0 else -.026)} -.15",".032 .003","metal",euler="90 0 0")
    g("front-optics","box",".211 0 .07",".012 .066 .018","lens")
    for y in (-.04,.04):
        g(f"front-lens-{y}","sphere",f".224 {y} .07",".011","lens")
    g("stop-base","cylinder","-.13 .075 .12",".024 .007","metal")
    g("stop-button","cylinder","-.13 .075 .13",".016 .006","indicator-material")
    g("lidar-base","cylinder","0 0 .15",".065 .035","metal")
    g("lidar","cylinder","0 0 .23",".07 .04",f"robot-{rid}-color")
    ET.SubElement(b,"site",name=f"robot-{rid}-lidar",pos="0 0 .27",size=".012",rgba=color)
    robot.update(body=f"robot-{rid}", joints=[f"robot-{rid}-x",f"robot-{rid}-y"],
                 actuators=[f"robot-{rid}-x",f"robot-{rid}-y"], lidar_site=f"robot-{rid}-lidar")


def generate():
    import mujoco
    if mujoco.__version__ != "4.0.0":
        raise ValueError("Generate with pinned MuJoCo 4.0.0")
    tree = ET.parse(SOURCE)
    root = tree.getroot(); root.set("model", "wendy-collaborative-office-v1")
    world = root.find("worldbody")
    world.remove(world.find("body[@name='inspection-puck']"))
    root.remove(root.find("actuator"))
    presentation = finish_office(root)
    floor_names = {"original-ground-plane", "building-plinth", "office-floor", "central-corridor"}
    floor_names.update(f"detail-{index}" for index in range(92, 97))
    for geom in world.iter("geom"):
        floor = geom.get("name") in floor_names or geom.get("name", "").startswith("finish-oak-")
        geom.set("group", "2" if floor else "0")
    # Move the freestanding synthetic alarm away from the middle meeting-room doorway.
    world.find("body[@name='indicator']").set("pos", "-7.3 .9 .65")
    # Leave an actual clear entry aisle in front of the meeting-room chairs.
    meeting = world.find("body[@name='meeting-room']")
    meeting.set("pos", "0 4.4 0")
    for geom in meeting.findall("geom"):
        position = list(map(float, geom.get("pos", "0 0 0").split()))
        if position[1] > 1.5:
            position[1] -= .6  # Keep its wall display fixed on the original rear wall.
            geom.set("pos", " ".join(map(str, position)))
    actuator = ET.SubElement(root,"actuator")
    initial_xml = ET.tostring(root,encoding="unicode")
    static_model = mujoco.MjModel.from_xml_string(initial_xml)
    data = mujoco.MjData(static_model); mujoco.mj_forward(static_model,data)
    bounds = collision_bounds(static_model,data,mujoco)
    blocked = occupied(bounds)
    free = {(x,y) for x in range(WIDTH) for y in range(HEIGHT)} - set(blocked)
    seed = nearest([0,0],free)
    reachable = {seed}; queue = deque([seed])
    while queue:
        for nxt in neighbors(queue.popleft()):
            if nxt in free and nxt not in reachable:
                reachable.add(nxt); queue.append(nxt)
    robots=[]
    for template in ROBOTS:
        robot=copy.deepcopy(template)
        candidate = nearest(robot.pop("desired"), reachable, [-7.3,7.3,-5.3,-1.5])
        robot["start"]=center(candidate)
        if any(math.dist(robot["start"],r["start"]) < 1.2 for r in robots):
            raise ValueError("Robot starts are not sufficiently separated")
        add_robot(world,actuator,root.find("asset"),robot); robots.append(robot)
    presentation["geometry_categories"] = geometry_categories(world)
    jobs=[]
    for jid,label,desired,region in (
        ("workspace","Shared workspace",[-3.4,3.6],[-7.3,-3.15,2.1,5.3]),
        ("meeting","Meeting room",[-1.8,3.5],[-2.1,2.1,2.1,5.3]),
        ("private-office","Private office",[5.2,2.7],[3.1,7.3,2.1,5.3]),
        ("lounge","Collaboration lounge",[-3.0,-3.7],[-7.3,-2.0,-5.3,-2.1]),
        ("reception","Reception desk",[.3,-4.5],[-.9,2.7,-5.3,-3.7]),
        ("kitchen","Kitchenette",[4.0,-3.9],[3.7,7.3,-4.7,-2.1]),
    ):
        cell=nearest(desired,reachable,region)
        jobs.append({"id":jid,"label":label,"position":center(cell)})
    # Every route uses four-neighbour cell centres; no diagonal corner cutting.
    route_lengths={}
    for robot in robots:
        start=nearest(robot["start"],free)
        for job in jobs:
            route_lengths[f'{robot["id"]}/{job["id"]}']=len(path_to(start,nearest(job["position"],free),free))-1
    ET.indent(root,space="  ")
    xml=ET.tostring(root,encoding="utf-8",xml_declaration=True)+b"\n"
    (HERE/"office.xml").write_bytes(xml)
    model=mujoco.MjModel.from_xml_string(xml.decode())
    if (model.nq,model.nv,model.nu)!=(6,6,6): raise ValueError("Unexpected multi-robot dynamics")
    result={"schema_version":"wendy.collaborative-office.scene.v1",
        "model":{"path":"office.xml","sha256":hashlib.sha256(xml).hexdigest()},
        "frame":{"up":"Z","axes":["X","Y"],"units":"metres"},
        "robots":robots,"grid":{"origin":list(ORIGIN),"resolution":RESOLUTION,"width":WIDTH,"height":HEIGHT,
          "blocked":[list(c) for c in blocked],"cell_coordinates":"center = origin + (index + 0.5) * resolution",
          "obstacle_inflation_m":INFLATION,"static_height_interval_m":[.001,.56]},"jobs":jobs,
        "provenance":{"origin":"simulated","source_office_sha256":hashlib.sha256(SOURCE.read_bytes()).hexdigest(),
          "geometry":"hand-authored furnished office with local material, window, furniture and sensor-housing refinements; alarm relocated west and meeting furniture shifted north .6m to leave a real entry aisle",
          "robot":"synthetic planar velocity model, two slides per robot, no yaw or differential-drive dynamics",
          "grid":"compiled collision-geom conservative world AABBs at robot height, inflated .32m; entire grid cell must be clear",
          "not_established":["physical safety","survey accuracy","general autonomous navigation","hardware authority"]},
        "presentation":presentation}
    (HERE/"scene.json").write_text(json.dumps(result,indent=2)+"\n")
    (HERE/"generation-report.json").write_text(json.dumps({"mujoco":mujoco.__version__,"model_sha256":result["model"]["sha256"],
        "compiled_geoms":model.ngeom,"static_obstacle_bounds":len(bounds),"blocked_cells":len(blocked),
        "free_cells":len(free),"reachable_cells":len(reachable),"four_neighbor_route_lengths":route_lengths,
        "synthetic_office":True,"external_assets":0,"spaces":len(SPACES),
        "visual_sensor_housings":len(presentation["sensors"])},indent=2)+"\n")
    return result


if __name__=="__main__":
    scene=generate()
    print(json.dumps({"model":scene["model"],"robots":scene["robots"],"jobs":scene["jobs"]},indent=2))
