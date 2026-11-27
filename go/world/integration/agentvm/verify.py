#!/usr/bin/env python3
"""Run the private agent and ordinary publication apps in an owned Docker VM container.

All credentials are disposable test credentials generated inside the container.
The output directory can contain private runtime logs and is not a Git artifact.
"""
import argparse
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import time
import uuid

HERE = Path(__file__).resolve().parent
APP = "com.wendy.world.vmfixture"
SERVICES = ("gas", "temperature")
CAPS = ("CHOWN", "DAC_OVERRIDE", "FSETID", "FOWNER", "MKNOD", "NET_RAW", "SETGID", "SETUID", "SETFCAP", "SETPCAP", "NET_BIND_SERVICE", "KILL", "AUDIT_WRITE", "SYS_ADMIN", "NET_ADMIN", "SYS_CHROOT")
ENV = {"WENDY_AGENT_SOCKET": "/var/lib/wendy/agent-control/agent.sock", "WENDY_CONFIG_DIR": "/var/lib/agentvm/cli", "XDG_CACHE_HOME": "/var/lib/agentvm/cache", "WENDY_ANALYTICS": "false", "WENDY_RUN_NO_CLOUD_FALLBACK": "1", "WENDY_BUILDKIT_HOST": "unix:///run/buildkit/buildkitd.sock"}


