#!/usr/bin/env python3
"""Generate a physical Lite enrollment/recovery rehearsal for audit #2429/#2263.

No device, account, firmware or enrollment is changed by this program.

Inventory JSON:
{"tenant_uuid":"<test tenant UUID>","cloud_host":"<host:port>","login_target":"production","boards":[
 {"role":"current","hardware_id":"<USB hardware ID>","serial":"<label>",
  "usb_selector":"wendy-lite:<exact port>","version":"<firmware version>",
  "firmware":"<local release artifact>","setup_recipe":"<release flashing instructions file>"},
 {"role":"supported-older","hardware_id":"<different hardware ID>","serial":"<label>",
  "usb_selector":"wendy-lite:<exact port>","version":"<older supported version>",
  "firmware":"<local release artifact>","setup_recipe":"<release flashing instructions file>"}]}

Generate:
  python lite-identity-rehearsal.py plan --cli /path/to/wendy --inventory inventory.json --output evidence
After signing in and making the first authenticated Lite request, export ONLY
public certificates from the isolated test configuration:
  python lite-identity-rehearsal.py certificates --config-dir evidence/private-config --tenant-uuid UUID --output evidence/public-certificates
Never attach private-config or a full config.json to a PR.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[3]


def line(value, field):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError(f'{field} must be a nonempty single-line string')
    if '<' in value or '>' in value:
        raise ValueError(f'{field} still contains a placeholder')
    return value


def artifact(value, field):
    path = Path(line(value, field)).expanduser().resolve(strict=True)
    if not path.is_file() or not path.stat().st_size:
        raise ValueError(f'{field} must be a nonempty file')
    return {'path': str(path), 'sha256': hashlib.sha256(path.read_bytes()).hexdigest()}


def private_write(path, data):
    with path.open('x', encoding='utf-8') as output:
        os.chmod(path, 0o600)
        output.write(data)


def public_pem(value):
    if not isinstance(value, str) or not re.fullmatch(
        r'(?:-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----\s*)+', value
    ):
        raise ValueError('expected certificate PEM only; refusing to export other configuration material')
    return value


def certificates(args):
    tenant = str(uuid.UUID(args.tenant_uuid))
    config = json.loads((args.config_dir / 'config.json').read_text())
    candidates = []
    for auth in config.get('auth', []):
        for certificate in auth.get('certificates', []):
            if certificate.get('principalUri', '').startswith(f'spiffe://wendy.sh/tenant/{tenant}/operator/'):
                candidates.append((auth, certificate))
    if len(candidates) != 1:
        raise ValueError('expected exactly one operator certificate for the test tenant in the isolated configuration')
    auth, certificate = candidates[0]
    operator = public_pem(certificate.get('pemCertificate'))
    lite = public_pem(auth.get('liteCertificate'))
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    private_write(args.output / 'operator.pem', operator)
    private_write(args.output / 'lite-operator.pem', lite)
    print('Exported two public certificate files. No private keys or tokens were exported.')


def plan(args):
    inventory = json.loads(args.inventory.read_text())
    tenant = str(uuid.UUID(inventory['tenant_uuid']))
    cloud = line(inventory['cloud_host'], 'cloud_host')
    login_target = inventory['login_target']
    if login_target not in ('production', 'development'):
        raise ValueError('login_target must be production or development')
    boards = inventory['boards']
    if len(boards) != 2 or {b['role'] for b in boards} != {'current', 'supported-older'}:
        raise ValueError('provide exactly one current and one supported-older physical board')
    if len({b['hardware_id'] for b in boards}) != 2 or len({b['usb_selector'] for b in boards}) != 2:
        raise ValueError('boards need distinct hardware IDs and USB selectors')
    if boards[0]['version'] == boards[1]['version']:
        raise ValueError('current and supported-older firmware versions must differ')
    normalized = []
    for board in boards:
        item = {key: line(board[key], key) for key in ['role', 'hardware_id', 'serial', 'usb_selector', 'version']}
        if not item['usb_selector'].startswith('wendy-lite:'):
            raise ValueError('use each exact Wendy Lite USB selector, never an automatic or Cloud selector')
        item['firmware'] = artifact(board['firmware'], 'firmware')
        item['setup_recipe'] = artifact(board['setup_recipe'], 'setup_recipe')
        recipe = Path(item['setup_recipe']['path']).read_text()
        if len(recipe.strip()) < 40:
            raise ValueError('setup_recipe must include the release-specific flash commands, target checks and recovery steps')
        normalized.append(item)
    cli = artifact(str(args.cli), 'cli')
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True).strip())
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    (output / 'private-config').mkdir(mode=0o700)
    windows = platform.system() == 'Windows'
    quote = (lambda s: "'" + s.replace("'", "''") + "'") if windows else shlex.quote
    command = lambda *xs: ('& ' if windows else '') + ' '.join(quote(str(x)) for x in xs)
    cli_command = lambda *xs: command(cli['path'], *xs)
    config_env = ("$env:WENDY_CONFIG_DIR = " if windows else 'export WENDY_CONFIG_DIR=') + quote(str(output / 'private-config'))
    cases = []
    lines = [f'''Lite identity physical rehearsal, audit #2429 and #2263
Source commit: {commit}; dirty checkout: {dirty}
CLI: {cli['path']}; SHA-256: {cli['sha256']}
Host: {platform.platform()}
Test tenant: {tenant}; Cloud: {cloud}

This file is a human procedure. The generator has not run any hardware test.
All case results in evidence.json start as not_run. A dirty checkout is suitable
only for preview; regenerate from a clean committed checkout before execution.

Preparation
1. Reserve both disposable boards and the test tenant. Confirm the owner permits
   enrolling and later unenrolling each exact hardware ID. Do not use production
   assets. Back up their Wi-Fi/app settings and record the restoration procedure.
2. Install each recorded release using its copied setup recipe. Verify the recipe
   targets the exact chip, flash layout and serial before approving any erase.
   Record the firmware artifact hash and actual running version. Do not pass an
   ESP32 target through --device-type or assume --version pins Lite firmware.
   A missing or unsupported older release makes that matrix cell unavailable.
3. Attach only the selected board while enrolling it. Keep native USB and any
   USB-UART port labels distinct. Record the host, USB driver and physical port.
4. Use a fresh terminal and this isolated configuration:
   {config_env}
   {cli_command('--version')}
   {cli_command('auth', 'login', '--' + login_target, '--cloud-grpc', cloud)}
   Choose the test tenant and the recorded Cloud environment. Complete browser
   sign-in without capturing the credential entry or tokens in a transcript.
   {cli_command('--json', 'auth', 'status')}
   Verify exactly the expected tenant/context. Do not use legacy login. If the
   session points elsewhere, stop and select the intended context before testing.
5. Start a terminal transcript for the remaining CLI output. Keep private-config
   outside the evidence attachment. Record start/end UTC and exit status for every
   command. Status/info calls must finish in 30 seconds; enrollment in five minutes;
   Cloud presence and reconnect in two minutes. On timeout press Ctrl+C and record
   failure, then preserve USB logs before recovery. A subsequent retry cannot turn
   an earlier failed case into a pass.
''']
    for board in normalized:
        role, selector = board['role'], board['usb_selector']
        recipe_name = role + '-setup.txt'
        private_write(output / recipe_name, Path(board['setup_recipe']['path']).read_text())
        name = 'regression-lite-' + role
        lines.append(f'''Board {role}: serial {board['serial']}; hardware ID {board['hardware_id']}
USB selector: {selector}; required version: {board['version']}
Firmware SHA-256: {board['firmware']['sha256']}
Setup recipe: {recipe_name}; SHA-256: {board['setup_recipe']['sha256']}

A. Initial enrollment and authenticated access
   {cli_command('--json', 'device', 'enrollment-status', '--device', selector)}
   Check hardwareId exactly equals {board['hardware_id']}. Record name, enrolled,
   tenantId and deviceId. If already enrolled, stop until the owner approves the
   explicit USB unenroll of THIS board; never infer authorization from this plan.
   {cli_command('device', 'wifi', 'connect', '--device', selector)}
   Enter the test Wi-Fi credentials outside the transcript, then resume recording.
   {cli_command('device', 'enroll', '--device', selector, '--name', name, '--cloud-grpc', cloud)}
   {cli_command('--json', 'device', 'enrollment-status', '--device', selector)}
   {cli_command('--json', 'cloud', 'discover')}
   Require enrolled=true, tenantId={tenant}, a nonempty device UUID and exactly one
   matching Cloud asset. Record its asset UUID and exact cloud:// selector. Use that
   recorded selector in the next command, never select by an ambiguous display name:
   {cli_command('--json', 'device', 'info', '--read-only', '--device', 'RECORDED_CLOUD_SELECTOR')}
   Require successful authenticated device output and the recorded firmware version.
   A Cloud list row alone is not a successful relay connection.
   Before C, perform the public certificate check below and retain this baseline.
   Use a distinct output directory for each board and each later comparison.

B. Cold restart
   Disconnect all board power for ten seconds, then reconnect the same USB cable.
   Wait at most two minutes; rediscover the USB port if its name changed and compare
   hardwareId before accepting it. Repeat enrollment-status and the exact Cloud info
   call from A. The same tenantId/deviceId/asset UUID must return, with no new login or
   enrollment and no duplicate Cloud asset. Record the time to authenticated recovery.

C. Cloud removal followed by USB identity recovery
   Stop here until the owner authorizes removal of the recorded disposable asset.
   Record: REMOVE {board['hardware_id']} ASSET <the exact UUID from A>.
   {cli_command('cloud', 'device', 'unenroll', '--device', 'RECORDED_ASSET_UUID')}
   Read USB enrollment-status again. The local identity must remain enrolled.
   Re-run the enroll command from A. It must refuse the stale local enrollment and
   direct the operator to USB recovery; it must not create a second Cloud asset.
   {cli_command('device', 'unenroll', '--device', selector)}
   Confirm only this board. Wait for its reboot and read enrollment-status again.
   Require enrolled=false with cleared deviceId/tenantId, while Wi-Fi/app settings
   survive. Firmware that explicitly lacks reset support must refuse with update
   guidance and must not report success; record that compatibility result separately.

D. Re-enrollment after recovery
   If C cleared the identity, repeat A and B. Record the newly issued UUID, compare it
   with Cloud, and require authenticated relay access after restart. Compare public
   operator certificate fingerprints before and after; Lite certificate acquisition
   must not replace the ML-DSA sign-in identity. If older firmware cannot reset, follow
   its copied recovery recipe, record the required update, and keep C/D unavailable
   for that original version rather than claiming the updated version proves them.
''')
        for suffix in ['enroll-and-relay', 'cold-restart', 'cloud-removal-usb-recovery', 'reenroll']:
            cases.append({'id': role + '-' + suffix, 'audit_entries': ['2429', '2263'], 'status': 'not_run',
                          'started_utc': None, 'finished_utc': None, 'exit_codes': [], 'evidence_files': [],
                          'hardware_id': board['hardware_id'], 'firmware_version': board['version'],
                          'tenant_uuid': tenant, 'device_uuid_before': None, 'device_uuid_after': None,
                          'cloud_asset_uuid': None, 'notes': ''})
    lines.append(f'''Public certificate check for #2263
After the first authenticated Lite request, run:
  {command('python3', Path(__file__).resolve(), 'certificates', '--config-dir', output / 'private-config', '--tenant-uuid', tenant, '--output', output / 'public-certificates')}
  openssl x509 -in public-certificates/operator.pem -noout -text
  openssl x509 -in public-certificates/lite-operator.pem -noout -text
Run those openssl commands from {output}. Use an OpenSSL version able to decode
ML-DSA certificates. Require the primary operator public key to be ML-DSA-65 and
the Lite operator public key to be P-256. Both SANs must identify the expected
operator in tenant {tenant}. Record SHA-256 fingerprints, algorithm and SAN only.
Use a new output directory after re-enrollment and confirm the primary operator
identity is unchanged. Missing Lite certificate issuance or an undecodable algorithm
is missing evidence, not a pass. Never upload keys, config.json or OAuth tokens.

Compatibility and cleanup
- Repeat this matrix on macOS, Windows and Linux hosts supported by the selected
  firmware. Record the exact native USB driver and the USB-UART bridge separately.
- If an older build lacks UUID enrollment, require an explicit update refusal before
  Cloud allocation; preserve its USB status and Cloud inventory before/after. It does
  not satisfy the supported-older successful-enrollment cell.
- Read device logs over the exact USB selector for up to 60 seconds on failure:
  {cli_command('device', 'logs', '--device', 'RECORDED_USB_SELECTOR')}
  Stop with Ctrl+C. Do not apply WendyOS-only --tail/--no-follow flags to Lite logs.
- With explicit owner approval, USB-unenroll each test board. Remove only its recorded
  disposable Cloud asset if cleanup is incomplete. Restore firmware and Wi-Fi/app
  settings using its setup recipe; verify the final local status and Cloud absence.
- Sign out of the isolated configuration with {cli_command('auth', 'logout')}.
  Securely remove private-config after the owner accepts cleanup. Keep sanitized
  transcripts, public certificate metadata, firmware hashes and evidence.json.
- Fill each case result as pass, fail or unavailable with the exact evidence files.
  The script generator never marks a test passed. Record exceptions per firmware
  and host; no hardware run has been performed merely by creating this directory.
''')
    private_write(output / 'instructions.txt', '\n'.join(lines))
    private_write(output / 'evidence.json', json.dumps({'audit_entries': ['2429', '2263'], 'source_commit': commit,
        'source_dirty': dirty, 'host': platform.platform(), 'cli': cli, 'boards': normalized,
        'hardware_execution': 'not_run', 'authorization': None, 'cases': cases}, indent=2) + '\n')
    print(f'Wrote instructions and {len(cases)} unexecuted cases to {output}')


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    modes = parser.add_subparsers(dest='mode', required=True)
    generate = modes.add_parser('plan')
    generate.add_argument('--cli', required=True, type=Path)
    generate.add_argument('--inventory', required=True, type=Path)
    generate.add_argument('--output', required=True, type=Path)
    export = modes.add_parser('certificates')
    export.add_argument('--config-dir', required=True, type=Path)
    export.add_argument('--tenant-uuid', required=True)
    export.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    try:
        (plan if args.mode == 'plan' else certificates)(args)
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        parser.error(str(error))


if __name__ == '__main__':
    main()
