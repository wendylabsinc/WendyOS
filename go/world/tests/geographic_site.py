"""Project retained live-scene captures through the real CLI without a network."""
import datetime
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile


def run():
    root = Path(__file__).resolve().parents[1]
    site = root / 'examples/geography/office.json'
    report_path = root / 'docs/verification/2026-11-09-live-inspection.json'
    report = json.loads(report_path.read_text())
    inspection = report['inspection_result']
    at = datetime.datetime.fromtimestamp(inspection['captured_unix_ms']/1000, datetime.timezone.utc).isoformat()
    with tempfile.TemporaryDirectory(prefix='wendy-geography-') as tmp:
        binary = Path(tmp) / 'world'
        subprocess.run(['go','build','-o',str(binary),'./cmd/world'],cwd=root,check=True)
        def command(verb, **files):
            args = [str(binary),'frame',verb,'--site',str(site),'--json']
            for name,value in files.items():
                if name == 'at': args += ['--at',value]; continue
                path = Path(tmp)/(name+'.json'); path.write_text(json.dumps(value))
                args += ['--'+name,str(path)]
            return subprocess.run(args,cwd=root,capture_output=True,text=True)
        def success(verb, **files):
            p=command(verb,**files)
            assert p.returncode == 0, p.stderr
            return json.loads(p.stdout)
        features=[]; max_error=0
        for sample in inspection['samples']:
            projected=success('project',sample=sample,at=at)
            assert projected['sample']==sample
            location=projected['location']; g=location['geographic']
            assert location['placement']=='illustrative'
            assert location['uncertainty_metres']>=100
            back=success('locate',request={'geographic':g,'to':sample['frame'],'at':at,'uncertainty_metres':sample['uncertainty_m']})
            max_error=max(max_error,max(abs(a-b) for a,b in zip(sample['position_m'],back['position_m'])))
            features.append({'type':'Feature','id':sample['id'],'geometry':{'type':'Point','coordinates':[g['longitude_degrees'],g['latitude_degrees'],g['altitude_metres']]},'properties':projected})
        assert len(features)==4 and max_error<1e-6
        wrong=dict(inspection['samples'][0]);wrong['frame']={'id':'office-floor','revision':'unknown'}
        assert command('project',sample=wrong,at=at).returncode!=0
        wrong=dict(inspection['samples'][0]);wrong['package_sha256']='0'*64
        assert command('project',sample=wrong,at=at).returncode!=0
        assert command('project',sample=inspection['samples'][0],at='2027-01-01T00:00:00Z').returncode!=0
        return {'mode':'offline CLI over retained synthetic inspection captures','inspection_report_sha256':hashlib.sha256(report_path.read_bytes()).hexdigest(),
                'georeference_sha256':hashlib.sha256(site.read_bytes()).hexdigest(),'max_round_trip_error_metres':max_error,
                'unknown_revision_rejected':True,'wrong_package_rejected':True,'expired_placement_rejected':True,
                'map':{'type':'FeatureCollection','features':features},
                'limits':['illustrative anchor, not real office survey','conversion does not authenticate placement or authorize motion',
                          'translation uncertainty only; orientation uncertainty is not modeled','uses retained captures, not a new VM or hardware run']}


if __name__=='__main__':
    print(json.dumps(run(),indent=2))
