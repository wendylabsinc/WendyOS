"""Verify compiled-grid consistency, isolated planar routes and office rendering."""
import argparse
import hashlib
import json
import math
from pathlib import Path
import struct
import time
import zlib
import xml.etree.ElementTree as ET

import generate

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]


def coplanar_floor_faces(model, data, mujoco):
    """Detect overlapping visible top faces, including a hidden base under finishes."""
    faces=[]
    for i in range(model.ngeom):
        if model.geom_group[i] != 2 or int(model.geom_type[i]) != int(mujoco.mjtGeom.mjGEOM_BOX):
            continue
        rgba=model.geom_rgba[i]
        material=int(model.geom_matid[i])
        if material >= 0 and all(abs(float(a)-b)<1e-6 for a,b in zip(rgba,(.5,.5,.5,1.))):
            rgba=model.mat_rgba[material]
        if rgba[3] <= .001:
            continue
        identity=(1,0,0,0,1,0,0,0,1)
        assert all(abs(float(a)-b)<1e-6 for a,b in zip(data.geom_xmat[i],identity)), "Floor finish must be axis aligned"
        x,y,z=map(float,data.geom_xpos[i]);sx,sy,sz=map(float,model.geom_size[i])
        faces.append((mujoco.mj_id2name(model,mujoco.mjtObj.mjOBJ_GEOM,i),x-sx,x+sx,y-sy,y+sy,z+sz))
    pairs=[]
    for i,a in enumerate(faces):
        for b in faces[i+1:]:
            if (abs(a[5]-b[5])<1e-6 and min(a[2],b[2])-max(a[1],b[1])>1e-6
                    and min(a[4],b[4])-max(a[3],b[3])>1e-6):
                pairs.append([a[0],b[0]])
    return pairs


def png(rgb):
    def chunk(kind, value):
        return struct.pack(">I",len(value))+kind+value+struct.pack(">I",zlib.crc32(kind+value)&0xffffffff)
    h,w,_=rgb.shape
    return b"\x89PNG\r\n\x1a\n"+chunk(b"IHDR",struct.pack(">IIBBBBB",w,h,8,2,0,0,0))+chunk(b"IDAT",zlib.compress(b"".join(b"\0"+row.tobytes() for row in rgb),6))+chunk(b"IEND",b"")


