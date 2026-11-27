"""Exercise signed durable missions against independently running MuJoCo physics.

Temporary isolated Linux containers on the existing Docker VM; no hardware.
Operator commands are short-lived containers. The robot gateway and executor
retain a local connection while remote world peers are partitioned.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

from virtual_site import command, ROOT


def run(node_image, actuator_image):
    suffix = uuid.uuid4().hex[:10]
    peer, local = f"mission-peer-{suffix}", f"mission-local-{suffix}"
    nodes = [f"mission-node-{i}-{suffix}" for i in range(1, 4)]
    actuator, executor = f"mission-actuator-{suffix}", f"mission-executor-{suffix}"
    created, networks = [], []
    user = f"{os.getuid()}:{os.getgid()}"
    report = {"mode": "isolated Linux containers on existing Docker VM", "physical_hardware": False}
    with tempfile.TemporaryDirectory(prefix="wendy-mission-") as tmp:
        folder = Path(tmp)
        try:
            command(["go", "run", "./cmd/world-lab", "--out", str(folder), "--runtime-dir", "/state", "--containers", "--missions", "--run-id", "mission-test"])
            for network in (peer, local):
                command(["docker", "network", "create", "--internal", network]); networks.append(network)
            for i, name in enumerate(nodes, 1):
                state = folder / f"state-{i}"; state.mkdir(mode=0o700)
                args = ["docker", "run", "-d", "--name", name, "--network", local if i == 1 else peer,
                        "--network-alias", f"world-node-{i}", "--user", user,
                        "--mount", f"type=bind,src={folder / f'node-{i}.json'},dst=/config.json,readonly",
                        "--mount", f"type=bind,src={state},dst=/state"]
                command(args + [node_image, "--config", "/config.json"]); created.append(name)
            command(["docker", "network", "connect", "--alias", "world-node-1", peer, nodes[0]])
            command(["docker", "run", "-d", "--name", actuator, "--network", local, "--network-alias", "actuator", "--user", user,
                     "--mount", f"type=bind,src={folder / 'adapter.token'},dst=/token,readonly",
                     actuator_image, "examples/mission-office/world.json", "--token-file", "/token", "--host", "0.0.0.0",
                     "--allow-network-simulation", "--allow-test-faults"]); created.append(actuator)
            def physics(path="state", data=None):
                script = """import json,urllib.request,sys
