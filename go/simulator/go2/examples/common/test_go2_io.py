import importlib.util
import math
from pathlib import Path
import struct
from types import SimpleNamespace as NS

import pytest

from go2_io import cloud_scan, sensor_wall_ns, sport_request


def cloud(points, *, endian=False, padding=0, rows=1):
    width = len(points) // rows
    data = b''.join(b''.join(struct.pack(('>' if endian else '<') + 'fff', *p)
                    + b'\0' * 20 for p in points[row*width:(row+1)*width])
                    + b'\0' * padding for row in range(rows))
    return NS(header=NS(stamp=NS(sec=10, nanosec=0), frame_id='base_link'),
        width=width, height=rows, point_step=32, row_step=width*32+padding,
        is_bigendian=endian, data=data,
        fields=[NS(name=n, offset=i*4, count=1, datatype=7) for i,n in enumerate('xyz')])


def circle(radius=3):
    return [(radius*math.cos(-math.pi+i*math.pi/36),
             radius*math.sin(-math.pi+i*math.pi/36), 0.08) for i in range(72)]


@pytest.mark.parametrize('endian,rows,padding', [(False,1,0),(True,2,8)])
def test_padded_cloud_projects_obstacles_in_body_coordinates(endian,rows,padding):
    message=cloud(circle(),endian=endian,rows=rows,padding=padding)
    scan=cloud_scan(message)
    assert scan.header.stamp is message.header.stamp
    assert scan.header.frame_id=='base_link'
    assert len(scan.ranges)==72
    assert scan.ranges==pytest.approx([3]*72)


def test_unknown_sectors_and_floor_are_not_fabricated_clear_space():
    points=circle()
    points[36]=(0.4,0,0.1)
    points[0]=(-1,0,-0.31)
    points[1]=(float('nan'),0,0)
    scan=cloud_scan(cloud(points))
    assert scan.ranges[36]==pytest.approx(.4)
    assert math.isinf(scan.ranges[0])
    assert math.isinf(scan.ranges[1])


@pytest.mark.parametrize('change', [lambda m:setattr(m.header,'frame_id','odom'),
    lambda m:setattr(m,'row_step',1),lambda m:setattr(m,'data',m.data[:-1]),
    lambda m:setattr(m.fields[0],'offset',31),lambda m:setattr(m.fields[0],'datatype',2),
    lambda m:setattr(m.fields[0],'name','z')])
def test_malformed_cloud_rejected(change):
    message=cloud(circle());change(message)
    with pytest.raises(ValueError):cloud_scan(message)


def test_clock_offset_is_explicit_and_does_not_replace_capture_age(monkeypatch):
    assert sensor_wall_ns(10000000000)==10000000000
    monkeypatch.setenv('GO2_SENSOR_CLOCK_OFFSET_SECONDS','-8.5')
    assert sensor_wall_ns(10000000000)==1500000000
    monkeypatch.setenv('GO2_SENSOR_CLOCK_OFFSET_SECONDS','nan')
    with pytest.raises(ValueError):sensor_wall_ns(0)


def test_native_move_is_bounded_and_has_no_posture_or_lease_changes():
    def request():return NS(header=NS(identity=NS(),policy=NS()),parameter='')
    message=sport_request(request,.3,-.2,.4)
    assert message.header.identity.api_id==1008
    assert message.header.policy.noreply
    for values in [(1,0,0),(0,1,0),(0,0,2),(float('nan'),0,0),(True,0,0)]:
        with pytest.raises(ValueError):sport_request(request,*values)


def test_standalone_copies_match_shared_source():
    root=Path(__file__).resolve().parents[2]
    shared=Path(__file__).with_name('go2_io.py').read_bytes()
    for app in ('patrol','roam','teleop','sensors'):
        assert (root/'examples'/app/'go2_io.py').read_bytes()==shared


def test_fresh_attitude_levels_cloud_without_turning_heading():
    # A 10-degree nose-up robot sees a level ring tilted in its body coordinates.
    pitch = math.radians(10)
    points = [(x*math.cos(pitch), y, x*math.sin(pitch)) for x,y,z in circle()]
    message = cloud(points)
    orientation = NS(x=0, y=math.sin(pitch/2), z=0, w=math.cos(pitch/2))
    odometry = NS(header=NS(stamp=message.header.stamp, frame_id='odom'), child_frame_id='base_link',
                  pose=NS(pose=NS(orientation=orientation)))
    scan = cloud_scan(message, odometry)
    assert scan.header.frame_id == 'base_footprint'
    assert scan.ranges == pytest.approx([3]*72)
    odometry.header.stamp = NS(sec=9, nanosec=0)
    with pytest.raises(ValueError, match='100 ms'):
        cloud_scan(message, odometry)
