"""Render anonymous moving mannequins for image-based pose evaluation.

This is a standalone annex using the office material palette, not a change to
the live office. Actors are kinematically posed; no human-dynamics claim is made.
Only camera pixels and calibration belong in detector inputs. Evaluation-only
joint positions are written separately under ground-truth/.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import math
import os
from pathlib import Path
import struct
import xml.etree.ElementTree as ET
import zlib

ROOT = Path(__file__).resolve().parents[1]
OFFICE = ROOT / 'examples/collaborative-office/office.xml'
WIDTH, HEIGHT = 640, 480
CAMERAS = ('people-front', 'people-oblique')
JOINTS = ('left_shoulder', 'right_shoulder', 'left_elbow', 'right_elbow',
          'left_hip', 'right_hip', 'left_knee', 'right_knee')
SITES = ('head', 'neck', 'left_shoulder', 'right_shoulder', 'left_elbow',
         'right_elbow', 'left_wrist', 'right_wrist', 'pelvis', 'left_hip',
         'right_hip', 'left_knee', 'right_knee', 'left_ankle', 'right_ankle')


def text(values):
    return ' '.join(format(float(v), '.9g') for v in values)


def _geom(parent, name, kind, size, material, **attributes):
    return ET.SubElement(parent, 'geom', name=name, type=kind, size=size,
                         material=material, contype='0', conaffinity='0', **attributes)


def _site(parent, name, position):
    ET.SubElement(parent, 'site', name=name, pos=position, size='.004', rgba='0 0 0 0')


def _person(world, number, shirt):
    prefix = 'person-' + str(number)
    body = ET.SubElement(world, 'body', name=prefix, mocap='true', pos='0 0 0')
    _geom(body, prefix+'-torso', 'ellipsoid', '.215 .125 .32', shirt, pos='0 0 1.245')
    _geom(body, prefix+'-waist', 'ellipsoid', '.175 .115 .12', shirt, pos='0 0 .98')
    _geom(body, prefix+'-pelvis', 'ellipsoid', '.18 .12 .13', 'people-trousers', pos='0 0 .89')
    _geom(body, prefix+'-neck', 'capsule', '.055', 'people-skin', fromto='0 0 1.48 0 0 1.57')
    _geom(body, prefix+'-head', 'ellipsoid', '.105 .105 .135', 'people-skin', pos='0 -.008 1.69')
    _geom(body, prefix+'-hair', 'ellipsoid', '.108 .104 .078', 'people-hair', pos='0 .008 1.775')
    _geom(body, prefix+'-nose', 'ellipsoid', '.024 .027 .032', 'people-skin', pos='0 -.109 1.68')
    for side in (-1, 1):
        _geom(body, prefix+'-ear-'+str(side), 'ellipsoid', '.022 .026 .04', 'people-skin', pos=text([side*.106,0,1.69]))
    _site(body, prefix+'/head', '0 0 1.69')
    _site(body, prefix+'/neck', '0 0 1.51')
    _site(body, prefix+'/pelvis', '0 0 .89')
    for label, sign in (('left', 1), ('right', -1)):
        arm = ET.SubElement(body, 'body', name=prefix+'-'+label+'-arm', pos=text([sign*.205,0,1.46]))
        ET.SubElement(arm, 'joint', name=prefix+'/'+label+'_shoulder', type='hinge', axis='1 0 0', range='-60 60')
        _site(arm, prefix+'/'+label+'_shoulder', '0 0 0')
        _geom(arm, prefix+'-'+label+'-sleeve', 'capsule', '.075', shirt,
              fromto=text([0,0,0,sign*.055,0,-.25]))
        forearm = ET.SubElement(arm, 'body', name=prefix+'-'+label+'-forearm', pos=text([sign*.055,0,-.25]))
        ET.SubElement(forearm, 'joint', name=prefix+'/'+label+'_elbow', type='hinge', axis='1 0 0', range='-100 10')
        _site(forearm, prefix+'/'+label+'_elbow', '0 0 0')
        _geom(forearm, prefix+'-'+label+'-forearm-geom', 'capsule', '.052', 'people-skin', fromto='0 0 0 0 -.025 -.245')
        _geom(forearm, prefix+'-'+label+'-hand', 'ellipsoid', '.047 .032 .078', 'people-skin', pos='0 -.025 -.285')
        _site(forearm, prefix+'/'+label+'_wrist', '0 -.025 -.245')
        leg = ET.SubElement(body, 'body', name=prefix+'-'+label+'-thigh', pos=text([sign*.10,0,.89]))
        ET.SubElement(leg, 'joint', name=prefix+'/'+label+'_hip', type='hinge', axis='1 0 0', range='-45 45')
        _site(leg, prefix+'/'+label+'_hip', '0 0 0')
        _geom(leg, prefix+'-'+label+'-thigh-geom', 'capsule', '.085', 'people-trousers', fromto='0 0 -.015 0 0 -.36')
        shin = ET.SubElement(leg, 'body', name=prefix+'-'+label+'-shin', pos='0 0 -.38')
        ET.SubElement(shin, 'joint', name=prefix+'/'+label+'_knee', type='hinge', axis='1 0 0', range='-5 75')
        _site(shin, prefix+'/'+label+'_knee', '0 0 0')
        _geom(shin, prefix+'-'+label+'-shin-geom', 'capsule', '.064', 'people-trousers', fromto='0 0 0 0 0 -.35')
        _geom(shin, prefix+'-'+label+'-shoe', 'ellipsoid', '.075 .145 .065', 'people-shoes', pos='0 -.07 -.415')
        _site(shin, prefix+'/'+label+'_ankle', '0 0 -.35')


def scene_xml(office=OFFICE):
    """Generate a small, static-camera scene; original office bytes stay unchanged."""
    source = ET.fromstring(Path(office).read_bytes())
    root = ET.Element('mujoco', model='wendy-people-pose-annex-v1')
    ET.SubElement(root, 'compiler', angle='degree')
    ET.SubElement(root, 'option', timestep='.01', gravity='0 0 -9.81')
    visual = ET.SubElement(root, 'visual')
    ET.SubElement(visual, 'global', offwidth=str(WIDTH), offheight=str(HEIGHT))
    ET.SubElement(visual, 'headlight', ambient='.30 .30 .30', diffuse='.55 .55 .55', specular='.1 .1 .1')
    ET.SubElement(visual, 'quality', shadowsize='2048', offsamples='4')
    ET.SubElement(visual, 'map', znear='.01', zfar='30')
    assets = ET.SubElement(root, 'asset')
    for material in source.findall('./asset/material'):
        if material.attrib['name'] in ('wall', 'wood', 'stone', 'white', 'metal', 'leaf', 'pot'):
            assets.append(copy.deepcopy(material))
    for name, rgba in [('skin', '.68 .43 .30 1'), ('hair', '.09 .065 .05 1'),
                       ('trousers', '.085 .12 .17 1'), ('shoes', '.035 .04 .045 1'),
                       ('teal', '.13 .43 .43 1'), ('ochre', '.70 .35 .12 1')]:
        ET.SubElement(assets, 'material', name='people-'+name, rgba=rgba, specular='.08', shininess='.1')
    world = ET.SubElement(root, 'worldbody')
    ET.SubElement(world, 'light', pos='-2 -3 5', dir='.2 .4 -1', diffuse='.8 .77 .72', castshadow='true')
    ET.SubElement(world, 'light', pos='3 1 4', dir='-.3 -.2 -1', diffuse='.35 .4 .45')
    _geom(world, 'annex-floor', 'plane', '5 5 .1', 'stone')
    _geom(world, 'annex-back-wall', 'box', '4 .08 1.5', 'wall', pos='0 3 1.5')
    _geom(world, 'annex-back-skirting', 'box', '4 .1 .05', 'white', pos='0 2.9 .05')
    for x in (-2.8, 2.8):
        _geom(world, 'window-'+str(x), 'box', '.55 .02 .6', 'white', pos=text([x,2.9,1.7]))
        _geom(world, 'window-pane-'+str(x), 'box', '.51 .024 .56', 'people-teal', pos=text([x,2.86,1.7]))
    _geom(world, 'background-console', 'box', '.9 .22 .055', 'wood', pos='0 2.6 .84')
    for x in (-.75,.75):
        _geom(world, 'console-leg-'+str(x), 'box', '.025 .18 .4', 'metal', pos=text([x,2.6,.4]))
    ET.SubElement(world, 'body', name='camera-focus', pos='0 .2 .95')
    for name, position, fovy in [(CAMERAS[0], '0 -5.7 2.1', '40'), (CAMERAS[1], '4.1 -4.6 2.4', '43')]:
        ET.SubElement(world, 'camera', name=name, pos=position, mode='targetbody', target='camera-focus', fovy=fovy)
    _person(world, 1, 'people-teal');_person(world, 2, 'people-ochre')
    return ET.tostring(root, encoding='unicode')


def png(rgb):
    height, width, channels = rgb.shape
    if channels != 3 or str(rgb.dtype) != 'uint8' or (width,height)!=(WIDTH,HEIGHT):
        raise ValueError('invalid rendered image')
    def chunk(name, value):
        return struct.pack('>I',len(value))+name+value+struct.pack('>I',zlib.crc32(name+value)&0xffffffff)
    pixels=b''.join(b'\0'+rgb[row].tobytes() for row in range(height))
    return b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',width,height,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(pixels,6))+chunk(b'IEND',b'')


class PeopleScene:
    def __init__(self):
        import mujoco
        import numpy as np
        if mujoco.__version__ != '4.0.0':
            raise ValueError('MuJoCo 4.0.0 required')
        self.mj,self.np=mujoco,np
        self.xml=scene_xml()
        self.model=mujoco.MjModel.from_xml_string(self.xml)
        self.data=mujoco.MjData(self.model)
        self.renderer=None
        self.pose(0)

    def pose(self, seconds):
        if type(seconds) not in (float,int) or not math.isfinite(seconds) or not 0<=seconds<=120:
            raise ValueError('pose time outside 0..120 seconds')
        self.data.time=float(seconds)
        # Stand for the first second, then cross at different depths. This is
        # the actor controller, never a detector/tracker input.
        walked=max(0,seconds-1)
        travel=min(walked*.32,2.5)
        for number,sign in [(1,1),(2,-1)]:
            prefix='person-'+str(number)
            body=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_BODY,prefix)
            mocap=int(self.model.body_mocapid[body])
            self.data.mocap_pos[mocap]=[sign*(-1.25+travel), .10 if number==1 else .70, 0]
            yaw=sign*math.pi/2*min(walked,1)
            self.data.mocap_quat[mocap]=[math.cos(yaw/2),0,0,math.sin(yaw/2)]
            gait=math.sin(walked*2*math.pi*.85) if 0<walked<2.5/.32 else 0
            for side,phase in [('left',1),('right',-1)]:
                angles={'shoulder':-.30*gait*phase,'elbow':-.17-.08*abs(gait),
                        'hip':.22*gait*phase,'knee':.22*max(0,-gait*phase)}
                for joint,angle in angles.items():
                    jid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_JOINT,prefix+'/'+side+'_'+joint)
                    self.data.qpos[self.model.jnt_qposadr[jid]]=angle
        self.mj.mj_forward(self.model,self.data)

    def calibration(self, camera):
        cid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_CAMERA,camera)
        if cid<0:raise ValueError('unknown camera')
        # Camera convention: +X right, +Y down, +Z forward. Scene remains Z-up.
        rotation=self.data.cam_xmat[cid].reshape(3,3)@self.np.diag([1.,-1.,-1.])
        transform=self.np.eye(4);transform[:3,:3]=rotation;transform[:3,3]=self.data.cam_xpos[cid]
        focal=HEIGHT/(2*math.tan(math.radians(float(self.model.cam_fovy[cid]))/2))
        return {'camera_id':camera,'width':WIDTH,'height':HEIGHT,'fx':focal,'fy':focal,
                'cx':WIDTH/2,'cy':HEIGHT/2,'camera_to_scene':transform.reshape(-1).tolist(),
                'frame':{'id':'people-annex','units':'metres','up':'Z','handedness':'right'},
                'pixel_convention':'centres, x right, y down'}

    def render(self,camera):
        if camera not in CAMERAS:raise ValueError('unknown camera')
        if self.renderer is None:self.renderer=self.mj.Renderer(self.model,height=HEIGHT,width=WIDTH)
        self.renderer.update_scene(self.data,camera=camera)
        return png(self.renderer.render())

    def truth(self):
        actors=[]
        for number in (1,2):
            prefix='person-'+str(number);joints={}
            for name in SITES:
                sid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_SITE,prefix+'/'+name)
                joints[name]=[float(x) for x in self.data.site_xpos[sid]]
            actors.append({'actor_id':prefix,'joints_scene_m':joints})
        return {'evaluation_only':True,'time_s':float(self.data.time),'actors':actors}

    def close(self):
        if self.renderer is not None:self.renderer.close();self.renderer=None


def capture(out,times=(0.,.8,2.,4.8,5.2,7.5)):
    out=Path(out)
    if out.exists() or out.is_symlink():raise ValueError('output must be fresh')
    os.umask(0o077);out.mkdir(mode=0o700)
    inputs,truth=out/'detector-input',out/'ground-truth'
    inputs.mkdir(mode=0o700);truth.mkdir(mode=0o700)
    scene=PeopleScene()
    try:
        (out/'scene.xml').write_text(scene.xml)
        items=[]
        for camera in CAMERAS:
            (inputs/(camera+'-calibration.json')).write_text(json.dumps(scene.calibration(camera),indent=2)+'\n')
        for index,seconds in enumerate(times):
            scene.pose(seconds)
            (truth/f'{index:03d}.json').write_text(json.dumps(scene.truth(),indent=2)+'\n')
            for camera in CAMERAS:
                filename=f'{index:03d}-{camera}.png';raw=scene.render(camera)
                (inputs/filename).write_bytes(raw)
                items.append({'image':filename,'camera_id':camera,'sample_time_s':seconds,
                              'sha256':hashlib.sha256(raw).hexdigest(),'bytes':len(raw)})
        request={'schema':'wendy.people-pose.camera-input.v1','simulated':True,'frames':items,
                 'model_sha256':hashlib.sha256(scene.xml.encode()).hexdigest()}
        (inputs/'frames.json').write_text(json.dumps(request,indent=2)+'\n')
        report={'status':'complete','frames':len(items),'actors':2,'rendered_pixels':True,
                'person_detection_verified':False,'kinematic_mannequins':True,
                'ground_truth_separate':True,'original_office_sha256':hashlib.sha256(OFFICE.read_bytes()).hexdigest()}
        (out/'capture.json').write_text(json.dumps(report,indent=2)+'\n')
        return report
    finally:scene.close()


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out',required=True,type=Path)
    arguments=parser.parse_args()
    print(json.dumps(capture(arguments.out)))
