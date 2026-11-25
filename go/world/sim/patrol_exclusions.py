"""Signed-patrol restriction geometry, independent of ROS and sensor observations."""
import copy
import hashlib
import math
import re

# Same configured footprint and padding as patrol_nav; the extra 0.20 m is a
# simulation tracking allowance, not a measured localization or safety bound.
FOOTPRINT_RADIUS = math.hypot(.45+.025, .29+.025)
TRACKING_ALLOWANCE = .20
CLEARANCE = FOOTPRINT_RADIUS+TRACKING_ALLOWANCE
TOKEN = re.compile(r'^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$')


def digest(policy):
    text = f"wendy.patrol.exclusions.v1\nnav-xy-z-up\n{policy['frame']['id']}\n{policy['frame']['revision']}\n{policy['revision']}\n"
    for r in policy['rectangles']:
        text += f"{r['id']}:{r['min_mm'][0]},{r['min_mm'][1]}:{r['max_mm'][0]},{r['max_mm'][1]}\n"
    return hashlib.sha256(text.encode()).hexdigest()


def validate(policy, frame, require_digest=True):
    if not isinstance(policy,dict) or set(policy) != {'frame','revision','rectangles','sha256'} or policy['frame'] != frame:
        raise ValueError('Restricted areas require the current frame and exact policy fields')
    if set(frame) != {'id','revision'} or not all(isinstance(v,str) and TOKEN.fullmatch(v) for v in frame.values()):
        raise ValueError('Invalid restricted-area frame')
    if type(policy['revision']) is not int or policy['revision'] != 1 or not isinstance(policy['rectangles'],list) or not 1<=len(policy['rectangles'])<=16:
        raise ValueError('Use revision 1 and 1..16 restricted rectangles')
    ids=set()
    for r in policy['rectangles']:
        if not isinstance(r,dict) or set(r) != {'id','min_mm','max_mm'} or not isinstance(r['id'],str) or not TOKEN.fullmatch(r['id']) or r['id'] in ids:
            raise ValueError('Restricted rectangles require unique IDs and both corners')
        ids.add(r['id'])
        if not all(isinstance(r[k],list) and len(r[k])==2 and all(type(v) is int for v in r[k]) for k in ('min_mm','max_mm')):
            raise ValueError('Rectangle coordinates must be integer millimetres')
        if not (-9000<=r['min_mm'][0]<r['max_mm'][0]<=9000 and -7000<=r['min_mm'][1]<r['max_mm'][1]<=7000):
            raise ValueError('Nonempty rectangle must lie inside the signed mission area')
    expected=digest(policy)
    if require_digest and policy['sha256'] != expected: raise ValueError('Restricted area digest mismatch')
    result=copy.deepcopy(policy);result['sha256']=expected
    return result


def clear(policy,x,y,radius=CLEARANCE):
    if not all(math.isfinite(v) for v in (x,y,radius)): return False
    for r in (policy or {}).get('rectangles',[]):
        dx=max(r['min_mm'][0]/1000-x,0,x-r['max_mm'][0]/1000)
        dy=max(r['min_mm'][1]/1000-y,0,y-r['max_mm'][1]/1000)
        if math.hypot(dx,dy)<=radius: return False
    return True


def uncertainty_radius(covariance):
    if len(covariance)!=36: return math.inf
    a,b,c,d=(covariance[i] for i in (0,1,6,7))
    if not all(math.isfinite(v) for v in (a,b,c,d)) or a<0 or d<0 or abs(b-c)>1e-6 or a*d-b*c < -1e-12: return math.inf
    eigenvalue=(a+d+math.sqrt((a-d)**2+4*b*b))/2
    return CLEARANCE+2*math.sqrt(max(0,eigenvalue))


def path_clear(policy, route, radius=CLEARANCE):
    # Check entire connectors, including smoothed/diagonal segments, at <=2 cm
    # spacing with an extra half-step allowance. Endpoint-only checks are unsafe.
    for a,b in zip(route,route[1:]):
        steps=max(1,math.ceil(math.hypot(b[0]-a[0],b[1]-a[1])/.02))
        if any(not clear(policy,a[0]+(b[0]-a[0])*i/steps,a[1]+(b[1]-a[1])*i/steps,radius+.01) for i in range(steps+1)): return False
    return all(clear(policy,p[0],p[1],radius+.01) for p in route)


def mask(grid, policy, robot_centres=True):
    result=[0]*(grid['width']*grid['height'])
    res=grid['resolution'];ox,oy=grid['origin'][:2]
    # Keepout cells are distinct operator policy, never added to /map or /scan.
    # Filters run after costmap layers. Mark forbidden robot centres using the
    # full heading-independent Go2 bound, plus 0.10 m planning margin. Do not
    # rely on an obstacle inflation layer also inflating a policy filter.
    for r in (policy or {}).get('rectangles',[]):
        padding=CLEARANCE+.10 if robot_centres else TRACKING_ALLOWANCE
        low=[r['min_mm'][i]/1000-padding for i in range(2)]
        high=[r['max_mm'][i]/1000+padding for i in range(2)]
        for y in range(max(0,math.floor((low[1]-oy)/res)),min(grid['height'],math.floor((high[1]-oy)/res)+1)):
            for x in range(max(0,math.floor((low[0]-ox)/res)),min(grid['width'],math.floor((high[0]-ox)/res)+1)):
                result[y*grid['width']+x]=100
    return result


def matches(policy, supplied_digest):
    # A bridge restart loses its frozen policy. Old in-flight goals must fail,
    # even though an ordinary unrestricted fixture may use an empty digest.
    return supplied_digest == policy['sha256'] if policy is not None else supplied_digest in (None,'')
