"""Read the saved room and reuse its camera pose. No typed coordinates."""
import base64
import math
import tempfile
import time
from pathlib import Path
import numpy as np
from core import finite


def contains(triangle, point):
    a,b,c=np.asarray(triangle,dtype=float)
    p=np.asarray(point,dtype=float)
    cross=lambda u,v: u[0]*v[1]-u[1]*v[0]
    area=cross(b-a,c-a)
    if abs(area)<1e-8:return False
    u,v=cross(p-a,c-a)/area,cross(b-a,p-a)/area
    return u>=-1e-6 and v>=-1e-6 and u+v<=1+1e-6


def read_room(room):
    from pxr import Usd, UsdGeom, Gf
    model=base64.b64decode(room.get('model',''),validate=True)
    if not model or len(model)>140<<20:
        raise ValueError('This room has no usable saved model. Scan or import a room in Wendy first.')
    scale=float(room['site']['metresPerModelUnit'])
    if not math.isfinite(scale) or not 0<scale<=1000:raise ValueError('Invalid room scale.')
    architecture,furniture=[],[]
    with tempfile.TemporaryDirectory(prefix='go2-room-') as directory:
        path=Path(directory)/'room.usdz';path.write_bytes(model)
        stage=Usd.Stage.Open(str(path))
        if not stage:raise ValueError('The saved room could not be opened.')
        factor=UsdGeom.GetStageMetersPerUnit(stage)*scale
        up=UsdGeom.GetStageUpAxis(stage)
        transform=UsdGeom.XformCache()
        for prim in stage.Traverse():
            if not prim.IsA(UsdGeom.Mesh):continue
            names=str(prim.GetPath()).split('/')
            objects='Object_grp' in names
            if any(n.startswith('Door') or (objects and n.startswith('Chair')) for n in names):continue
            mesh=UsdGeom.Mesh(prim)
            points=mesh.GetPointsAttr().Get()
            indices=mesh.GetFaceVertexIndicesAttr().Get()
            counts=mesh.GetFaceVertexCountsAttr().Get()
            if points is None or indices is None or counts is None:continue
            if len(points)>1_000_000:raise ValueError('Room mesh is too large.')
            matrix=transform.GetLocalToWorldTransform(prim)
            world=[]
            for point in points:
                p=matrix.Transform(Gf.Vec3d(*point))*factor
                v=[p[0],p[1],p[2]] if up=='Y' else [p[0],p[2],-p[1]]
                finite(v)
                if max(map(abs,v))>1000:raise ValueError('Room model exceeds supported bounds.')
                world.append(v)
            at=0
            for count in counts:
                face=list(indices[at:at+count]);at+=count
                if count<3:continue
                if min(face)<0 or max(face)>=len(world):raise ValueError('Invalid room mesh index.')
                # RoomPlan exports convex triangle/quad faces.
                if count>4:raise ValueError('Triangulate this room model in Wendy before using it.')
                for i in range(1,count-1):
                    triangle=[world[face[0]],world[face[i]],world[face[i+1]]]
                    (furniture if objects else architecture).append(triangle)
            if (len(architecture)+len(furniture))>10000:raise ValueError('Room mesh is too complex for this demo.')
    flat=[t for t in architecture if max(p[1] for p in t)-min(p[1] for p in t)<.02 and abs(np.cross(np.subtract(t[1],t[0]),np.subtract(t[2],t[0]))[1])>.01]
    if not flat:raise ValueError('No floor was found in the saved room. Use a room scan with a floor.')
    floor=min(sum(p[1] for p in t)/3 for t in flat)
    floors=[[[p[0],p[2]] for p in t] for t in flat if abs(sum(p[1] for p in t)/3-floor)<.05]
    walls=[]
    for t in architecture:
        if max(p[1] for p in t)-min(p[1] for p in t)>.3:
            for a,b in zip(t,t[1:]+t[:1]):
                if abs(a[1]-b[1])<.02 and math.dist((a[0],a[2]),(b[0],b[2]))>.1:
                    walls.append([[a[0],a[2]],[b[0],b[2]]])
    return dict(frame=room['site']['id']+'/'+room['site']['modelRevision'],name=room['site']['name'],floor=floor,
                floors=floors,walls=walls,architecture=np.asarray(architecture).flatten().tolist(),
                furniture=np.asarray(furniture).flatten().tolist())


