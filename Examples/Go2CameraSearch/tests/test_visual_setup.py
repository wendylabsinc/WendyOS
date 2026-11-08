import copy
import json
import math
import time
import base64
from pathlib import Path
import numpy as np
import pytest
from scene import CameraPose, contains, read_room
from vision import parse_result, Tracker, Grounder

SCENE=dict(frame='room/revision',floor=0.,floors=[[[-5,-5],[5,-5],[5,5]],[[-5,-5],[5,5],[-5,5]]])
RECORD=dict(intrinsics=dict(width=640,height=480,fx=500,fy=500,cx=320,cy=240,provenance=1),capture_mode='H.264 640x480',distortion_model='none',site_poses=[dict(model_revision='revision',position=[0,2,0],rotation=[math.sqrt(.5),0,0,math.sqrt(.5)],valid_until_unix_ms=(time.time()+3600)*1000)])


def test_saved_camera_projects_to_room_floor():
    p=CameraPose(copy.deepcopy(RECORD),SCENE,'sensorlink:camera')
    assert p.project(320,240,640,480)==pytest.approx((0,0))
    assert p.project(420,240,640,480)==pytest.approx((.4,0))
    assert p.overlay()
    with pytest.raises(ValueError):p.project(320,240,800,600)
    p.expires=0
    with pytest.raises(ValueError):p.project(320,240,640,480)

@pytest.mark.parametrize('change',['revision','expired','mode','rotation'])
def test_invalid_camera_pose_never_activates(change):
    r=copy.deepcopy(RECORD)
    if change=='revision':r['site_poses'][0]['model_revision']='other'
    if change=='expired':r['site_poses'][0]['valid_until_unix_ms']=0
    if change=='mode':r['capture_mode']='different'
    if change=='rotation':r['site_poses'][0]['rotation']=[0,0,0,0]
    with pytest.raises(ValueError):CameraPose(r,SCENE,'sensorlink:camera')


def test_reads_real_usdz_without_typed_coordinates(tmp_path):
    from pxr import Usd,UsdGeom,Gf
    stage=Usd.Stage.CreateNew(str(tmp_path/'room.usdc'))
    UsdGeom.SetStageMetersPerUnit(stage,1)
    UsdGeom.SetStageUpAxis(stage,UsdGeom.Tokens.y)
    root=UsdGeom.Xform.Define(stage,'/Room')
    root.AddTranslateOp().Set(Gf.Vec3d(1,0,2))
    mesh=UsdGeom.Mesh.Define(stage,'/Room/Floor')
    mesh.GetPointsAttr().Set([(-2,0,-2),(2,0,-2),(2,0,2),(-2,0,2)])
    mesh.GetFaceVertexIndicesAttr().Set([0,1,2,3])
    mesh.GetFaceVertexCountsAttr().Set([4])
    stage.GetRootLayer().Save()
    # A stage layer's content is autodetected, even at the app's temporary .usdz path.
    from pxr import UsdUtils,Sdf
    UsdUtils.CreateNewUsdzPackage(Sdf.AssetPath(str(tmp_path/'room.usdc')),str(tmp_path/'room.usdz'))
    result=read_room(dict(model=base64.b64encode((tmp_path/'room.usdz').read_bytes()).decode(),site=dict(id='room',modelRevision='revision',metresPerModelUnit=1,name='Room')))
    assert len(result['floors'])==2
    assert result['floor']==0
    assert any(contains(t,[1,2]) for t in result['floors'])
    assert len(result['architecture'])==18


def test_vlm_accepts_watering_can_box():
    content=json.dumps({'objects':[{'label':'green watering can','box':[200,200,500,600],'on_floor':True}]})
    result=parse_result(content,1000,800,'watering can')
    assert result['name']=='watering can'
    assert result['box']==pytest.approx([200,160,300,320])

@pytest.mark.parametrize('objects',[[],[{'box':[10,10,800,800],'on_floor':False}],[{'box':[10,10,800,800],'on_floor':True}]*2,[{'box':[500,500,200,200],'on_floor':True}],[{'box':[float('nan'),200,500,600],'on_floor':True}]])
def test_bad_vlm_results_do_not_become_targets(objects):
    with pytest.raises(ValueError):parse_result(json.dumps({'objects':objects}),640,480,'watering can')


def test_tracker_revalidates_current_frame_and_rejects_missing_object():
    rng=np.random.default_rng(42)
    image=np.zeros((480,640,3),np.uint8)
    image[100:200,200:300]=rng.integers(0,255,(100,100,3),dtype=np.uint8)
    tracker=Tracker(image,dict(name='watering can',box=[200,100,100,100]))
    assert tracker.update(image)[0]['box']==[200,100,100,100]
    shifted=np.zeros_like(image);shifted[105:205,205:305]=image[100:200,200:300]
    assert tracker.update(shifted)[0]['box']==[205,105,100,100]
    assert tracker.update(np.zeros_like(image))==[]


def test_flat_crop_cannot_be_tracked():
    with pytest.raises(ValueError):Tracker(np.zeros((100,100,3),np.uint8),dict(name='can',box=[10,10,40,40]))


def test_camera_alignment_service_protojson_timestamp_projects_correctly():
    record=copy.deepcopy(RECORD)
    record['site_poses'][0]['valid_until_unix_ms']=str(int((time.time()+3600)*1000))
    pose=CameraPose(record,SCENE,'sensorlink:camera')
    assert pose.project(320,240,640,480)==pytest.approx((0,0))
    assert pose.overlay()
