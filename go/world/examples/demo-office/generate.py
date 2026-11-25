"""Generate the proprietary synthetic office MJCF. No external assets required."""
from pathlib import Path
import hashlib
import json
import math
import xml.etree.ElementTree as ET

HERE = Path(__file__).resolve().parent


def fmt(values):
    return " ".join(f"{x:.5g}" for x in values)


def main():
    root = ET.Element("mujoco", model="wendy-demo-office-v1")
    ET.SubElement(root, "compiler", angle="degree")
    ET.SubElement(root, "option", timestep="0.01", gravity="0 0 -9.81")
    ET.SubElement(root, "statistic", center="0 0 0.7", extent="19")
    visual = ET.SubElement(root, "visual")
    ET.SubElement(visual, "global", offwidth="320", offheight="240", azimuth="130", elevation="-48")
    ET.SubElement(visual, "headlight", ambient="0.35 0.35 0.35", diffuse="0.55 0.55 0.55", specular="0.15 0.15 0.15")
    ET.SubElement(visual, "quality", shadowsize="512", offsamples="0")
    ET.SubElement(visual, "map", znear="0.01", zfar="50", shadowclip="1")
    assets = ET.SubElement(root, "asset")
    ET.SubElement(assets, "texture", name="carpet-pattern", type="2d", builtin="checker", width="512", height="512", rgb1="0.30 0.37 0.40", rgb2="0.32 0.39 0.42")
    ET.SubElement(assets, "texture", name="tile-pattern", type="2d", builtin="checker", width="512", height="512", rgb1="0.76 0.78 0.76", rgb2="0.82 0.83 0.80")
    materials = {
        "wall": ("0.84 0.86 0.84 1", {}), "edge": ("0.16 0.26 0.29 1", {}),
        "floor": ("1 1 1 1", {"texture":"tile-pattern", "texrepeat":"16 12"}),
        "carpet": ("1 1 1 1", {"texture":"carpet-pattern", "texrepeat":"8 6"}),
        "wood": ("0.66 0.45 0.26 1", {"specular":"0.18"}), "birch": ("0.83 0.70 0.50 1", {}),
        "metal": ("0.20 0.25 0.27 1", {"specular":"0.5", "shininess":"0.4"}),
        "white": ("0.92 0.92 0.88 1", {}), "screen": ("0.035 0.09 0.13 1", {"emission":"0.1"}),
        "ui": ("0.16 0.66 0.64 1", {"emission":"0.3"}), "blue": ("0.12 0.35 0.43 1", {}),
        "seat": ("0.15 0.28 0.32 1", {}), "sofa": ("0.69 0.48 0.28 1", {}),
        "leaf": ("0.22 0.43 0.22 1", {}), "leaf-light": ("0.38 0.57 0.28 1", {}),
        "pot": ("0.72 0.71 0.65 1", {}), "glass": ("0.40 0.71 0.76 0.25", {"specular":"0.6"}),
        "indicator-material": ("1 0.02 0.02 1", {"emission":"0.5", "specular":"0"}),
        "route": ("0.10 0.53 0.53 1", {}), "paper": ("0.97 0.97 0.92 1", {}),
    }
    for name, (rgba, extra) in materials.items():
        ET.SubElement(assets, "material", name=name, rgba=rgba, **extra)
    world = ET.SubElement(root, "worldbody")
    for i, (x, y) in enumerate(((-5,-3),(4,-2),(0,5))):
        ET.SubElement(world, "light", name=f"ceiling-light-{i}", pos=fmt((x,y,8)), dir="0 0 -1", diffuse="0.65 0.65 0.62", castshadow="true" if i==0 else "false")
    counter = 0

    def geom(parent, kind, pos, size, mat, name=None, **extra):
        nonlocal counter
        counter += 1
        return ET.SubElement(parent, "geom", name=name or f"detail-{counter}", type=kind, pos=fmt(pos), size=fmt(size), material=mat, **extra)

    def box(pos, size, mat, name=None, parent=None, **extra):
        return geom(world if parent is None else parent, "box", pos, size, mat, name, **extra)

    def cylinder(parent, pos, size, mat, **extra):
        return geom(parent, "cylinder", pos, size, mat, **extra)

    def wall(x, y, sx, sy, height=2.6):
        box((x,y,height/2), (sx,sy,height/2), "wall")
        box((x,y,.06), (sx+.012,sy+.012,.06), "edge")
        box((x,y,height+.025), (sx+.025,sy+.025,.025), "white")

    def body(name, x, y, angle=0):
        return ET.SubElement(world, "body", name=name, pos=fmt((x,y,0)), euler=fmt((0,0,angle)))

    def chair(parent, x, y, angle=0):
        b=ET.SubElement(parent, "body", pos=fmt((x,y,0)), euler=fmt((0,0,angle)))
        box((0,0,.48),(.24,.23,.065),"seat",parent=b)
        box((0,.21,.77),(.25,.045,.24),"seat",parent=b)
        cylinder(b,(0,0,.24),(.045,.22),"metal")
        for a in (0,72,144,216,288):
            dx,dy=.22*math.cos(math.radians(a)),.22*math.sin(math.radians(a))
            geom(b,"capsule",(0,0,0),(.025,),"metal",fromto=fmt((0,0,.08,dx,dy,.08)))
            geom(b,"sphere",(dx,dy,.055),(.045,),"metal")

    def desk(name,x,y,angle=0):
        b=body(name,x,y,angle)
        box((0,0,.76),(.8,.38,.04),"birch",parent=b)
        for dx in (-.69,.69):
            box((dx,0,.36),(.035,.3,.36),"white",parent=b)
        box((.09,.18,1.08),(.31,.035,.22),"metal",parent=b)
        box((.09,.14,1.08),(.285,.008,.193),"screen",parent=b)
        for k in range(3):
            box((.0,.129,1.17-k*.072),(.16-k*.025,.003,.009),"ui",parent=b)
        box((.09,.18,.85),(.045,.035,.085),"metal",parent=b)
        box((.09,.10,.808),(.15,.10,.01),"metal",parent=b)
        box((.05,-.17,.817),(.23,.075,.015),"white",parent=b)
        box((-.48,-.08,.81),(.13,.18,.007),"paper",parent=b,euler="0 0 -12")
        cylinder(b,(.61,-.08,.86),(.045,.055),"blue")
        chair(b,0,-.77,180)

    def plant(name,x,y,scale=1):
        b=body(name,x,y)
        cylinder(b,(0,0,.22*scale),(.20*scale,.22*scale),"pot")
        cylinder(b,(0,0,.65*scale),(.025*scale,.45*scale),"wood")
        for k in range(7):
            angle=k*137.5; dx=.20*math.cos(math.radians(angle)); dy=.20*math.sin(math.radians(angle))
            geom(b,"ellipsoid",(dx*scale,dy*scale,(.70+.075*k)*scale),(.16*scale,.085*scale,.27*scale),"leaf" if k%2 else "leaf-light",euler=fmt((20,-25,angle)))

    # A 16 by 12 metre floor plate, with a roofless, low-front-wall cutaway.
    ET.SubElement(world,"geom",name="original-ground-plane",type="plane",size="8 6 0.1",rgba="0 0 0 0",group="3")
    floor_visual={"contype":"0","conaffinity":"0"}
    box((0,0,-.12),(8.15,6.15,.12),"edge",name="building-plinth",**floor_visual)
    box((0,0,-.005),(8,6,.005),"floor",name="office-floor",**floor_visual)
    box((0,0,.004),(7.9,1.3,.004),"white",name="central-corridor",**floor_visual)
    for y in (-1.2,1.2):
        box((0,y,.012),(7.9,.025,.003),"route",**floor_visual)
    for x in range(-7,8):
        box((x,0,.012),(.16,.015,.003),"route",**floor_visual)
    wall(0,6,8,.10)
    wall(-8,0,.10,6,1.3)
    wall(8,0,.10,6,1.3)
    wall(0,-6,8,.10,.65)
    # Window assemblies on the rear wall are readable in the cutaway.
    for x in (-6,-3,0,3,6):
        box((x,5.885,1.63),(1.05,.025,.65),"edge")
        box((x,5.85,1.63),(.99,.014,.59),"glass")
        box((x,5.83,1.63),(.024,.025,.59),"white")
        box((x,5.83,1.63),(.99,.025,.024),"white")
    # North: a shared workspace, a glass meeting room and a private office.
    for x in (-2.65,2.65): wall(x,3.75,.07,2.15,1.8)
    for left,right,door in ((-8,-2.65,-4.6),(-2.65,2.65,0),(2.65,8,4.6)):
        for a,b in ((left,door-.65),(door+.65,right)):
            wall((a+b)/2,1.5,(b-a)/2,.065,1.10)
            box(((a+b)/2,1.5,1.90),((b-a)/2,.017,.75),"glass")
        for x in (door-.65,door+.65): box((x,1.5,1.18),(.045,.09,1.18),"edge")
        box((door,1.5,2.40),(.70,.09,.06),"edge")
    for x,y,sx,sy in ((-5.3,3.75,2.5,2.1),(0,3.75,2.5,2.1),(5.3,3.75,2.5,2.1),(-4.7,-3.75,3,2.1),(3.6,-3.75,4.1,2.1)):
        box((x,y,.007),(sx,sy,.007),"carpet",**floor_visual)
    for name,x,y,angle in (("workstation-1",-6.4,4.5,0),("workstation-2",-4.4,4.5,0),("workstation-3",-6.4,2.5,180),("workstation-4",-4.4,2.5,180),("private-desk",5.4,4.4,0)):
        desk(name,x,y,angle)
    # Central meeting table and six chairs.
    b=body("meeting-room",0,3.8)
    box((0,0,.77),(1.12,.62,.055),"wood",parent=b)
    for x in (-.8,.8): box((x,0,.36),(.08,.42,.36),"metal",parent=b)
    for x in (-.72,0,.72):
        chair(b,x,-1.0,180); chair(b,x,1.0,0)
    box((0,1.94,1.6),(.85,.025,.48),"screen",parent=b)
    box((-.25,1.91,1.65),(.38,.008,.20),"ui",parent=b)
    box((.43,1.91,1.55),(.16,.008,.30),"blue",parent=b)
    box((-.4,-.1,.84),(.22,.15,.01),"paper",parent=b,euler="0 0 10")
    # South: open collaboration lounge, reception counter, kitchenette.
    wall(-1.5,-4.05,.07,1.9,.9)
    for x,y,angle in ((-4.8,-4.9,0),(-6.7,-3.6,90)):
        b=body(f"lounge-sofa-{x}",x,y,angle)
        box((0,0,.4),(1.18,.43,.23),"sofa",parent=b)
        box((0,.36,.77),(1.18,.10,.3),"sofa",parent=b)
        for dx in (-1.1,1.1): box((dx,0,.63),(.10,.43,.18),"sofa",parent=b)
        for dx in (-.7,0,.7): box((dx,-.02,.64),(.32,.30,.035),"birch",parent=b)
    box((-4.7,-3.2,.40),(.85,.52,.055),"wood")
    for x in (-5.35,-4.05): box((x,-3.2,.20),(.045,.40,.20),"metal")
    box((-4.8,-3.2,.465),(.22,.16,.012),"paper")
    plant("lounge-plant",-2.2,-5.1,1.2)
    b=body("reception",1.2,-2.75)
    box((0,0,.5),(1.65,.48,.5),"white",parent=b)
    box((0,-.50,.55),(1.65,.025,.45),"wood",parent=b)
    for x in [i*.17-1.5 for i in range(19)]: box((x,-.535,.55),(.022,.017,.43),"birch",parent=b)
    box((0,0,1.025),(1.72,.54,.025),"white",parent=b)
    box((.45,.15,1.23),(.30,.025,.17),"screen",parent=b)
    chair(b,.45,.9,0)
    for x in (4.5,5.7,6.9):
        box((x,-5.45,.45),(.57,.40,.45),"blue")
        box((x,-5.45,.925),(.59,.43,.025),"birch")
        box((x,-5.865,.50),(.50,.015,.39),"white")
        box((x,-5.89,.68),(.20,.02,.015),"metal")
    box((4.55,-5.45,1.15),(.22,.25,.20),"metal")
    box((4.55,-5.18,1.18),(.15,.02,.10),"screen")
    cylinder(world,(6.0,-3.5,.77),(.60,.045),"birch")
    cylinder(world,(6.0,-3.5,.36),(.065,.36),"metal")
    for dx,dy,angle in ((-.85,0,90),(.85,0,-90),(0,.85,0)):
        chair(world,6+dx,-3.5+dy,angle)
    # Bookshelves, wall art and a few plants give the rooms distinct uses.
    for x in (3.5,7.05):
        box((x,5.5,.92),(.5,.27,.92),"wood")
        for z in (.15,.65,1.15,1.7):
            box((x,5.20,z),(.48,.03,.025),"birch")
            for k in range(5): box((x-.36+k*.17,5.2,z+.20),(.045,.13,.17),"blue" if k%2 else "white")
    for name,x,y,s in (("workspace-plant",-7.4,5.2,1),("meeting-plant",2.1,5.3,1), ("private-plant",7.3,2.3,1.1),("entrance-plant",-7.25,-1.9,1),("reception-plant",3.1,-1.95,.85)):
        plant(name,x,y,s)
    # The alarm remains an explicit synthetic visual input to the existing rule.
    indicator=ET.SubElement(world,"body",name="indicator",pos="0 0.9 0.65")
    box((0,.08,0),(.39,.18,.39),"edge",name="alarm-cabinet",parent=indicator)
    box((0,-.115,0),(.33,.024,.31),"indicator-material",name="indicator-housing",parent=indicator)
    box((0,-.145,.39),(.28,.018,.035),"white",parent=indicator)
    for x in (-.28,.28): cylinder(indicator,(x,0,-.50),(.035,.16),"metal")
    ET.SubElement(world,"camera",name="camera-a",pos="-3.7 -1.1 2.3",mode="targetbody",target="indicator",fovy="55")
    ET.SubElement(world,"body",name="overview-target",pos="0 0 0.4")
    ET.SubElement(world,"camera",name="overview",pos="12 -16 17",mode="targetbody",target="overview-target",fovy="46")
    ET.SubElement(world,"camera",name="corridor-view",pos="-7.3 -0.9 2.1",mode="targetbody",target="indicator",fovy="65")
    robot=ET.SubElement(world,"body",name="inspection-puck",pos="-2 0 0.2")
    ET.SubElement(robot,"joint",name="travel",type="slide",axis="1 0 0",range="0 4",limited="true",damping="2")
    ET.SubElement(robot,"inertial",pos="0 0 0",mass="1",diaginertia="0.016 0.016 0.016")
    # Preserve the original sphere contact shape and inertia. All bodywork is visual.
    ET.SubElement(robot,"geom",name="original-contact-envelope",type="sphere",size="0.2",rgba="0 0 0 0",mass="0",group="3")
    def visual_box(pos,size,mat): box(pos,size,mat,parent=robot,mass="0",contype="0",conaffinity="0")
    visual_box((0,0,-.01),(.27,.19,.095),"white")
    visual_box((0,0,.09),(.23,.16,.035),"blue")
    visual_box((.275,0,-.015),(.012,.17,.035),"metal")
    visual_box((.29,0,.02),(.008,.09,.012),"ui")
    for x in (-.17,.17):
        for y in (-.20,.20):
            cylinder(robot,(x,y,-.11),(.085,.025),"metal",euler="90 0 0",mass="0",contype="0",conaffinity="0")
    cylinder(robot,(0,0,.18),(.09,.05),"metal",mass="0",contype="0",conaffinity="0")
    cylinder(robot,(-.13,0,.34),(.022,.21),"white",mass="0",contype="0",conaffinity="0")
    visual_box((-.10,0,.56),(.065,.055,.05),"metal")
    # The current ingestion contract requires the original mounting transform.
    ET.SubElement(robot,"camera",name="camera-b",pos="0 -2 1.8",mode="targetbody",target="indicator",fovy="60")
    actuator=ET.SubElement(root,"actuator")
    ET.SubElement(actuator,"velocity",name="drive",joint="travel",kv="40",ctrlrange="-0.5 0.5",ctrllimited="true")
    ET.indent(root,space="  ")
    xml=ET.tostring(root,encoding="utf-8",xml_declaration=True)+b"\n"
    (HERE/"office.xml").write_bytes(xml)
    manifest=json.loads((HERE.parent/"live-office/world.json").read_text())
    manifest["package_revision"]="demo-office-building-v1"
    manifest["description"]="Synthetic furnished office building with open-roof cutaway, workspace, meeting room, private office, lounge, reception and kitchenette. One-axis test robot; no captured real office data."
    manifest["mujoco"]["sha256"]=hashlib.sha256(xml).hexdigest()
    manifest["fidelity"]["geometry"]="Hand-authored 16 by 12 metre furnished office cutaway; native MJCF primitives and procedural materials, illustrative layout and collision geometry"
    manifest["fidelity"]["robot"]="One-axis test robot with visual mobile-base bodywork; original 1 kg sphere collision envelope and 0.016 kg m^2 diagonal inertia, ideal watchdog brake; not differential-drive or Go2 dynamics"
    manifest["scenario"]["name"]="furnished-office-inspection"
    (HERE/"world.json").write_text(json.dumps(manifest,indent=2)+"\n")
    print(json.dumps({"geoms":counter+2,"xml_sha256":manifest["mujoco"]["sha256"]}))


if __name__=="__main__": main()