def verify(out):
    import mujoco
    import numpy as np
    if mujoco.__version__ != "4.0.0": raise ValueError("Pinned MuJoCo 4.0.0 required")
    out=out.resolve()
    if not out.is_relative_to(ROOT/".local"): raise ValueError("Use a fresh private .local output directory")
    out.mkdir(parents=True,mode=0o700,exist_ok=False)
    scene=json.loads((HERE/"scene.json").read_text())
    xml=(HERE/scene["model"]["path"]).read_bytes()
    assert hashlib.sha256(xml).hexdigest()==scene["model"]["sha256"]
    model=mujoco.MjModel.from_xml_string(xml.decode())
    assert (model.nq,model.nv,model.nu)==(6,6,6)
    tree=ET.fromstring(xml)
    assert not tree.findall(".//include") and not any("file" in x.attrib for x in tree.iter()), "External scene assets are not part of this fixture"
    supported={int(getattr(mujoco.mjtGeom,"mjGEOM_"+name)) for name in
               ("PLANE","SPHERE","CAPSULE","ELLIPSOID","CYLINDER","BOX")}
    assert all(int(kind) in supported for kind in model.geom_type), "Native export needs primitive geometry"
    presentation=scene["presentation"]
    assert presentation["footprint_m"]==[16,12]
    assert {space["id"] for space in presentation["spaces"]}=={job["id"] for job in scene["jobs"]}
    assert len(presentation["labels"])==6 and all(len(label["position"])==3 and
           all(math.isfinite(x) for x in label["position"]) for label in presentation["labels"])
    categories=presentation["geometry_categories"]
    names=[name for group in categories.values() for name in group]
    compiled_names=[mujoco.mj_id2name(model,mujoco.mjtObj.mjOBJ_GEOM,i) for i in range(model.ngeom)]
    assert len(names)==len(set(names))==model.ngeom and set(names)==set(compiled_names), "Geometry category coverage differs"
    assert all("visual placement only" in sensor["status"] for sensor in presentation["sensors"])
    data=mujoco.MjData(model);mujoco.mj_forward(model,data)
    assert coplanar_floor_faces(model,data,mujoco)==[], "Visible floor faces overlap at equal depth"
    plinth=mujoco.mj_name2id(model,mujoco.mjtObj.mjOBJ_GEOM,"building-plinth")
    ground=mujoco.mj_name2id(model,mujoco.mjtObj.mjOBJ_GEOM,"original-ground-plane")
    assert model.geom_contype[plinth]==model.geom_conaffinity[plinth]==0
    assert abs(data.geom_xpos[ground][2])<1e-12 and abs(data.geom_xpos[plinth][2]+model.geom_size[plinth][2]+.01)<1e-12
    # Reproduce the actual pre-fix source defect to qualify the regression check.
    old_tree=ET.fromstring(xml)
    old_tree.find("worldbody/geom[@name='building-plinth']").set("pos","0 0 -0.12")
    old_model=mujoco.MjModel.from_xml_string(ET.tostring(old_tree,encoding="unicode"))
    old_data=mujoco.MjData(old_model);mujoco.mj_forward(old_model,old_data)
    assert ["building-plinth","office-floor"] in coplanar_floor_faces(old_model,old_data,mujoco)
    bounds=generate.collision_bounds(model,data,mujoco)
    blocked=set(map(tuple,scene["grid"]["blocked"]))
    assert set(generate.occupied(bounds))==blocked,"Grid no longer matches compiled collision geometry"
    free={(x,y) for x in range(generate.WIDTH) for y in range(generate.HEIGHT)}-blocked
    ids={}
    for robot in scene["robots"]:
        rid=robot["id"]
        bid=mujoco.mj_name2id(model,mujoco.mjtObj.mjOBJ_BODY,robot["body"])
        joints=[mujoco.mj_name2id(model,mujoco.mjtObj.mjOBJ_JOINT,n) for n in robot["joints"]]
        actuators=[mujoco.mj_name2id(model,mujoco.mjtObj.mjOBJ_ACTUATOR,n) for n in robot["actuators"]]
        ids[rid]={"body":bid,"q":model.jnt_qposadr[joints],"v":model.jnt_dofadr[joints],"a":actuators}
        assert np.allclose(model.actuator_ctrlrange[actuators],[[-.65,.65]]*2)
        np.testing.assert_allclose(data.xpos[bid],robot["start"]+[.24],atol=1e-12)
    report={"schema_version":"wendy.collaborative-office.verification.v1","mujoco":mujoco.__version__,
        "model_sha256":scene["model"]["sha256"],"scene_sha256":hashlib.sha256((HERE/"scene.json").read_bytes()).hexdigest(),
        "ngeom":model.ngeom,"grid_matches_compiled_collision_geometry":True,"routes":[],
        "primitive_export_compatible":True,"external_assets":0,"spaces":len(presentation["spaces"]),
        "geometry_categories":{name:len(value) for name,value in categories.items()},
        "visible_floor_coplanar_pairs":[],"original_floor_overlap_reproduced":True,
        "limits":["routes tested individually; other robots parked outside building",
          "no simultaneous collision-avoidance or distributed coordination verification",
          "synthetic planar velocity model; no hardware or differential-drive qualification"]}
    started=time.monotonic()
    for robot in scene["robots"]:
        entry=ids[robot["id"]]
        start=generate.nearest(robot["start"],free)
        for job in scene["jobs"]:
            data=mujoco.MjData(model)
            # Isolate static-path qualification from the separate fleet coordinator.
            for index,other in enumerate(scene["robots"]):
                if other["id"]!=robot["id"]:
                    data.qpos[ids[other["id"]]["q"]]=[20+index*3-other["start"][0],20-other["start"][1]]
            mujoco.mj_forward(model,data)
            path=generate.path_to(start,generate.nearest(job["position"],free),free)
            steps=0
            for cell in path[1:]:
                target=np.array(generate.center(cell))
                for step in range(800):
                    position=model.body_pos[entry["body"],:2]+data.qpos[entry["q"]]
                    error=target-position
                    speed=np.linalg.norm(data.qvel[entry["v"]])
                    if np.linalg.norm(error)<.015 and speed<.04: break
                    command=4*error
                    length=np.linalg.norm(command)
                    if length>.5: command*=.5/length
                    data.ctrl[entry["a"]]=command
                    mujoco.mj_step(model,data);steps+=1
                    for contact in data.contact[:data.ncon]:
                        a,b=int(contact.geom1),int(contact.geom2)
                        if entry["body"] not in (model.geom_bodyid[a],model.geom_bodyid[b]): continue
                        other=b if model.geom_bodyid[a]==entry["body"] else a
                        name=mujoco.mj_id2name(model,mujoco.mjtObj.mjOBJ_GEOM,other)
                        if name!="original-ground-plane":
                            raise AssertionError(f'{robot["id"]}/{job["id"]}: unexpected contact with {name}')
                else: raise AssertionError(f'{robot["id"]}/{job["id"]}: waypoint did not settle')
            position=model.body_pos[entry["body"],:2]+data.qpos[entry["q"]]
            error=math.dist(position,job["position"])
            assert error<.02,error
            report["routes"].append({"robot":robot["id"],"job":job["id"],"grid_edges":len(path)-1,
                                      "steps":steps,"final_error_m":error,"static_obstacle_contacts":0})
            print(f'{robot["id"]}/{job["id"]}: reached in {steps} steps',flush=True)
    report["wall_seconds"]=round(time.monotonic()-started,3)
    data=mujoco.MjData(model);mujoco.mj_forward(model,data)
    model.vis.global_.offwidth=1440;model.vis.global_.offheight=900
    with mujoco.Renderer(model,height=900,width=1440) as renderer:
        renderer.update_scene(data,camera="overview")
        overview=png(renderer.render());(out/"overview.png").write_bytes(overview)
        detail=mujoco.MjvCamera()
        detail.lookat[:]=[-4.6,-3.0,.55];detail.distance=7.5;detail.azimuth=118;detail.elevation=-33
        renderer.update_scene(data,camera=detail)
        detail_png=png(renderer.render());(out/"lounge-detail.png").write_bytes(detail_png)
    report["overview_sha256"]=hashlib.sha256(overview).hexdigest()
    report["lounge_detail_sha256"]=hashlib.sha256(detail_png).hexdigest()
    (out/"report.json").write_text(json.dumps(report,indent=2)+"\n")
    print(json.dumps({"passed_routes":len(report["routes"]),"wall_seconds":report["wall_seconds"],"report":str(out/"report.json")}))


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out",type=Path,required=True)
    verify(parser.parse_args().out)
