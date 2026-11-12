"""Host activity policy and expiring permissions. Never starts host workloads."""
import json
import os
from pathlib import Path
import time

from worker import atomic_json


class Policy:
    def __init__(self, idle_seconds=60, max_foreign_cores=0.25, reserve_gib=4,
                 start_gib=12, model_start_gib=140, gpu_mib=180000, gpu_index=0):
        self.idle_seconds = idle_seconds
        self.max_foreign_cores = max_foreign_cores
        self.reserve = reserve_gib * 1024**3
        self.start_memory = start_gib * 1024**3
        self.model_start_memory = model_start_gib * 1024**3
        self.gpu_mib, self.gpu_index = gpu_mib, gpu_index
        self.previous = None
        self.cpu_since = self.gpu_since = None
        self.cpu = self.gpu = False

    def update(self, sample):
        previous, self.previous = self.previous, sample
        reason = "collecting_baseline"
        try:
            if "error" in sample:
                raise ValueError(sample["error"])
            if previous is None or "error" in previous or previous["boot_id"] != sample["boot_id"]:
                raise ValueError("collecting_baseline")
            if not 0 <= time.monotonic() - sample["time"] <= 8:
                raise ValueError("stale_sample")
            elapsed = sample["time"] - previous["time"]
            total = sample["cpu_total"] - previous["cpu_total"]
            own = sample["own_cpu_usec"] - previous["own_cpu_usec"]
            if not 0 < elapsed <= 8 or total <= 0 or own < 0 or previous["groups"] != sample["groups"]:
                raise ValueError("sampling_gap_or_cgroup_change")
            busy = 1 - (sample["cpu_idle"] - previous["cpu_idle"]) / total
            foreign_cores = max(0, busy * sample["cpu_count"] - own / elapsed / 1e6)
            gpu = next((g for g in sample["gpus"] if g["index"] == self.gpu_index), None)
            # Foreign GPU work yields the entire app, including CPU tests.
            foreign_gpu = any(g["foreign"] for g in sample["gpus"])
            memory_floor = self.reserve if self.cpu else self.start_memory
            cpu_safe = foreign_cores <= self.max_foreign_cores and not foreign_gpu and sample["memory_available"] >= memory_floor
            gpu_safe = (cpu_safe and sample["gpu_known"] and gpu is not None and gpu["supported"]
                        and sample["memory_available"] >= (self.reserve if self.gpu else self.model_start_memory)
                        and (self.gpu or (gpu["free_mib"] is not None and gpu["utilization"] is not None
                            and gpu["free_mib"] >= self.gpu_mib and gpu["utilization"] <= 5)))
            reason = "idle" if cpu_safe else "host_busy_or_memory_pressure"
            if not sample["gpu_known"] and sample.get("gpu_present", False):
                # Lost GPU visibility must also stop ongoing CPU work until the
                # next sample, so a failing GPU probe cannot mask competition.
                cpu_safe = False
                reason = "gpu_visibility_lost"
            now = sample["time"]
            self.cpu_since = (self.cpu_since if self.cpu_since is not None else now) if cpu_safe else None
            self.gpu_since = (self.gpu_since if self.gpu_since is not None else now) if gpu_safe else None
            self.cpu = self.cpu_since is not None and now - self.cpu_since >= self.idle_seconds
            self.gpu = self.gpu_since is not None and now - self.gpu_since >= self.idle_seconds
        except (KeyError, TypeError, ValueError, ZeroDivisionError) as error:
            self.cpu = self.gpu = False
            self.cpu_since = self.gpu_since = None
            reason = str(error)
        return {"cpu": self.cpu, "gpu": self.gpu, "reason": reason,
                "boot_id": sample.get("boot_id"), "issued": time.monotonic()}


def samples(socket, app_id):
    import grpc
    import host_shell_pb2 as pb
    source = Path(__file__).with_name("host_probe.py").read_text()
    with grpc.insecure_channel("unix:" + socket) as channel:
        call = channel.stream_stream("/wendy.agent.services.v1.WendyShellService/HostShell",
                                     request_serializer=pb.HostShellRequest.SerializeToString,
                                     response_deserializer=pb.HostShellResponse.FromString)
        request = pb.HostShellRequest(start=pb.HostShellRequest.Start(command=["python3", "-u", "-c", source, app_id]))
        buffer = b""
        for response in call(iter([request])):
            if response.WhichOneof("response_type") == "exit_code":
                raise RuntimeError(f"host probe exited: {response.exit_code}")
            buffer += response.stdout_data
            if len(buffer) > 65536:
                raise ValueError("host probe response too large")
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                if line.startswith(b"SPARE_SAMPLE "):
                    yield json.loads(line[len(b"SPARE_SAMPLE "):])
        raise RuntimeError("host probe disconnected")


def main():
    control = Path("/control")
    control.mkdir(exist_ok=True)
    # Persist mounts are shared. Only root in this supervisor may publish grants.
    control.chmod(0o755)
    policy = Policy(idle_seconds=float(os.environ.get("IDLE_SECONDS", 60)),
                    max_foreign_cores=float(os.environ.get("MAX_FOREIGN_CORES", 0.25)),
                    reserve_gib=float(os.environ.get("RESERVE_GIB", 4)),
                    start_gib=float(os.environ.get("START_FREE_GIB", 12)),
                    model_start_gib=float(os.environ.get("MODEL_START_FREE_GIB", 140)),
                    gpu_mib=float(os.environ.get("MODEL_START_FREE_MIB", 180000)),
                    gpu_index=int(os.environ.get("GPU_INDEX", 0)))
    path = control / "lease.json"
    last = None
    while True:
        atomic_json(path, {"cpu": False, "gpu": False, "issued": 0})
        try:
            for sample in samples(os.environ["WENDY_AGENT_SOCKET"], os.environ["WENDY_APP_ID"]):
                grant = policy.update(sample)
                atomic_json(path, grant)
                status = (grant["cpu"], grant["gpu"], grant["reason"])
                if status != last:
                    print(json.dumps({"event": "admission", **grant}), flush=True)
                    last = status
        except Exception as error:
            atomic_json(path, {"cpu": False, "gpu": False, "issued": 0})
            policy.previous = None
            policy.cpu_since = policy.gpu_since = None
            policy.cpu = policy.gpu = False
            print(json.dumps({"event": "probe_unavailable", "error": str(error)}), flush=True)
            time.sleep(5)


if __name__ == "__main__":
    main()