request=urllib.request.Request('http://127.0.0.1:8091/v1/'+sys.argv[1],data=None if sys.argv[2]=='null' else sys.argv[2].encode(),headers={'Authorization':'Bearer '+open('/token').read(),'Content-Type':'application/json'})
with urllib.request.urlopen(request,timeout=2) as response: print(response.read().decode())
"""
                return json.loads(command(["docker","exec",actuator,"python","-c",script,path,json.dumps(data)]))

            def cli(verb, *args):
                mounts=["--mount",f"type=bind,src={folder / 'operator-1.json'},dst=/operator.json,readonly"]
                if verb=="send":
                    mounts += ["--mount",f"type=bind,src={args[1]},dst=/spec.json,readonly"]
                    args=("--spec","/spec.json")
                return json.loads(command(["docker","run","--rm","--network",local,"--user",user,"--entrypoint","/world",
                    *mounts,node_image,"task",verb,"--credentials","/operator.json","--json",*args]))

            def until(fn, predicate, timeout=10):
                end = time.monotonic() + timeout; last = None
                while time.monotonic() < end:
                    try:
                        last = fn()
                        if predicate(last): return last
                    except (RuntimeError, OSError) as error: last = str(error)
                    time.sleep(.04)
                raise AssertionError(f"condition not met: {last}")

            def tasks(): return cli("list")["tasks"]
            def status(task_id): return next(t["status"]["Phase"] for t in tasks() if t["spec"]["id"] == task_id)
            def stopped(s): return abs(s["velocity_mps"][0]) < .001 and abs(s["commanded_velocity_mps"]) < .001
            def moving(): return until(physics, lambda s: abs(s["velocity_mps"][0]) > .1)
            def hold_position(seconds=.45):
                a = until(physics, stopped); time.sleep(seconds); b = physics()
                assert stopped(b) and abs(a["position_m"][0]-b["position_m"][0]) < .005, (a,b)
                return b
            def submit(task_id, target):
                spec = {"id":task_id,"type":"wendy.nav.go_to.v1","site":"synthetic-office","assignee":"sim-robot/navigation",
                        "domain":"simulation","class":"MOVE","mode":"autonomous","frame":{"id":"office-floor","revision":"office-frame-v1"},
                        "not_after_unix_ms":int(time.time()*1000)+120000,"area":{"min":[-2.1,.1,-.1],"max":[2.1,.3,.1]},
                        "required_sensors":["gas","local-obstacles"],"minimum_energy":.2,"fallback":"hold",
                        "target":[target,.2,0],"speed_mps":.4,"tolerance_m":.03,"stop_speed_mps":.03}
                path = folder / (task_id+".json"); path.write_text(json.dumps(spec))
                return cli("send", "--spec", str(path))

            until(tasks, lambda value: not value)
            until(physics, lambda s: s["synthetic"])
            command(["docker", "run", "-d", "--name", executor, "--network", local, "--user", user, "--entrypoint", "/world-executor",
                     "--mount", f"type=bind,src={folder / 'executor-1.json'},dst=/client.json,readonly",
                     "--mount", f"type=bind,src={folder / 'adapter.token'},dst=/token,readonly",
                     node_image, "--credentials", "/client.json", "--adapter", "http://actuator:8091", "--adapter-token-file", "/token"])
            created.append(executor)
            # A fresh session plus the first idle stop proves executor startup,
            # including durable epoch allocation, completed before task delivery.
            until(physics, lambda s: s["sequence"] > 0)
            initial = physics()["position_m"][0]
            submit("partition", -.7); moving()
            command(["docker", "network", "disconnect", peer, nodes[0]])
            started = time.monotonic()
            until(lambda:status("partition"),lambda s:s=="succeeded",timeout=15)
            finish = hold_position()
            assert abs(finish["position_m"][0]+.7) <= .031
            report["autonomous_partition"]={"start_x":initial,"finish_x":finish["position_m"][0],"seconds_after_partition":round(time.monotonic()-started,3),"status":"succeeded"}
            command(["docker", "network", "connect", "--alias", "world-node-1", peer, nodes[0]])

            submit("sensor-loss", 1.5); moving(); physics("fault",{"sensor":"gas","available":False})
            until(lambda:status("sensor-loss"),lambda s:s=="held"); hold_position()
            physics("fault",{"sensor":"gas","available":True}); hold_position()
            assert status("sensor-loss")=="held"
            cli("stop","--id","sensor-loss"); assert status("sensor-loss")=="canceled"
            report["sensor_loss"]="held; restoring gas did not resume; operator stop canceled"

            cli("stop","--id","delayed")
            try: submit("delayed",1)
            except RuntimeError: pass
            else: raise AssertionError("delayed task accepted after stop")
            assert status("delayed")=="canceled"; hold_position()
            report["stop_before_delivery"]="delayed signed task rejected"

            submit("operator-stop",1.5); moving()
            started=time.monotonic(); cli("stop","--id","operator-stop")
            hold_position(); assert status("operator-stop")=="canceled"
            report["operator_stop"]="moving mission canceled; measured motion stopped"

            submit("gateway-crash",1.5); moving()
            started=time.monotonic(); command(["docker","kill","--signal","KILL",nodes[0]])
            stopped_state=until(physics,stopped,timeout=1.5)
            elapsed=time.monotonic()-started
            assert elapsed < 1, elapsed
            hold_position(); command(["docker","start",nodes[0]])
            until(lambda:status("gateway-crash"),lambda s:s=="failed"); hold_position()
            report["gateway_sigkill"]={"observed_stop_seconds_including_docker_command":round(elapsed,3),"after_restart":"failed; no resume"}

            submit("executor-crash",1.5); moving()
            started=time.monotonic(); command(["docker","kill","--signal","KILL",executor])
            until(physics,stopped,timeout=1.5);elapsed=time.monotonic()-started
            assert elapsed < 1, elapsed
            hold_position(); command(["docker","start",executor])
            until(lambda:status("executor-crash"),lambda s:s=="failed"); hold_position()
            report["executor_sigkill"]={"observed_stop_seconds_including_docker_command":round(elapsed,3),"after_restart":"failed; no resume"}
            report["limits"]=["single synthetic one-axis robot trial, not safety certification", "direct assignee task delivery; no mesh task relay yet", "ideal simulated braking; no physical robot", "no camera/audio fusion or evidence transfer in this test"]
            return report
        except Exception:
            for name in created:
                logs=subprocess.run(["docker","logs","--tail","15",name],capture_output=True)
                print(name,logs.stdout.decode(),logs.stderr.decode(),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created): subprocess.run(["docker","rm","-f",name],capture_output=True)
            for network in reversed(networks): subprocess.run(["docker","network","rm",network],capture_output=True)

if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image",default="wendy-world-node:local")
    parser.add_argument("--actuator-image",default="wendy-world-actuator:local")
    args=parser.parse_args()
    print(json.dumps(run(args.node_image,args.actuator_image),indent=2))
