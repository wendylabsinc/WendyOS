#!/usr/bin/env python3
"""Open a private room twin using existing Wendy Lite cameras and iPhone scans."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from wendy_site import import_archive, export_archive, list_sites, read_site

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from integration import nativeapp


def private_path(path):
    resolved = path.expanduser().resolve()
    if resolved.is_relative_to(ROOT):
        raise ValueError('Camera runs and evidence must remain outside the repository')
    return resolved


def interrupted(_signal, _frame):
    raise KeyboardInterrupt


def stop(process):
    if process is None or process.poll() is not None:
        return
    process.send_signal(signal.SIGTERM)
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--device', help='wendy-lite:HOST from wendy discover')
    parser.add_argument('--camera-config', type=Path, help='JSON array of cameras with stable id, name, device and camera_id')
    parser.add_argument('--camera-id', type=int, help='SensorLink MJPEG channel ID; required unless listing with --preflight')
    parser.add_argument('--out', type=Path, help='new private run directory outside the repository')
    parser.add_argument('--state-dir', type=Path, help='reuse a previous private state directory to reopen evidence')
    parser.add_argument('--app', type=Path, help='integrated Wendy.app or WendyDesktop executable (also WENDY_DESKTOP_APP)')
    parser.add_argument('--site-dir', type=Path, help='existing Wendy site directory with model.usdz, site.json and visual-map.json')
    parser.add_argument('--list-sites', action='store_true', help='list available Wendy iPhone scans without opening a camera')
    parser.add_argument('--site', help='choose an existing Wendy site by UUID or exact unique name')
    parser.add_argument('--choose-site', action='store_true', help='open the native picker for a shared .wendysite file')
    parser.add_argument('--import-site', type=Path, help='import a shared iPhone .wendysite into the private run')
    parser.add_argument('--export-site', type=Path, help='export the selected site as .wendysite without opening a camera')
    parser.add_argument('--pose-helper', type=Path, help='reuse an existing native Wendy localization helper')
    parser.add_argument('--calibration-profile', type=Path, help='measured camera intrinsics profile; requires --calibration-binding')
    parser.add_argument('--calibration-binding', type=Path, help='explicit hardware and capture-mode binding JSON')
    parser.add_argument('--wendyos-root', type=Path, default=ROOT.parent / 'wendyos', help='WendyOS checkout supplying its existing localization implementation')
    parser.add_argument('--emulated-source', type=Path, help='executable emitting original MJPEG from emulated firmware; requires --simulation-run')
    parser.add_argument('--simulation-run', help='explicit run ID for a live emulated camera; marks all receipts simulated')
    parser.add_argument('--duration', default='1h', help='session duration, e.g. 30m or 1h; maximum 24h')
    parser.add_argument('--headless', action='store_true', help='run capture without opening the native viewer')
    parser.add_argument('--preflight', action='store_true', help='list the selected device cameras without capturing')
    args = parser.parse_args()
    if args.simulation_run is not None and (not args.simulation_run.strip() or len(args.simulation_run.encode()) > 256 or any(c in args.simulation_run for c in '\x00\r\n')):
        parser.error('--simulation-run requires a nonempty run ID of at most 256 bytes without control line breaks')
    if args.emulated_source and (not args.simulation_run or args.camera_config or args.preflight):
        parser.error('--emulated-source requires --simulation-run and a single camera; preflight uses the physical Wendy CLI')
    sites = list_sites(Path.home() / 'Library/Application Support/WendySites')
    if args.list_sites:
        print(json.dumps(sites, indent=2))
        return 0
    if sum(bool(value) for value in (args.site, args.site_dir, args.import_site, args.choose_site)) > 1:
        parser.error('choose one of --site, --site-dir, --import-site or --choose-site')
    if args.site:
        matches = [site for site in sites if site['id'] == args.site or site['name'] == args.site]
        if len(matches) != 1:
            parser.error('site must match exactly one saved UUID or name; use --list-sites')
        args.site_dir = Path(matches[0]['path'])
    if args.choose_site:
        selection = subprocess.run(['osascript', '-e', 'POSIX path of (choose file with prompt "Choose the .wendysite shared from Wendy on iPhone")'],
                                   check=True, capture_output=True, text=True)
        args.import_site = Path(selection.stdout.strip())
    if args.device and not args.device.startswith('wendy-lite:') or (args.camera_id is not None and not 0 <= args.camera_id <= 4294967295):
        parser.error('use a Wendy Lite selector and nonnegative SensorLink channel ID')
    if args.camera_config and (args.device or args.camera_id is not None):
        parser.error('--camera-config replaces --device and --camera-id')
    if bool(args.calibration_profile) != bool(args.calibration_binding):
        parser.error('--calibration-profile and --calibration-binding must be supplied together')
    if args.camera_config and args.calibration_profile:
        parser.error('put calibration_profile and calibration_binding in each --camera-config entry')
    wendy = shutil.which('wendy')
    if wendy is None and not args.export_site and not args.emulated_source:
        # Multi-camera configs may mix bindings; retain the normal CLI requirement.
        parser.error('Wendy CLI is required')
    if args.preflight:
        if not args.device or args.camera_config:
            parser.error('--preflight requires one --device')
        result = subprocess.run([wendy, '--json', 'device', 'camera', 'list', '--device', args.device], timeout=45, stdout=subprocess.PIPE)
        if result.returncode:
            return result.returncode
        cameras = json.loads(result.stdout)
        print(json.dumps(cameras, indent=2))
        if not cameras:
            raise RuntimeError('This device advertises no video channels. No camera was activated.')
        if args.camera_id is not None and not any(row.get('channel_id') == args.camera_id and row.get('video', {}).get('codec') == 'MJPEG' for row in cameras):
            raise RuntimeError('The selected channel is not an advertised MJPEG camera.')
        return 0
    if not args.export_site and not args.camera_config and (not args.device or args.camera_id is None):
        parser.error('--camera-id is required; use --preflight to list the source channels')
    for tool in (() if args.export_site else ('go', 'swiftc')):
        if shutil.which(tool) is None:
            parser.error(tool + ' is required')
    os.umask(0o077)
    cache = private_path(Path.home() / 'Library/Caches/wendy-world')
    cache.mkdir(parents=True, exist_ok=True, mode=0o700)
    if args.out:
        out = private_path(args.out)
        if out.exists():
            parser.error('--out must be a new private directory outside the repository')
        out.mkdir(parents=True, mode=0o700)
    else:
        out = Path(tempfile.mkdtemp(prefix='stationary-camera-', dir=cache))
    if args.import_site:
        args.site_dir = import_archive(args.import_site.expanduser(), out / 'imported-site')
    if args.site_dir:
        read_site(args.site_dir.expanduser())
    if args.export_site:
        if not args.site_dir:
            parser.error('--export-site requires a selected site')
        destination = private_path(args.export_site)
        export_archive(args.site_dir.expanduser(), destination)
        print('Private site export: ' + str(destination))
        return 0
    state = private_path(args.state_dir) if args.state_dir else out / 'state'
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    binary = out / 'world-camera'
    pose_args = []
    pose_helper = None
    if args.pose_helper and not args.site_dir:
        parser.error('--pose-helper requires --site-dir')
    if args.site_dir:
        site = args.site_dir.expanduser().resolve()
        for name in ('site.json', 'model.usdz'):
            if not (site / name).is_file():
                parser.error('The selected site needs ' + name)
        has_references = (site / 'visual-map.json').is_file()
        pose_helper = args.pose_helper.expanduser().resolve() if args.pose_helper else out / 'world-site-localize' if has_references else None
        if has_references and not args.pose_helper:
            print('Building the existing Wendy markerless localization adapter...', flush=True)
            subprocess.run(['bash', str(ROOT / 'adapters/wendy-localization/build.sh'),
                            str(args.wendyos_root.expanduser().resolve()), str(pose_helper)], check=True)
        pose_args = ['--site-dir', str(site)] + (['--pose-helper', str(pose_helper)] if pose_helper else [])
    source_args = ['--device', args.device, '--camera-id', str(args.camera_id)]
    if args.camera_config:
        # Backend validates identities, limits and selectors before opening any source.
        source_args = ['--camera-config', str(args.camera_config.expanduser().resolve())]
    if args.calibration_profile:
        source_args += ['--calibration-profile', str(args.calibration_profile.expanduser().resolve()),
                        '--calibration-binding', str(args.calibration_binding.expanduser().resolve())]
    if args.emulated_source:
        source_args += ['--emulated-source', str(args.emulated_source.expanduser().resolve())]
    if args.simulation_run is not None:
        source_args += ['--simulation-run', args.simulation_run]
    print('Building the stationary camera service...', flush=True)
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/world-camera'], cwd=ROOT, check=True)
    app = nativeapp.executable(args.app) if not args.headless else None
    (out / 'launch.json').write_text(json.dumps({
        'schema': 'wendy.stationary-camera.launch.v1', 'device': args.device,
        'camera_id': args.camera_id, 'state_dir': str(state), 'origin': 'simulated' if args.simulation_run is not None else 'measured',
        'simulation_run': args.simulation_run,
        'emulated_source': str(args.emulated_source.expanduser().resolve()) if args.emulated_source else None,
        'camera_config': str(args.camera_config.expanduser().resolve()) if args.camera_config else None,
        'calibration_profile': str(args.calibration_profile.expanduser().resolve()) if args.calibration_profile else None,
        'calibration_binding': str(args.calibration_binding.expanduser().resolve()) if args.calibration_binding else None,
        'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
        'site_dir': str(args.site_dir.expanduser().resolve()) if args.site_dir else None,
        'pose_helper_sha256': hashlib.sha256(pose_helper.read_bytes()).hexdigest() if pose_helper else None,
        'scope': 'Local room twin, camera evidence and inferred observations; no robot or mission control.'
    }, indent=2) + '\n')
    service = viewer = None
    signal.signal(signal.SIGTERM, interrupted)
    try:
        with (out / 'service.log').open('x') as log:
            service = subprocess.Popen([str(binary), '--state-dir', str(state), '--wendy', wendy or 'wendy', '--duration', args.duration] + source_args + pose_args,
                                       stdout=log, stderr=log)
            profile = state / 'viewer-profile.json'
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                if service.poll() is not None:
                    raise RuntimeError('Camera service exited. Inspect ' + str(out / 'service.log'))
                try:
                    if json.loads(profile.read_text()).get('pid') == service.pid:
                        break
                except (FileNotFoundError, json.JSONDecodeError):
                    pass
                time.sleep(.1)
            else:
                raise RuntimeError('Camera service did not become ready')
            print('Private run: ' + str(out), flush=True)
            print('Camera evidence stays on this Mac. Stop with Ctrl+C or quit this owned Wendy instance.', flush=True)
            if args.headless:
                return service.wait()
            viewer = subprocess.Popen([app, '--camera-profile', str(profile)], start_new_session=True)
            while viewer.poll() is None and service.poll() is None:
                time.sleep(.25)
            return service.returncode if service.returncode is not None else viewer.returncode
    except KeyboardInterrupt:
        return 0
    finally:
        stop(service)
        # This is the directly launched child, never a pre-existing Wendy app.
        stop(viewer)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
