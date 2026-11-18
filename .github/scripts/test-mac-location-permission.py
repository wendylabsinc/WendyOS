#!/usr/bin/env python3
"""Run actual Location model/timer with native request submission disabled."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('location_guardian',ROOT/'.github/scripts/run-owned-media-test.py')
OWNER=importlib.util.module_from_spec(spec);spec.loader.exec_module(OWNER)

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('output',type=Path);args=parser.parse_args()
    if sys.platform!='darwin':raise ValueError('native macOS required; never request actual permissions')
    output=args.output.resolve();output.mkdir(mode=0o700,parents=True,exist_ok=False)
    source=ROOT/'swift/WendyAgentMac/Sources/WelcomeAndPermissions.swift';text=source.read_text()
    replacements={'''Bundle.main.object(
                forInfoDictionaryKey: "NSLocationWhenInUseUsageDescription"
            ) as? String''':'LocationFixtureFacts.description',
        'CLLocationManager.locationServicesEnabled()':'LocationFixtureFacts.enabled',
        'NSApplication.shared.isActive':'false'}
    for before,after in replacements.items():
        if text.count(before)!=1:raise ValueError('native fact seam changed: '+before)
        text=text.replace(before,after)
    variants={'normal':text,
        'early-completion':text.replace('self.locationManager.requestWhenInUseAuthorization()','self.locationManager.requestWhenInUseAuthorization()\nself.locationRequest.finish()'),
        'no-timeout':text.replace('private static let locationRequestTimeout = Duration.seconds(60)','private static let locationRequestTimeout = Duration.seconds(3600)')}
    if len(set(variants.values()))!=3:raise ValueError('Location historical control sites changed')
    inputs=[source,ROOT/'swift/WendyAgentMac/Sources/LocationPermissionRequest.swift',ROOT/'swift/Scripts/Tests/LocationPermissionLifecycleTests.swift']
    (output/'sources.json').write_text(json.dumps({str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in inputs},indent=2)+'\n')
    for name,content in variants.items():
        model=output/(name+'.swift');model.write_text(content);binary=output/(name+'.test')
        with (output/(name+'-compile.log')).open('w') as log:
            status=OWNER.run(['xcrun','swiftc','-swift-version','6','-default-isolation','MainActor','-parse-as-library',str(inputs[1]),str(model),str(inputs[2]),'-o',str(binary)],120,cwd=ROOT,stdout=log)
        if status:raise RuntimeError(name+' compile failed')
        selection=[] if name=='normal' else ['pending' if name=='early-completion' else 'timeout']
        with (output/(name+'.log')).open('w') as log:
            status=OWNER.run([str(binary)]+selection,75,cwd=ROOT,stdout=log)
        result=(output/(name+'.log')).read_text();print(result)
        if name=='normal':
            if status or any(marker not in result for marker in ('LOCATION_MODEL_PENDING_DENIAL_RECOVERY_VERIFIED','LOCATION_MODEL_NO_NATIVE_REQUEST_GUARDS_VERIFIED','LOCATION_MODEL_TIMEOUT_RETRY_VERIFIED','LOCATION_MODEL_LIFECYCLE_PASSED')):raise RuntimeError('Location lifecycle failed')
        elif name=='early-completion':
            if status==0 or 'submission falsely completed native Location decision' not in result:raise RuntimeError('early completion control missed intended assertion')
        elif status!=124 or 'LOCATION_MODEL_UNANSWERED_STARTED actual_timeout=60s' not in result or 'LOCATION_MODEL_TIMEOUT_RETRY_VERIFIED' in result:raise RuntimeError('missing timeout control did not reach the unanswered wait')
        binary.unlink()
    print('LOCATION_PERMISSION_MODEL_VERIFIED no TCC requests or installed app changes')
    return 0
if __name__=='__main__':sys.exit(main())
