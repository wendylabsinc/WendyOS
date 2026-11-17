#!/usr/bin/env python3
"""Synthetic process/record fixtures only; never call native hardware commands."""
from datetime import datetime, timezone, timedelta
import importlib.util
import json
import re
from pathlib import Path
import signal
import subprocess
import time
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

OLD = load("camera_base_tests", HERE / "mac_camera_rehearsal_test.py")
GEN = load("camera_hardware", HERE.parent / "regression-coverage/manual/mac-camera-hardware-rehearsal.py")

class MacHardwareRehearsalTests(unittest.TestCase):
    def prepare(self, root):
        inventory, raw = OLD.MacCameraRehearsalTests().inventory(root)
        raw["camera"].update(unique_id="0x123400002bdf0102", name="SYNTHETIC TOPDON")
        raw["actor_checks"] = True
        Path(raw["records"]["build"]).write_text("SYNTHETIC ONLY -enable-actor-data-race-checks " + raw["candidate_source_commit"])
        def record(name, data):
            path = root / name
            path.write_text(json.dumps(data))
            return str(path)
        raw["thermal"] = {
            "format_record": record("formats.json", {"camera_unique_id": raw["camera"]["unique_id"],
                "dimensions": json.loads(GEN.FIXTURE.read_text())["dimensions"]}),
            "provenance_record": record("format-provenance.json", {"kind": "SYNTHETIC OFFLINE FIXTURE, never captured"})}
        control = {"name": "brightness", "value": 4, "minimum": 0, "maximum": 8, "step": 2, "default_value": 4, "mutable": True}
        store = root / "account/Library/Application Support/sh.wendy.WendyAgentMac/camera-controls/controls.json"
        raw["uvc"] = {"camera": {"unique_id": "0x123500000c456366", "name": "SYNTHETIC UVC", "id": 2, "physical_label": "inert fixture"},
            "control_name": "brightness", "permitted_value": 6,
            "controls_record": record("controls.json", [control]),
            "camera_list_record": record("camera-list.json", [{"id": 1, "name": raw["camera"]["name"], "transport": 1, "online": True},
                {"id": 2, "name": "SYNTHETIC UVC", "transport": 1, "online": True}]),
            "permission_record": record("permission.json", {"synthetic": "no hardware authorization"}),
            "store_record": record("store.json", {"path": str(store), "exists": False, "sha256": None})}
        state, events = root / "inert-state.json", root / "inert-events.jsonl"
        state.write_text(json.dumps(control))
        # A real child process implementing only this inert file protocol. It
        # cannot reach devices, permissions, signing tools or user preferences.
        cli = Path(raw["cli_binary"])
        cli.write_text(f'''#!{sys.executable}
import json,os,sys
from pathlib import Path
state=Path({str(state)!r});events=Path({str(events)!r})
role=Path(os.environ['WENDY_CONFIG_DIR']).name
args=sys.argv[1:]
with events.open('a') as out:out.write(json.dumps([role,args])+'\\n')
if role!='operator':
 print('SYNTHETIC PermissionDenied',file=sys.stderr);sys.exit(1)
if 'list' in args:
 print(Path({raw['uvc']['camera_list_record']!r}).read_text());sys.exit(0)
row=json.loads(state.read_text())
if 'set-control' in args:
 assert '--no-persist' in args
 value=int(next(a.split('=')[1] for a in args if a.startswith('brightness=')))
 row['value']=value;state.write_text(json.dumps(row));print('synthetic applied')
else:print(json.dumps([row]))
''')
        inventory.write_text(json.dumps(raw))
        return inventory, raw, state, events

    def generate(self, inventory, directory):
        with patch.object(GEN.BASE.subprocess, "check_output", side_effect=OLD.git_output), \
             patch.object(GEN.BASE.subprocess, "Popen", side_effect=AssertionError("generation launched a process")):
            return GEN.generate(inventory, directory)

    def confirmation(self, root, plan):
        raw = plan["inventory"]
        data = {"captured_at": datetime.now(timezone.utc).isoformat(), **raw["mac"],
                "bundle_sha256": plan["bundle"]["sha256"], "camera_unique_id": raw["uvc"]["camera"]["unique_id"],
                "camera_id": 2, "listener_pid": 123, "listener_start": "synthetic start", "observer": "synthetic", "ownership_confirmed": True}
        path = root / "confirmation.json"
        path.write_text(json.dumps(data))
        return path

    def observe(self, root, confirmation):
        with patch.object(GEN.BASE.platform, "system", return_value="Darwin"), \
             patch.object(GEN.BASE.platform, "node", return_value="fixture-mac"):
            GEN.observe_controls(root / "plan", confirmation, root / "observation")

    def test_historical_format_transcription_matches_recorded_swift_test(self):
        fixture = json.loads(GEN.FIXTURE.read_text())
        source = (GEN.BASE.ROOT / fixture['source_file']).read_text()
        thermal = source.split('let formats = sizes([', 1)[1].split('])', 1)[0]
        dimensions = [[int(w), int(h)] for w,h in re.findall(r'\((\d+), (\d+)\)', thermal)]
        self.assertEqual(fixture['dimensions'], dimensions)
        self.assertIn('captureFormatChoice(formats) == .pinned(8)', source)
        self.assertEqual(fixture['expected_dimensions'], dimensions[fixture['expected_index']])
        self.assertEqual(fixture['provenance_kind'], 'source_reported_device_formats')
        self.assertEqual(fixture['physical_execution'], 'not_run')

    def test_generation_preserves_prior_cases_and_pins_source_reported_provenance(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, raw, _, _ = self.prepare(root)
            plan = self.generate(inventory, root / "plan")
            GEN.BASE.verify(plan)
            self.assertEqual(plan["audit_entries"], ["1924", "2011", "2315", "2222"])
            self.assertEqual(len(plan["cases"]), 17)
            self.assertTrue(all(c["status"] == "not_run" for c in plan["cases"]))
            self.assertEqual(plan["hardware_execution"], "not_run")
            self.assertEqual(plan["cleanup"]["status"], "not_run")
            procedure = (root / "plan/hardware-procedure.txt").read_text()
            self.assertIn("not a new raw hardware capture", procedure)
            self.assertIn("512x384", procedure)
            self.assertIn("RPC listing\ndoes not expose uniqueID", procedure)
            self.assertIn("--no-persist", procedure)
            self.assertTrue(all(p.stat().st_mode & 0o777 == 0o600 for p in (root / "plan").iterdir()))

    def test_rejects_unsafe_unknown_or_uninstrumented_inputs_before_output(self):
        mutations = [lambda r: r["uvc"].update(control_name="zoom_absolute"),
                     lambda r: r["uvc"].update(permitted_value=8),
                     lambda r: r["uvc"].update(permitted_value=True),
                     lambda r: r["uvc"]["camera"].update(unique_id="camera-name"),
                     lambda r: r["uvc"]["camera"].update(id=1),
                     lambda r: r.update(actor_checks=False),
                     lambda r: Path(r["records"]["build"]).write_text("uninstrumented"),
                     lambda r: Path(r["thermal"]["format_record"]).write_text('{}'),
                     lambda r: Path(r["uvc"]["controls_record"]).write_text('[{"name":"brightness","mutable":false}]')]
        for mutate in mutations:
            with self.subTest(mutate=mutate), tempfile.TemporaryDirectory() as temp:
                root = Path(temp); inventory, raw, _, _ = self.prepare(root)
                mutate(raw); inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):self.generate(inventory, root / "plan")
                self.assertFalse((root / "plan").exists())

    def test_changed_format_permission_or_store_record_invalidates_plan(self):
        for key in ("thermal_formats", "thermal_provenance", "uvc_controls", "uvc_permission", "uvc_store", "uvc_camera_list"):
            with self.subTest(key=key), tempfile.TemporaryDirectory() as temp:
                root = Path(temp); inventory, _, _, _ = self.prepare(root)
                plan = self.generate(inventory, root / "plan")
                Path(plan["records"][key]["path"]).write_text("changed")
                with self.assertRaises(ValueError):GEN.BASE.verify(plan)

    def test_real_inert_children_read_change_readback_restore_without_physical_claim(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, _, state, events = self.prepare(root)
            plan = self.generate(inventory, root / "plan")
            self.observe(root, self.confirmation(root, plan))
            result = json.loads((root / "observation/observation.json").read_text())
            self.assertEqual(result["restoration"], "observed")
            self.assertTrue(result["roundtrip_observed"])
            self.assertTrue(result["store_unchanged"])
            self.assertEqual(result["hardware_case_status"], "not_run")
            self.assertEqual(json.loads(state.read_text())["value"], 4)
            commands = [json.loads(line) for line in events.read_text().splitlines()]
            sets = [(role, next(a for a in args if a.startswith('brightness='))) for role,args in commands if 'set-control' in args]
            self.assertEqual(sets[-2:], [('operator','brightness=6'),('operator','brightness=4')])
            self.assertTrue(all('--no-persist' in args for _,args in commands if 'set-control' in args))
            self.assertTrue(all(c["status"] == "not_run" for c in json.loads((root/'plan/evidence.json').read_text())["cases"]))

    def test_ambiguous_write_or_interruption_still_restores_original(self):
        for failure in (ValueError("synthetic lost reply"), KeyboardInterrupt(), SystemExit(143)):
            with self.subTest(failure=type(failure).__name__), tempfile.TemporaryDirectory() as temp:
                root = Path(temp); inventory, _, state, _ = self.prepare(root)
                plan = self.generate(inventory, root / "plan")
                real = GEN.BASE.bounded_process
                def interrupt_after_write(argv, *args, **kwargs):
                    result = real(argv, *args, **kwargs)
                    if 'brightness=6' in argv:raise failure
                    return result
                with patch.object(GEN.BASE, "bounded_process", side_effect=interrupt_after_write):
                    with self.assertRaises(type(failure)):self.observe(root, self.confirmation(root, plan))
                result = json.loads((root / 'observation/observation.json').read_text())
                self.assertEqual(result['restoration'], 'observed')
                self.assertEqual(json.loads(state.read_text())['value'], 4)

    def test_stale_wrong_identity_and_live_state_refuse_before_setting(self):
        for mismatch in ('stale','uuid','control'):
            with self.subTest(mismatch=mismatch), tempfile.TemporaryDirectory() as temp:
                root = Path(temp); inventory, _, state, events = self.prepare(root)
                plan = self.generate(inventory, root / 'plan');confirmation=self.confirmation(root,plan)
                data=json.loads(confirmation.read_text())
                if mismatch=='stale':data['captured_at']=(datetime.now(timezone.utc)-timedelta(hours=1)).isoformat()
                if mismatch=='uuid':data['hardware_uuid']='another Mac'
                confirmation.write_text(json.dumps(data))
                if mismatch=='control':row=json.loads(state.read_text());row['value']=2;state.write_text(json.dumps(row))
                with self.assertRaises(ValueError):self.observe(root,confirmation)
                if events.exists():self.assertNotIn('set-control', events.read_text())

    def test_real_sigterm_after_inert_write_restores_and_reaps(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, raw, state, _ = self.prepare(root)
            marker = root / "write-started"
            cli = Path(raw["cli_binary"])
            text = cli.read_text().replace("import json,os,sys", "import json,os,sys,time")
            text = text.replace("row['value']=value;state.write_text(json.dumps(row));print('synthetic applied')",
                "row['value']=value;state.write_text(json.dumps(row));print('synthetic applied')\n if value==6:Path("+repr(str(marker))+").write_text('inert write reached');time.sleep(2)")
            cli.write_text(text)
            plan = self.generate(inventory, root/'plan');confirmation=self.confirmation(root,plan)
            code = f"""import importlib.util
from pathlib import Path
spec=importlib.util.spec_from_file_location('hardware', {str(GEN.__file__)!r})
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
m.BASE.platform.system=lambda:'Darwin'
m.BASE.platform.node=lambda:'fixture-mac'
m.observe_controls(Path({str(root/'plan')!r}),Path({str(confirmation)!r}),Path({str(root/'observation')!r}))
"""
            with (root/'helper.log').open('wb') as log:
                process=subprocess.Popen([sys.executable,'-c',code],stdout=log,stderr=log)
                try:
                    until=time.monotonic()+6
                    while not marker.exists() and time.monotonic()<until and process.poll() is None:time.sleep(.02)
                    self.assertTrue(marker.exists(),(root/'helper.log').read_text())
                    process.send_signal(signal.SIGTERM)
                    self.assertNotEqual(process.wait(timeout=10),0)
                finally:
                    if process.poll() is None:process.kill();process.wait(timeout=3)
            result=json.loads((root/'observation/observation.json').read_text())
            self.assertEqual(result['restoration'],'observed')
            self.assertEqual(json.loads(state.read_text())['value'],4)
            self.assertIn('SystemExit',result['error'])

    def test_failed_restore_is_reported_and_never_hidden(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, _, state, _ = self.prepare(root)
            plan = self.generate(inventory, root / 'plan');real=GEN.BASE.bounded_process
            def failed_restore(argv,*args,**kwargs):
                if 'brightness=4' in argv and Path(args[0]['WENDY_CONFIG_DIR']).name=='operator':
                    return {'returncode':1,'timed_out':False,'forced_termination':False}
                return real(argv,*args,**kwargs)
            with patch.object(GEN.BASE,'bounded_process',side_effect=failed_restore):
                with self.assertRaisesRegex(ValueError,'restoration'):self.observe(root,self.confirmation(root,plan))
            result=json.loads((root/'observation/observation.json').read_text())
            self.assertEqual(result['restoration'],'failed')
            self.assertIn('restore_error',result)
            self.assertEqual(json.loads(state.read_text())['value'],6)

    def test_protobuf_zero_values_are_omitted_but_not_invented(self):
        row=GEN.control_record([{'name':'brightness','maximum':4,'step':1,'mutable':True}], 'brightness')
        self.assertEqual(row['value'],0)
        for value in (True,'0'):
            with self.assertRaises(ValueError):GEN.control_record([dict(row,value=value)],'brightness')

if __name__=='__main__':unittest.main()