class CameraPose:
    def __init__(self, record, scene, camera):
        self.camera,self.frame=camera,scene['frame']
        lens=record['intrinsics']
        self.width,self.height=int(lens['width']),int(lens['height'])
        self.fx,self.fy,self.cx,self.cy=[float(lens[k]) for k in ('fx','fy','cx','cy')]
        finite([self.fx,self.fy,self.cx,self.cy])
        if min(self.fx,self.fy)<=0 or min(self.width,self.height)<16:raise ValueError('This camera needs lens calibration in Wendy.')
        if record.get('capture_mode') not in ('H.264 %dx%d'%(self.width,self.height),'H264 %dx%d'%(self.width,self.height)):
            raise ValueError('The saved camera calibration belongs to a different capture mode. Align this camera in Wendy.')
        poses=[p for p in record.get('site_poses',[]) if p['model_revision'].lower()==self.frame.split('/')[1].lower() and int(p['valid_until_unix_ms'])>time.time()*1000]
        if len(poses)!=1:raise ValueError('Align this camera with the room in Wendy, then try again.')
        pose=poses[0]
        self.position=np.asarray(pose['position'],dtype=float)
        q=np.asarray(pose['rotation'],dtype=float)
        finite([*self.position,*q])
        if self.position.shape!=(3,) or q.shape!=(4,) or abs(np.linalg.norm(q)-1)>.01:raise ValueError('Invalid saved camera pose.')
        x,y,z,w=q
        self.rotation=np.array([[1-2*(y*y+z*z),2*(x*y-z*w),2*(x*z+y*w)],[2*(x*y+z*w),1-2*(x*x+z*z),2*(y*z-x*w)],[2*(x*z-y*w),2*(y*z+x*w),1-2*(x*x+y*y)]])
        model=record.get('distortion_model','none')
        self.distortion=np.asarray(record.get('distortion',[]),dtype=float)
        if model not in ('none','brown_conrady') or (model=='brown_conrady' and self.distortion.shape!=(5,)):
            raise ValueError('Unsupported camera lens calibration.')
        finite(self.distortion)
        self.floor=scene['floor'];self.floors=scene['floors']
        self.expires=int(pose['valid_until_unix_ms'])/1000
        self.data=dict(mode='camera-pose',camera=camera,frame=self.frame,width=self.width,height=self.height,
                       approximate=lens.get('provenance')!=1,position=pose['position'],
                       forward=self.rotation[:,2].tolist(),expires=self.expires)

    def matrix(self):
        return np.array([[self.fx,0,self.cx],[0,self.fy,self.cy],[0,0,1]],dtype=float)

    def project(self,u,v,width,height):
        import cv2
        if time.time()>self.expires:raise ValueError('Camera alignment expired. Align it again in Wendy.')
        if (width,height)!=(self.width,self.height):raise ValueError('Camera format changed. Align it again in Wendy.')
        finite([u,v])
        if not 0<=u<width or not 0<=v<height:raise ValueError('Object lies outside the camera image.')
        ray=cv2.undistortPoints(np.asarray([[[u,v]]],dtype=float),self.matrix(),self.distortion if len(self.distortion) else None)[0,0]
        direction=self.rotation@np.array([*ray,1.])
        if abs(direction[1])<1e-6:raise ValueError('The object is too close to the camera horizon.')
        distance=(self.floor-self.position[1])/direction[1]
        if not 0<distance<15:raise ValueError('The object does not project onto nearby floor.')
        point=self.position+distance*direction
        result=(float(point[0]),float(point[2]))
        if not any(contains(t,result) for t in self.floors):raise ValueError('The object is outside this room’s floor.')
        return result

    def overlay(self):
        import cv2
        # A metric grid lets the operator check the saved pose without numbers.
        points=np.asarray(self.floors).reshape(-1,2)
        lo,hi=points.min(axis=0),points.max(axis=0)
        lines=[]
        for axis in (0,1):
            other=1-axis
            for value in np.arange(math.ceil(lo[axis]*2)/2,hi[axis],.5):
                previous=None
                for along in np.arange(lo[other],hi[other],.1):
                    p=[0.,0.];p[axis]=float(value);p[other]=float(along)
                    if not any(contains(t,p) for t in self.floors):previous=None;continue
                    optical=self.rotation.T@(np.array([p[0],self.floor,p[1]])-self.position)
                    if optical[2]<=.05:previous=None;continue
                    uv,_=cv2.projectPoints(optical.reshape(1,3),np.zeros(3),np.zeros(3),self.matrix(),self.distortion if len(self.distortion) else None)
                    pixel=uv[0,0].tolist()
                    if not (0<=pixel[0]<self.width and 0<=pixel[1]<self.height):previous=None;continue
                    if previous is not None:lines.append([previous,pixel])
                    previous=pixel
        return lines
