import copy
import math
import unittest
from sim import patrol_exclusions as p
from sim.patrol_nav import nav_parameters

class RestrictedAreasTests(unittest.TestCase):
    def policy(self):
        value=dict(frame=dict(id='office',revision='1'),revision=1,rectangles=[dict(id='area-a',min_mm=[0,0],max_mm=[1000,1000])],sha256='')
        value['sha256']=p.digest(value)
        return value
    def test_digest_and_strict_geometry(self):
        value=self.policy()
        self.assertEqual(p.validate(value,value['frame']),value)
        for mutate in (lambda v:v['rectangles'][0].pop('min_mm'),lambda v:v['rectangles'][0].update(min_mm=[True,0]),lambda v:v['rectangles'][0].update(max_mm=[1000,9000]),lambda v:v.update(sha256='0'*64),lambda v:v.update(revision=2)):
            bad=copy.deepcopy(value);mutate(bad)
            with self.assertRaises(ValueError):p.validate(bad,value['frame'])
        with self.assertRaises(ValueError):p.validate(value,dict(id='office',revision='2'))
    def test_whole_segments_and_heading_bound(self):
        value=self.policy()
        self.assertFalse(p.clear(value,-p.CLEARANCE,.5))
        self.assertTrue(p.clear(value,-p.CLEARANCE-.001,.5))
        self.assertFalse(p.path_clear(value,[[-2,.5],[3,.5]]))
        self.assertFalse(p.path_clear(value,[[-2,-2],[3,3]]))
        self.assertTrue(p.path_clear(value,[[-2,-2],[-2,3],[3,3]]))
    def test_uncertainty_envelope_and_restart_refusal(self):
        covariance=[0.]*36
        covariance[0]=covariance[7]=.01
        radius=p.uncertainty_radius(covariance)
        self.assertAlmostEqual(radius,p.CLEARANCE+.2)
        value=self.policy()
        self.assertTrue(p.clear(value,-p.CLEARANCE-.1,.5))
        self.assertFalse(p.clear(value,-p.CLEARANCE-.1,.5,radius))
        covariance[1]=.2
        self.assertFalse(math.isfinite(p.uncertainty_radius(covariance)))
        self.assertTrue(p.matches(value,value['sha256']))
        self.assertFalse(p.matches(value,''))
        self.assertFalse(p.matches(None,value['sha256']))
        self.assertTrue(p.matches(None,''))

    def test_policy_mask_does_not_modify_localization_map(self):
        grid=dict(resolution=.1,width=60,height=60,origin=[-2,-2,0],data=[0]*3600)
        original=copy.deepcopy(grid)
        mask=p.mask(grid,self.policy())
        self.assertEqual(grid,original)
        self.assertEqual(mask[25*60+25],100)
        self.assertEqual(mask[0],0)
        self.assertGreater(sum(mask),sum(p.mask(grid,self.policy(),robot_centres=False)))
        self.assertEqual(p.mask(grid,None),[0]*3600)
        base={name:{name:{'ros__parameters':{}}} for name in ('global_costmap','local_costmap')}
        config=nav_parameters(base,'original-map.yaml','primitive','bt',[-6,0,0])
        self.assertEqual(config['map_server']['ros__parameters']['yaml_filename'],'original-map.yaml')
        for name in ('global_costmap','local_costmap'):
            self.assertEqual(config[name][name]['ros__parameters']['patrol_keepout']['filter_info_topic'],'/patrol_policy/'+('global' if name=='global_costmap' else 'local')+'/info')

if __name__=='__main__':unittest.main()
