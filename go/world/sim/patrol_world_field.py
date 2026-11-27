"""Eight declared virtual sensors sampled on original articulated physics steps.

This analytic synthetic-tracer field is not a fluid or toxicology model. Reads
return the last immutable sample. Only fixture controls move its source marker.
"""
import copy
import math
import time

class WorldField:
    def __init__(self, mj, model, data, run, model_hash, boot):
        self.mj, self.model, self.data = mj, model, data
        self.run, self.model_hash, self.boot = run, model_hash, boot
        self.frame = dict(id='office-floor', revision='office-'+model_hash[:16])
        self.sequence, self.last_ns, self.sample_ns = 0, 0, 0
        self.last = None
        self.source_id = model.body('patrol-world-source').id
        self.mocap_id = int(model.body_mocapid[self.source_id])
        self.sensors = []
        for i in range(8):
            name = 'world-gas-'+str(i)
            point = data.site_xpos[model.site(name).id]
            self.sensors.append(dict(id=name, source=dict(device=name, app='gas', service='sensor', stream='ppm'),
                                     position_xz=[float(point[0]),float(-point[1])], position_uncertainty_m=0.,
                                     calibration='office-world-'+model_hash[:16],sensor_type='analytic-scalar-v1'))
    def step(self, captured_ms=None):
        stamp = round(float(self.data.time)*1e9)
        if stamp <= self.last_ns:
            self.last = None
            return
        self.last_ns = stamp
        if stamp-self.sample_ns < 250000000:
            return
        self.sample_ns = stamp; self.sequence += 1
        point = self.data.xpos[self.source_id]
        source = [float(point[0]),float(-point[1])]
        values = []
        for sensor in self.sensors:
            dx,dz = sensor['position_xz'][0]-source[0],sensor['position_xz'][1]-source[1]
            along = dx*math.cos(.25)+dz*math.sin(.25)
            cross = -dx*math.sin(.25)+dz*math.cos(.25)
            length = 3 if along >= 0 else 1
            value = 2+120*math.exp(-.5*((along/length)**2+(cross/1.3)**2))
            values.append(dict(sensor=sensor['id'],value_ppm=value,uncertainty_ppm=1.))
        self.last = dict(run_id=self.run,frame=self.frame,model_sha256=self.model_hash,boot_id=self.boot,
                         sequence=self.sequence,simulation_ns=stamp,
                         captured_unix_ms=int(time.time()*1000) if captured_ms is None else captured_ms,
                         method='original-step-office-analytic-field-v1',readings=values)
    def snapshot(self):
        return copy.deepcopy(dict(catalog=self.sensors,sample=self.last))
    def move(self,x,z):
        if any(type(v) not in (int,float) or not math.isfinite(v) for v in (x,z)) or not (-7<=x<=-1 and -3<=z<=3):
            raise ValueError('source fixture requires finite commissioned X/Z bounds')
        self.data.mocap_pos[self.mocap_id] = [x,-z,.5]
        self.mj.mj_forward(self.model,self.data)
        return dict(accepted=True,simulation_fixture=True,source_xz=[x,z])
