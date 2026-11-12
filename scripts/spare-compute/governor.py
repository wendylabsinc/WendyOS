"""Host activity policy and expiring permissions. Never starts host workloads."""
import json
import os
from pathlib import Path
import time

from worker import atomic_json


class Policy:
    def __init__(self, idle_seconds=60, max_foreign_cores=0.25, reserve_gib=4,
                 start_gib=12, model_start_gib=140, gpu_mib=180000, gpu_index=0, unified_model_gib=0):
        self.idle_seconds = idle_seconds
        self.max_foreign_cores = max_foreign_cores
        self.reserve = reserve_gib * 1024**3
        self.start_memory = start_gib * 1024**3
        self.model_start_memory = model_start_gib * 1024**3
        self.unified_model_memory = unified_model_gib * 1024**3
        self.gpu_mib, self.gpu_index = gpu_mib, gpu_index
        self.previous = None
        self.cpu_since = self.gpu_since = None
        self.cpu = self.gpu = False

    def update(self, sample):
        previous, self.previous = self.previous, sample
        reason = "collecting_baseline"
        foreign_cores = None
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
            idle = sample["cpu_idle"] - previous["cpu_idle"]
            if not 0 <= idle <= total:
                raise ValueError("invalid_cpu_delta")
            busy = 1 - idle / total
            foreign_cores = max(0, busy * sample["cpu_count"] - own / elapsed / 1e6)
            gpu = next((g for g in sample["gpus"] if g["index"] == self.gpu_index), None)
            # CPU admission uses CPU/RAM independently of resident GPU allocations.
            foreign_gpu = any(g["foreign"] for g in sample["gpus"])
            memory_floor = self.reserve if self.cpu else self.start_memory
            cpu_safe = foreign_cores <= self.max_foreign_cores and sample["memory_available"] >= memory_floor
            unified = gpu is not None and gpu.get("unified", False)
            loading_floor = (self.unified_model_memory + self.reserve) if unified else self.model_start_memory
            gpu_memory_safe = (self.unified_model_memory > 0 if unified else
                gpu is not None and gpu["free_mib"] is not None and gpu["free_mib"] >= self.gpu_mib)
            gpu_safe = (cpu_safe and not foreign_gpu and sample["gpu_known"] and gpu is not None and gpu["supported"]
                        and sample["memory_available"] >= (self.reserve if self.gpu else loading_floor)
                        and (not unified or self.unified_model_memory > 0)
                        and gpu["utilization"] is not None
                        and (unified or gpu["free_mib"] is not None)
                        and (self.gpu or (gpu_memory_safe and gpu["utilization"] is not None and gpu["utilization"] <= 5)))
            reason = "idle" if cpu_safe else "host_busy_or_memory_pressure"
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
                "boot_id": sample.get("boot_id"), "issued": time.monotonic(),
                "foreign_cpu_cores": foreign_cores, "memory_available": sample.get("memory_available"),
                "gpu_observation_valid": sample.get("gpu_known", False)}


def observation(response, app_id, started):
    host = response.host
    # Match this manifest's exact service IDs; another app can share a prefix.
    service_ids = {app_id + "_" + name for name in ("supervisor", "worker", "reviewer", "model")}
    owned = [c for c in response.containers if c.app_name == app_id or c.app_name in service_ids]
    if (not owned or host.cpu_count <= 0 or host.cpu_total_jiffies <= 0
            or host.mem_total_bytes <= 0 or not 0 < host.mem_available_bytes <= host.mem_total_bytes
            or host.cpu_idle_jiffies > host.cpu_total_jiffies):
        raise ValueError("incomplete_host_or_app_stats")
    return {"time": started, "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "cpu_count": host.cpu_count, "cpu_total": host.cpu_total_jiffies,
            "cpu_idle": host.cpu_idle_jiffies,
            "own_cpu_usec": sum(c.cpu_usage_nanos for c in owned) / 1000,
            "groups": sorted(c.app_name for c in owned),
            "memory_available": host.mem_available_bytes,
            # Older Agents do not report GPU process ownership. CPU admission
            # remains independent; GPU permission stays denied until supported.
            "gpu_known": host.HasField("gpu_admission") and host.gpu_admission.valid,
            "gpu_present": bool(host.gpus),
            "gpus": [{"index": g.index, "uuid": g.uuid, "foreign": g.foreign_process,
                      "supported": g.supported, "unified": g.unified_memory,
                      "utilization": g.utilization_percent if g.HasField("utilization_percent") else None,
                      "free_mib": g.memory_free_bytes / 1024**2 if g.HasField("memory_free_bytes") else None}
                     for g in host.gpu_admission.devices]}


def samples(socket, app_id):
    import grpc
    import resource_stats_pb2 as pb
    with grpc.insecure_channel("unix:" + socket,
            options=[("grpc.max_receive_message_length", 1024 * 1024)]) as channel:
        call = channel.unary_unary("/wendy.agent.services.v1.WendyContainerService/GetResourceStats",
                                  request_serializer=pb.GetResourceStatsRequest.SerializeToString,
                                  response_deserializer=pb.GetResourceStatsResponse.FromString)
        while True:
            started = time.monotonic()
            response = call(pb.GetResourceStatsRequest(gpu_admission_app_id=app_id), timeout=6)
            yield observation(response, app_id, started)
            time.sleep(max(0, 2 - (time.monotonic() - started)))


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
                    gpu_index=int(os.environ.get("GPU_INDEX", 0)),
                    unified_model_gib=float(os.environ.get("UNIFIED_MODEL_GIB", 0)))
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