class Runner:
    def __init__(self, args):
        self.args = args
        self.out = args.output.resolve()
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.name = "world-agent-vm-" + uuid.uuid4().hex[:12]
        self.network = self.name + "-net"
        self.volume = self.name + "-state"
        self.count = 0
        self.created = []
        self.agent_pid = 0

    def call(self, argv, timeout=60, check=True, input_data=None):
        self.count += 1
        try:
            result = subprocess.run(argv, capture_output=True, timeout=timeout, input=input_data)
        except subprocess.TimeoutExpired as error:
            result = subprocess.CompletedProcess(argv,124,error.stdout or b"",(error.stderr or b"")+b"\nBounded runner command timed out.\n")
        stem = self.out / f"{self.count:03d}"
        stem.with_suffix(".stdout").write_bytes(result.stdout)
        stem.with_suffix(".stderr").write_bytes(result.stderr)
        stem.with_suffix(".json").write_text(json.dumps({"argv": [str(x) for x in argv], "exit_code": result.returncode}) + "\n")
        if check and result.returncode:
            raise RuntimeError(f"step {self.count} failed with exit {result.returncode}; inspect private output {stem}")
        return result

    def inside(self, *argv, timeout=60, check=True):
        return self.call(["docker", "exec", self.name, *argv], timeout, check)

    def upload(self, destination, files):
        # Docker's host-side cp cannot see this runtime's tmpfs mount contents
        # on OrbStack. Transfer only the selected files through the owned
        # container process instead; no host directory/socket is mounted.
        data = io.BytesIO()
        with tarfile.open(fileobj=data,mode="w") as archive:
            for name,path in files.items():
                if Path(name).name != name: raise ValueError("flat upload names required")
                body = path.read_bytes()
                member = tarfile.TarInfo(name); member.size = len(body); member.mode = 0o755 if destination == "/var/lib/agentvm/bin" else 0o644
                archive.addfile(member,io.BytesIO(body))
        self.call(["docker","exec","-i",self.name,"tar","--no-same-owner","-xf","-","-C",destination],input_data=data.getvalue())

    def python(self, code, *args, check=True):
        return self.inside("python3", "-c", code, *args, check=check)

    def wait(self, code, seconds=60):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if self.python(code, check=False).returncode == 0:
                return
            running = self.call(["docker","inspect","--format","{{.State.Running}}",self.name],check=False)
            if running.returncode or running.stdout.strip() != b"true":
                raise RuntimeError("owned runtime exited before readiness")
            time.sleep(1)
        raise RuntimeError("bounded runtime readiness wait expired")

    def cli(self, *args, timeout=300, check=True):
        env = [item for key, value in ENV.items() for item in ("-e", key + "=" + value)]
        return self.call(["docker", "exec", *env, self.name, "/var/lib/agentvm/bin/wendy", "--json", *args], timeout, check)

    def agent(self, phase):
        # This fixed shell fragment has no interpolated credentials or user text.
        self.inside("mkdir", "-p", "/var/lib/agentvm/logs")
        self.call(["docker", "exec", "-d", "-e", "WENDY_CONFIG_PATH=/etc/wendy-agent", "-e", "WENDY_CONTAINERD_ADDR=/run/containerd/containerd.sock", self.name, "sh", "-c", 'echo $$ > /run/agentvm/agent.pid; exec /var/lib/agentvm/bin/wendy-agent > "/var/lib/agentvm/logs/agent-$1.log" 2>&1', "sh", phase])
        live = "import os; p=int(open('/run/agentvm/agent.pid').read()); assert p>1 and p!="+str(self.agent_pid)+"; assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentvm/bin/wendy-agent'; print(p)"
        self.wait(live)
        pid = int(self.python(live).stdout)
        deadline = time.monotonic()+60
        while time.monotonic() < deadline:
            if self.cli("device","apps","list",timeout=8,check=False).returncode == 0:
                self.python("import os,sys; p=int(open('/run/agentvm/agent.pid').read()); assert p==int(sys.argv[1]); assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentvm/bin/wendy-agent'",str(pid))
                self.agent_pid = pid
                return
            time.sleep(1)
        raise RuntimeError("new agent did not serve a bounded admin RPC")

    def stop_agent(self):
        self.python("import os,signal; p=int(open('/run/agentvm/agent.pid').read()); assert p>1; os.kill(p,signal.SIGKILL)")
        self.wait("import os; p=int(open('/run/agentvm/agent.pid').read()); assert not os.path.exists('/proc/'+str(p))")

    def deploy(self, phase):
        self.cli("run", "--prefix", "/var/lib/agentvm/project", "--builder", "buildkit", "--chunking", "force", "--detach", "--skip-cloud-registration", "--yes")
        self.cli("device", "apps", "list")
        self.wait("import glob,json; fs=glob.glob('/var/lib/wendy/volumes/world-vm-*-proofs/run-*/summary.json'); assert len(fs)==" + str(phase*2), 60)
        state = {}
        tasks = {}
        for row in self.inside("ctr","tasks","list").stdout.decode().splitlines()[1:]:
            fields = row.split()
            if len(fields) >= 3: tasks[fields[0]] = (int(fields[1]),fields[2])
        ids = self.inside("ctr", "containers", "list", "-q").stdout.decode().splitlines()
        for cid in ids:
            item = json.loads(self.inside("ctr", "containers", "info", cid).stdout)
            labels = item.get("Labels", {})
            if labels.get("sh.wendy/app.id") != APP:
                continue
            service = labels["sh.wendy/service"]
            if service not in SERVICES or service in state:
                raise RuntimeError("unexpected or duplicate fixture service")
            state[service] = {"container": cid, "generation": int(labels["sh.wendy/world.deployment-generation"]), "socket_directory": labels["sh.wendy/world.socket-directory"]}
            if cid not in tasks or tasks[cid][0] <= 1 or tasks[cid][1] != "RUNNING":
                raise RuntimeError("fixture service is not an actual running task")
            state[service]["task_pid"] = tasks[cid][0]
            state[service]["cgroup"] = self.python("import sys; print(open('/proc/'+str(int(sys.argv[1]))+'/cgroup').read().strip())",str(tasks[cid][0])).stdout.decode().strip()
        if set(state) != set(SERVICES) or len({x["socket_directory"] for x in state.values()}) != 2:
            raise RuntimeError("missing separate service containers or socket mounts")
        for service in SERVICES:
            r = self.python("import glob,json,os,sys; files=glob.glob('/var/lib/wendy/volumes/world-vm-'+sys.argv[1]+'-proofs/run-*/summary.json'); p=max(files,key=os.path.getmtime); print(json.dumps({'path':p,'summary':json.load(open(p))}))",service)
            entry = json.loads(r.stdout)
            summary = entry["summary"]
            if summary.get("service") != service or sorted(x.get("mode","") for x in summary.get("proofs",[])) != ["after-refusals","stream","unary"] or sorted(summary.get("refusals",[])) != ["physical_provenance","sibling_stream","uncommissioned_frame"]:
                raise RuntimeError("fixture summary lacks the exact publication and refusal cases")
            parent = str(Path(entry["path"]).parent)
            state[service]["proof_directory"] = parent
            for proof in entry["summary"]["proofs"]:
                mode = proof["mode"]
                self.inside("/var/lib/agentvm/bin/fixture", "verify", "--directory", "/etc/wendy-agent", "--proof", parent + "/" + mode + ".json", "--request", parent + "/" + mode + ".request.json", "--service", service, "--generation", str(state[service]["generation"]), "--epoch", str(phase))
        (self.out / f"phase-{phase}.json").write_text(json.dumps(state, indent=2)+"\n")
        return state

    def run(self):
        binaries = {"wendy-agent": self.args.agent, "wendy": self.args.cli, "fixture": self.args.fixture, "world-service-init": self.args.initializer, "world-agent-vm-publisher": self.args.publisher}
        hashes = {}
        for name, path in binaries.items():
            if not path.is_file():
                raise ValueError("missing binary " + str(path))
            hashes[name] = hashlib.sha256(path.read_bytes()).hexdigest()
        (self.out / "inputs.json").write_text(json.dumps({"image": self.args.image, "binaries": hashes, "name": self.name, "network": self.network, "volume": self.volume},indent=2)+"\n")
        self.created.append("network"); self.call(["docker", "network", "create", "--label", "wendy.world.agentvm="+self.name, self.network])
        self.created.append("volume"); self.call(["docker", "volume", "create", "--label", "wendy.world.agentvm="+self.name, self.volume])
        argv = ["docker", "run", "--detach", "--name", self.name, "--label", "wendy.world.agentvm="+self.name, "--cgroupns=private", "--network", self.network, "--read-only", "--cap-drop", "ALL"]
        for cap in CAPS: argv += ["--cap-add",cap]
        argv += ["--security-opt", "seccomp=unconfined", "--pids-limit", "2048", "--memory", "6g", "--cpus", "4", "--ulimit", "nofile=65536:65536"]
        for target,size,mode in (("/run","512m","0755"),("/tmp","512m","1777"),("/etc/wendy-agent","16m","0700"),("/config","16m","0700")):
            argv += ["--tmpfs",target+":rw,nosuid,nodev,size="+size+",mode="+mode]
        argv += ["--mount","type=volume,src="+self.volume+",dst=/var/lib", self.args.image]
        self.created.append("container"); self.call(argv)
        self.call(["docker", "inspect", self.name])
        self.wait("import json; assert json.load(open('/run/agentvm/ready.json'))['ready'] is True")
        self.inside("mkdir", "-p", "-m", "0755", "/var/lib/agentvm/bin", "/var/lib/agentvm/project")
        self.upload("/var/lib/agentvm/bin",binaries)
        self.upload("/var/lib/agentvm/project",{name:HERE/"project"/name for name in ("Dockerfile","wendy.json")})
        self.inside("cp", "/var/lib/agentvm/bin/world-agent-vm-publisher", "/var/lib/agentvm/project/world-agent-vm-publisher")
        self.inside("/var/lib/agentvm/bin/fixture","init","--directory","/etc/wendy-agent","--state-directory","/var/lib/wendy/world")
        self.python("open('/etc/wendy-agent/network-access','w').write('local\\n')")
        self.inside("/var/lib/agentvm/bin/world-service-init","--config","/etc/wendy-agent/world.json")
        self.call(["docker","exec","-d",self.name,"sh","-c","exec /var/lib/agentvm/bin/fixture serve --directory /etc/wendy-agent --handoff /config/acme-enrollment.json --duration 10m > /var/lib/agentvm/logs/issuer.log 2>&1"])
        self.wait("import os; assert os.path.isfile('/config/acme-enrollment.json')")
        self.agent("initial")
        self.wait("import json,os; assert json.load(open('/etc/wendy-agent/provisioning.json')).get('enrolled'); assert os.path.isfile('/etc/wendy-agent/.vm-fixture/issued.json')",90)
        initial = self.deploy(1)
        self.stop_agent()
        # The fixture checker reads bbolt without admitting or reconstructing state.
        for service, item in initial.items():
            self.inside("/var/lib/agentvm/bin/fixture","journal","--directory","/etc/wendy-agent","--proof",item["proof_directory"]+"/after-refusals.json","--service",service,"--generation",str(item["generation"]),"--epoch","1")
        self.agent("restart")
        for item in initial.values():
            parent = item["proof_directory"]
            self.python("import sys; open(sys.argv[1]+'/probe-request','x').close()",parent)
            self.wait("import json; p="+repr(parent+"/probe-result.json")+"; assert json.load(open(p))['accepted'] is False",10)
        subsequent = self.deploy(2)
        if any(subsequent[s]["generation"] == initial[s]["generation"] for s in SERVICES):
            raise RuntimeError("deployment generation reused after redeployment")
        self.stop_agent()
        for service, item in subsequent.items():
            self.inside("/var/lib/agentvm/bin/fixture","journal","--directory","/etc/wendy-agent","--proof",item["proof_directory"]+"/after-refusals.json","--service",service,"--generation",str(item["generation"]),"--epoch","2")
        self.call(["docker","cp",self.name+":/var/lib/wendy/volumes",str(self.out/"proof-volumes")])
        (self.out/"issued.json").write_bytes(self.inside("cat","/etc/wendy-agent/.vm-fixture/issued.json").stdout)
        (self.out/"result.json").write_text(json.dumps({"passed":True,"simulation_only":True,"normal_first_boot_acme":True,"normal_cli_containerd_deployment":True,"initial":initial,"redeployed":subsequent},indent=2)+"\n")

    def finish(self):
        def owned(kind, name):
            command = ["docker", "inspect", name] if kind == "container" else ["docker",kind,"inspect",name]
            result = self.call(command,check=False)
            if result.returncode: return False
            try:
                obj = json.loads(result.stdout)[0]
                labels = obj.get("Config",{}).get("Labels",{}) if kind == "container" else obj.get("Labels",{})
                return (labels or {}).get("wendy.world.agentvm") == self.name
            except (ValueError,KeyError,IndexError,TypeError): return False
        if "container" in self.created and owned("container",self.name):
            self.call(["docker","logs",self.name],check=False)
            self.call(["docker","cp",self.name+":/var/lib/agentvm/logs",str(self.out/"runtime-logs")],check=False)
            if not self.args.keep: self.call(["docker","rm","-f",self.name],check=False)
        if not self.args.keep:
            if "volume" in self.created and owned("volume",self.volume): self.call(["docker","volume","rm",self.volume],check=False)
            if "network" in self.created and owned("network",self.network): self.call(["docker","network","rm",self.network],check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image",required=True)
    for name in ("agent","cli","fixture","initializer","publisher","output"):
        parser.add_argument("--"+name,type=Path,required=True)
    parser.add_argument("--keep",action="store_true",help="retain only this owned container/network/volume for debugging")
    args = parser.parse_args()
    runner = Runner(args)
    try: runner.run()
    finally: runner.finish()


if __name__ == "__main__": main()
