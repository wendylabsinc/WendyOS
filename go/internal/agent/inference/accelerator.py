"""Engines that score the YOLO ONNX backend: the CPU, and TensorRT on an NVIDIA GPU.

The agent decides which engines to offer, in accelerator_plan on the config
line, and pins the TensorRT Python bindings it offers. This module decides
what actually runs and reports it. Its report, never the agent's plan, is what
inference_status shows.

With accelerator auto the worker scores on the CPU from the start. A TensorRT
engine is found in the cache or built by a child process in the background,
compared with the CPU on the same frames, and only then swapped in. Every way
of not getting there leaves the CPU running and says why.

TensorRT and CUDA cost a process about 500 MB once loaded, and it keeps them.
So the worker loads them only when an engine is about to load: what the cache
key needs from them is read by a short-lived child, and the build runs in
another.

Run as a script with --probe or --build-engine, this module is that child.
"""

import ctypes
import datetime
import fcntl
import hashlib
import json
import os
import random
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from collections import deque

# Bump only on purpose: every cached engine is rebuilt when it changes.
SCHEMA = 1
# FP16 halves the engine and runs on the Orin's tensor cores. The parity check
# below decides whether the scores are close enough to the CPU's FP32.
PRECISION = "fp16"
# TensorRT's default, named so that a change to it changes the cache key.
OPTIMIZATION_LEVEL = 3

# Parity tolerances. Each CPU detection must have a TensorRT detection of the
# same label overlapping it by IoU >= PARITY_IOU, with scores within
# PARITY_SCORE, and the other way round. Detections within PARITY_SCORE of the
# threshold may appear on one side only, since FP16 can move them across it.
# IoU 0.8 rather than tighter because FP16 moves a box edge by a fraction of a
# pixel, which costs a small, distant person's box several points of IoU; an
# engine that is wrong, rather than rounded, misses by far more.
PARITY_IOU = 0.8
PARITY_SCORE = 0.05
# And no class score anywhere in the raw output, detected or not, may differ by
# more than PARITY_RAW_SCORE. This catches an engine that is wrong on an empty
# scene, where there are no detections to compare.
PARITY_RAW_SCORE = 0.1
# Recent camera frames compared, besides one seeded noise frame.
PARITY_FRAMES = 3

# How long to wait before checking free memory again, for a build or a load.
MEMORY_RETRY_SECONDS = 120
# A failed build is not retried for the same cache key for this long, so a
# model TensorRT cannot handle costs one build a day rather than one a start.
TOMBSTONE_SECONDS = 24 * 3600
# Status and throughput go to the agent at least this often.
STATUS_SECONDS = 10

# When an auto campaign needs an engine built, the build starts after a random
# delay of up to this long. An agent update restarts every campaign on every
# device at once, and with no engine cached each would otherwise start a build
# of about 14 minutes and 2.2 GB the moment it came back. The CPU detects
# meanwhile, so the delay costs speed, never detections. A tensorrt campaign
# scores nothing until its engine is ready, so it builds at once.
BUILD_JITTER_SECONDS = 20 * 60
# A build then starts once at least this many CPUs' worth of the device's CPU
# time is idle, sampled over CPU_SAMPLE_SECONDS. This worker's own CPU time
# counts as idle: it is mostly the CPU inference that the engine replaces, so
# counting it would keep the busiest campaigns on the CPU for good. A build is
# expected to keep about one CPU busy, so two leave one for everything else.
#
# This replaced "other processes use at most half of the CPU", which never let
# a build start on a Jetson Orin Nano (6 CPUs) running a camera campaign. The
# campaign's own capture pipeline, a 720p x264 encode, took about 33% of the
# CPU, the agent 6% and an unrelated app 14%: 60 to 62% in all, and the build
# waited for good. That leaves about 2.3 CPUs idle, so that device builds at
# once now, though only just; BUILD_MAX_CPU_WAIT_SECONDS covers the rest.
BUILD_MIN_IDLE_CPUS = 2
CPU_SAMPLE_SECONDS = 5
# How long to wait before checking the CPU again.
CPU_RETRY_SECONDS = 120
# The longest a build waits for idle CPUs. The wait is for passing peaks, such
# as an app's image being pulled; load that lasts longer, such as continuous
# camera capture, does not go away by waiting. So the build then starts anyway,
# at the lowest priority (BUILD_LOWEST_NICE and the idle I/O class), and runs
# on what other processes leave. The CPU detects throughout, as in any build.
BUILD_MAX_CPU_WAIT_SECONDS = 30 * 60
# The niceness of a build, and of one that stopped waiting for idle CPUs.
BUILD_NICE = 10
BUILD_LOWEST_NICE = 19
# How long the --probe child may take to load TensorRT and CUDA.
PROBE_TIMEOUT_SECONDS = 120

# Why an auto campaign is on the CPU while its engine is on its way. Not
# degraded: the engine is coming.
NOT_READY = "the TensorRT engine is not ready yet"

MAX_PLAN_BYTES = 1 << 30

# What the child reports when a build had to wait for memory.
EXIT_WAITING_FOR_MEMORY = 3

# The TensorRT messages that are logged: TensorRT's ILogger.Severity runs 0
# internal error, 1 error, 2 warning, 3 info, 4 verbose. Info and verbose are
# for debugging TensorRT itself, and a build emits thousands of them.
TENSORRT_LOG_SEVERITY = 2

# Engine status fields that describe one state only. They go when the state
# changes, so a ready engine does not keep the start time of its build.
TRANSIENT_ENGINE_FIELDS = ("error", "started_at", "started", "not_before", "waiting_since", "build_by",
                           "low_priority")

# ioprio_set(2), to put a build in the idle I/O class. Its number depends on
# the architecture; these are the two WendyOS runs on.
IOPRIO_SET = {"aarch64": 30, "x86_64": 251}
IOPRIO_WHO_PROCESS = 1
IOPRIO_CLASS_IDLE = 3
IOPRIO_CLASS_SHIFT = 13


class EngineError(Exception):
    """An engine could not be loaded, built or run."""


def utc_now(after=0):
    moment = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=after)
    return moment.strftime("%Y-%m-%dT%H:%M:%SZ")


def available_mib():
    """MemAvailable in MiB, or None off Linux. The Jetson's GPU shares this
    memory, so it is also the GPU's free memory."""
    try:
        with open("/proc/meminfo") as meminfo:
            for line in meminfo:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1]) // 1024
    except OSError:
        return None
    return None


def rss_mib():
    """This process's resident memory in MiB, or None off Linux."""
    try:
        with open("/proc/self/status") as status:
            for line in status:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) / 1024
    except OSError:
        return None
    return None


def cpu_times():
    """(busy, total) clock ticks of all CPUs since boot, this process's own
    busy ticks, and the number of online CPUs, or None off Linux. /proc/stat
    and /proc/self/stat both count in USER_HZ ticks."""
    try:
        with open("/proc/stat") as stat:
            lines = stat.read().splitlines()
        with open("/proc/self/stat") as stat:
            own = stat.read()
    except OSError:
        return None
    fields = lines[0].split() if lines else []
    if not fields or fields[0] != "cpu":
        return None
    # user nice system idle iowait irq softirq steal; guest time is already
    # inside user and nice.
    values = [int(value) for value in fields[1:9]]
    idle = values[3] + values[4]
    # The first line adds up one cpuN line per online CPU.
    cpus = sum(1 for line in lines[1:] if line.startswith("cpu") and line[3:4].isdigit())
    # The process name, in parentheses, may hold spaces: fields count from the
    # last parenthesis. utime and stime are fields 14 and 15.
    after_name = own[own.rindex(")") + 2:].split()
    return sum(values) - idle, sum(values), int(after_name[11]) + int(after_name[12]), cpus


def short(text, limit=300):
    text = str(text)
    return text if len(text) <= limit else text[:limit - 3] + "..."


def claim_stdout():
    """Gives the protocol its own descriptor and points fd 1 at stderr.
    Returns the protocol's file.

    stdout is the agent's JSON channel, and the agent ends a worker on any
    line that is not JSON. Native libraries do not know that: TensorRT's
    default logger, CUDA and other C or C++ code write to fd 1 directly,
    below sys.stdout. After a reboot TensorRT printed a warning there while
    loading the cached engine, and the agent killed the worker every few
    seconds, with no detections at all. After this call nothing but the
    protocol can write to the channel: anything else written to fd 1, from
    C, C++ or a stray print, lands on stderr, which the agent keeps as the
    worker's diagnostics.

    Call it first, before onnxruntime, TensorRT or CUDA is imported. The
    duplicate is not inherited (PEP 446), so a child process cannot write to
    the channel either."""
    sys.stdout.flush()
    channel = os.dup(1)
    os.dup2(2, 1)
    return os.fdopen(channel, "w", encoding="utf-8")


def tensorrt_logger(trt):
    """A TensorRT logger that writes warnings and errors to stderr, labelled
    as TensorRT's own logger labels them, and keeps the last few for the
    status. TensorRT's built-in trt.Logger prints to stdout.

    Keep it referenced for as long as the builder or runtime it was given to
    lives: TensorRT holds only a pointer to it."""

    class StderrLogger(trt.ILogger):
        def __init__(self):
            trt.ILogger.__init__(self)
            self.recent = deque(maxlen=4)

        def log(self, severity, msg):
            # TensorRT calls this from C++, where an exception would abort
            # the process: a logger must never raise.
            try:
                level = int(getattr(severity, "value", severity))
                if level > TENSORRT_LOG_SEVERITY:
                    return
                label = "FEWIV"[level] if 0 <= level < 5 else str(level)
                self.recent.append(short(msg, 300))
                print(f"[TRT] [{label}] {msg}", file=sys.stderr, flush=True)
            except Exception:
                pass

    return StderrLogger()


# ---------------------------------------------------------------------------
# Engine cache


def key_fields(model_sha256, tensorrt_version, library_version, cuda_version, gpu_arch,
               input_shape, output_shape, workspace_mib):
    """Everything an engine depends on. Two engines with equal fields are
    interchangeable; any difference means a rebuild."""
    return {
        "schema": SCHEMA,
        "model_sha256": model_sha256,
        "tensorrt": tensorrt_version,
        "libnvinfer": library_version,
        "cuda": cuda_version,
        "gpu_arch": gpu_arch,
        "precision": PRECISION,
        "input_shape": [int(n) for n in input_shape],
        "output_shape": [int(n) for n in output_shape],
        "builder": {"optimization_level": OPTIMIZATION_LEVEL, "workspace_mib": int(workspace_mib)},
    }


def cache_key(fields):
    canonical = json.dumps(fields, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()


def _fsync_directory(path):
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _write_synced(path, payload):
    with open(path, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())


class EngineCache:
    """engines/<key[:32]>/{engine.plan,engine.json} under the agent's inference
    directory, which only root can read or write.

    Nothing is loaded on trust. A lookup recomputes the key, requires every
    field of the record to equal it, and hashes the plan against the record.
    Only a builder holding the build lock writes, into a private temporary
    directory that is renamed into place after the plan deserialized, so a
    partial build is never visible."""

    def __init__(self, root):
        self.root = root

    def directory(self, key):
        return os.path.join(self.root, key[:32])

    def lookup(self, key, fields):
        """(plan bytes, record, None) for a verified engine, else
        (None, None, why not). A missing engine is not a problem: why is None."""
        directory = self.directory(key)
        try:
            with open(os.path.join(directory, "engine.json"), "rb") as handle:
                record = json.loads(handle.read(1 << 20))
        except FileNotFoundError:
            return None, None, None
        except (OSError, ValueError) as exc:
            return None, None, f"engine record is unreadable ({type(exc).__name__})"
        if not isinstance(record, dict) or record.get("key") != key or record.get("fields") != fields:
            return None, None, "engine record does not match this model, TensorRT and GPU"
        try:
            with open(os.path.join(directory, "engine.plan"), "rb") as handle:
                plan = handle.read(MAX_PLAN_BYTES + 1)
        except OSError as exc:
            return None, None, f"engine plan is unreadable ({type(exc).__name__})"
        if len(plan) != record.get("plan_bytes") or hashlib.sha256(plan).hexdigest() != record.get("plan_sha256"):
            return None, None, "engine plan does not match its record"
        return plan, record, None

    def store(self, key, fields, plan, build_seconds):
        """Promotes a built plan. Call only while holding the build lock."""
        record = {"key": key, "fields": fields, "plan_sha256": hashlib.sha256(plan).hexdigest(),
                  "plan_bytes": len(plan), "built_at": utc_now(), "build_seconds": round(build_seconds, 1)}
        temporary = tempfile.mkdtemp(prefix=".tmp-", dir=self.root)
        try:
            _write_synced(os.path.join(temporary, "engine.plan"), plan)
            _write_synced(os.path.join(temporary, "engine.json"), json.dumps(record, indent=1).encode())
            _fsync_directory(temporary)
            final = self.directory(key)
            if os.path.lexists(final):
                shutil.rmtree(final)
            os.rename(temporary, final)
            _fsync_directory(self.root)
        except BaseException:
            shutil.rmtree(temporary, ignore_errors=True)
            raise
        return record

    def discard(self, key):
        """Removes an engine. Call only while holding the build lock."""
        shutil.rmtree(self.directory(key), ignore_errors=True)

    def remove_partial_builds(self):
        """Removes temporary directories of builders that died. Call only while
        holding the build lock: nobody else is writing one."""
        for name in os.listdir(self.root):
            if name.startswith(".tmp-"):
                shutil.rmtree(os.path.join(self.root, name), ignore_errors=True)

    def tombstone_path(self, key):
        return self.directory(key) + ".failed.json"

    def tombstone(self, key, error, now=None):
        payload = json.dumps({"key": key, "error": short(error, 500),
                              "failed_at": time.time() if now is None else now}).encode()
        descriptor, temporary = tempfile.mkstemp(prefix=".tmp-failed-", dir=self.root)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(payload)
            os.replace(temporary, self.tombstone_path(key))
        except BaseException:
            try:
                os.unlink(temporary)
            except OSError:
                pass
            raise

    def failed(self, key, now=None):
        """Why the last build of key failed, while that failure still holds."""
        try:
            with open(self.tombstone_path(key), "rb") as handle:
                record = json.loads(handle.read(1 << 16))
        except (OSError, ValueError):
            return None
        now = time.time() if now is None else now
        if not isinstance(record, dict) or record.get("key") != key:
            return None
        failed_at = record.get("failed_at")
        if not isinstance(failed_at, (int, float)) or not 0 <= now - failed_at < TOMBSTONE_SECONDS:
            return None
        hours = max(1, round((TOMBSTONE_SECONDS - (now - failed_at)) / 3600))
        return f"the engine build failed ({record.get('error')}); it is retried in about {hours}h or when the model changes"


# ---------------------------------------------------------------------------
# CUDA and TensorRT


class Cuda:
    """The few CUDA runtime calls an engine needs, through ctypes on the
    image's libcudart: the library TensorRT itself links, so no CUDA wheel."""

    MEMCPY_HOST_TO_DEVICE = 1
    MEMCPY_DEVICE_TO_HOST = 2
    STREAM_NON_BLOCKING = 1
    ATTRIBUTE_CAPABILITY_MAJOR = 75
    ATTRIBUTE_CAPABILITY_MINOR = 76

    def __init__(self, library):
        c_int, c_void, c_size = ctypes.c_int, ctypes.c_void_p, ctypes.c_size_t
        pointer = ctypes.POINTER
        self.lib = ctypes.CDLL(library)
        for name, restype, argtypes in (
            ("cudaGetErrorString", ctypes.c_char_p, [c_int]),
            ("cudaRuntimeGetVersion", c_int, [pointer(c_int)]),
            ("cudaSetDevice", c_int, [c_int]),
            ("cudaDeviceGetAttribute", c_int, [pointer(c_int), c_int, c_int]),
            ("cudaMalloc", c_int, [pointer(c_void), c_size]),
            ("cudaFree", c_int, [c_void]),
            ("cudaMemcpyAsync", c_int, [c_void, c_void, c_size, c_int, c_void]),
            ("cudaDeviceGetStreamPriorityRange", c_int, [pointer(c_int), pointer(c_int)]),
            ("cudaStreamCreateWithPriority", c_int, [pointer(c_void), ctypes.c_uint, c_int]),
            ("cudaStreamSynchronize", c_int, [c_void]),
            ("cudaStreamDestroy", c_int, [c_void]),
            ("cudaDeviceReset", c_int, []),
        ):
            function = getattr(self.lib, name)
            function.restype, function.argtypes = restype, argtypes
        # Creates the process's CUDA context: a wedged GPU fails here, before
        # anything is built.
        self.check(self.lib.cudaSetDevice(0), "cudaSetDevice")

    def check(self, code, call):
        if code:
            message = self.lib.cudaGetErrorString(code)
            raise EngineError(f"{call} failed: {message.decode(errors='replace') if message else code} ({code})")

    def runtime_version(self):
        version = ctypes.c_int()
        self.check(self.lib.cudaRuntimeGetVersion(ctypes.byref(version)), "cudaRuntimeGetVersion")
        return f"{version.value // 1000}.{version.value % 1000 // 10}"

    def gpu_arch(self):
        major, minor = ctypes.c_int(), ctypes.c_int()
        self.check(self.lib.cudaDeviceGetAttribute(ctypes.byref(major), self.ATTRIBUTE_CAPABILITY_MAJOR, 0),
                   "cudaDeviceGetAttribute")
        self.check(self.lib.cudaDeviceGetAttribute(ctypes.byref(minor), self.ATTRIBUTE_CAPABILITY_MINOR, 0),
                   "cudaDeviceGetAttribute")
        return f"sm_{major.value}{minor.value}"

    def malloc(self, size):
        address = ctypes.c_void_p()
        self.check(self.lib.cudaMalloc(ctypes.byref(address), size), "cudaMalloc")
        return address

    def free(self, address):
        self.lib.cudaFree(address)

    def stream(self):
        """A stream at the lowest priority, so apps sharing the GPU go first."""
        least, greatest = ctypes.c_int(), ctypes.c_int()
        self.check(self.lib.cudaDeviceGetStreamPriorityRange(ctypes.byref(least), ctypes.byref(greatest)),
                   "cudaDeviceGetStreamPriorityRange")
        stream = ctypes.c_void_p()
        self.check(self.lib.cudaStreamCreateWithPriority(ctypes.byref(stream), self.STREAM_NON_BLOCKING, least.value),
                   "cudaStreamCreateWithPriority")
        return stream

    def to_device(self, destination, array, stream):
        self.check(self.lib.cudaMemcpyAsync(destination, array.ctypes.data, array.nbytes,
                                            self.MEMCPY_HOST_TO_DEVICE, stream), "cudaMemcpyAsync")

    def to_host(self, array, source, stream):
        self.check(self.lib.cudaMemcpyAsync(array.ctypes.data, source, array.nbytes,
                                            self.MEMCPY_DEVICE_TO_HOST, stream), "cudaMemcpyAsync")

    def synchronize(self, stream):
        self.check(self.lib.cudaStreamSynchronize(stream), "cudaStreamSynchronize")

    def destroy_stream(self, stream):
        self.lib.cudaStreamDestroy(stream)

    def reset(self):
        """Releases this process's CUDA context and everything in it."""
        self.lib.cudaDeviceReset()


def import_tensorrt(bindings):
    """The tensorrt module from the bindings directory the agent verified. The
    directory is appended to sys.path, never prepended: nothing in it can
    shadow a package of the worker's own locked environment."""
    if bindings not in sys.path:
        sys.path.append(bindings)
    import tensorrt
    return tensorrt


def probe(bindings, cuda_runtime):
    """What the engine cache key needs from TensorRT and the GPU, read by
    loading them: TensorRT's version, the CUDA runtime's, and the GPU's compute
    capability. Raises EngineError with the reason they did not load.

    Only the --probe child calls this. A process that loads TensorRT and CUDA
    keeps about 500 MB of them, and the worker would hold that through a build
    delay and every wait for memory or CPU, scoring on the CPU all along."""
    try:
        version = import_tensorrt(bindings).__version__
    except Exception as exc:
        raise EngineError(f"the TensorRT Python bindings did not load ({type(exc).__name__}: {short(exc, 200)})")
    try:
        cuda = Cuda(cuda_runtime)
        return {"tensorrt": version, "cuda": cuda.runtime_version(), "gpu_arch": cuda.gpu_arch()}
    except Exception as exc:
        raise EngineError(f"CUDA is unavailable ({short(exc, 200)})")


class CpuEngine:
    """ONNX Runtime on the CPU: the engine every campaign could always use."""

    name = "cpu"
    precision = "fp32"

    def __init__(self, session, input_name, runtime="ONNX Runtime"):
        self.session, self.input_name, self.runtime = session, input_name, runtime

    def infer(self, tensor):
        return self.session.run(None, {self.input_name: tensor})[0]

    def close(self):
        pass


class TensorRTEngine:
    """A deserialized plan with one float32 input and one float32 output, run
    on one low-priority stream."""

    name = "tensorrt"
    precision = PRECISION

    def __init__(self, trt, cuda, plan, input_shape, output_shape):
        import numpy as np

        self.trt, self.cuda, self.np = trt, cuda, np
        self.runtime = f"TensorRT {trt.__version__}"
        self.buffers, self.stream = [], None
        self.logger = tensorrt_logger(trt)
        self.trt_runtime = trt.Runtime(self.logger)
        self.engine = self.trt_runtime.deserialize_cuda_engine(plan)
        # What TensorRT warned about while loading, such as a plan it thinks
        # was built on "a different model of device". Reported in the status.
        self.warnings = list(self.logger.recent)
        if self.engine is None:
            raise EngineError("TensorRT could not deserialize the engine plan")
        try:
            self.context = self.engine.create_execution_context()
            if self.context is None:
                raise EngineError("TensorRT could not create an execution context")
            names = [self.engine.get_tensor_name(i) for i in range(self.engine.num_io_tensors)]
            inputs = [n for n in names if self.engine.get_tensor_mode(n) == trt.TensorIOMode.INPUT]
            outputs = [n for n in names if self.engine.get_tensor_mode(n) == trt.TensorIOMode.OUTPUT]
            if len(inputs) != 1 or len(outputs) != 1:
                raise EngineError("the engine must have one input and one output")
            for name, shape in ((inputs[0], input_shape), (outputs[0], output_shape)):
                if list(self.engine.get_tensor_shape(name)) != list(shape):
                    raise EngineError(f"engine tensor {name} has shape {list(self.engine.get_tensor_shape(name))}, "
                                      f"the model has {list(shape)}")
                if self.engine.get_tensor_dtype(name) != trt.float32:
                    raise EngineError(f"engine tensor {name} is not float32")
            self.output = np.empty(output_shape, dtype=np.float32)
            self.stream = cuda.stream()
            self.input_buffer = cuda.malloc(int(np.prod(input_shape)) * 4)
            self.buffers.append(self.input_buffer)
            self.output_buffer = cuda.malloc(self.output.nbytes)
            self.buffers.append(self.output_buffer)
            self.context.set_tensor_address(inputs[0], self.input_buffer.value)
            self.context.set_tensor_address(outputs[0], self.output_buffer.value)
        except BaseException:
            self.close()
            raise

    def device_bytes(self):
        """Activation memory TensorRT asked for, a lower bound on what the
        engine takes."""
        size = getattr(self.engine, "device_memory_size_v2", None)
        if size is None:
            size = getattr(self.engine, "device_memory_size", 0)
        return int(size or 0)

    def infer(self, tensor):
        tensor = self.np.ascontiguousarray(tensor, dtype=self.np.float32)
        self.cuda.to_device(self.input_buffer, tensor, self.stream)
        if not self.context.execute_async_v3(self.stream.value or 0):
            raise EngineError("TensorRT execute_async_v3 failed")
        self.cuda.to_host(self.output, self.output_buffer, self.stream)
        self.cuda.synchronize(self.stream)
        return self.output.copy()

    def close(self):
        # TensorRT objects go before the memory and stream they point at.
        for name in ("context", "engine", "trt_runtime"):
            if hasattr(self, name):
                delattr(self, name)
        for buffer in self.buffers:
            self.cuda.free(buffer)
        self.buffers = []
        if self.stream is not None:
            self.cuda.destroy_stream(self.stream)
            self.stream = None


# ---------------------------------------------------------------------------
# Parity


def iou(a, b):
    width = max(0.0, min(a[2], b[2]) - max(a[0], b[0]))
    height = max(0.0, min(a[3], b[3]) - max(a[1], b[1]))
    intersection = width * height
    union = (a[2] - a[0]) * (a[3] - a[1]) + (b[2] - b[0]) * (b[3] - b[1]) - intersection
    return intersection / union if union > 0 else 0.0


def _unmatched(ours, theirs, threshold):
    """The first detection in ours, clearly above threshold, that theirs lacks,
    and the smallest IoU among the ones it has."""
    worst = 1.0
    for detection in ours:
        if detection["score"] < threshold + PARITY_SCORE:
            continue
        same = [d for d in theirs if d["label"] == detection["label"]]
        best = max(same, key=lambda d: iou(detection["box"], d["box"]), default=None)
        overlap = 0.0 if best is None else iou(detection["box"], best["box"])
        if best is None or overlap < PARITY_IOU or abs(best["score"] - detection["score"]) > PARITY_SCORE:
            return detection, overlap
        worst = min(worst, overlap)
    return None, worst


def parity(cpu, gpu, tensors, decode, threshold):
    """Scores the same tensors on both engines. The result's problem is None
    when TensorRT agrees with the CPU within the stated tolerances."""
    import numpy as np

    result = {"passed": False, "frames": 0, "max_score_delta": 0.0, "min_iou": 1.0}
    for tensor in tensors:
        expected, actual = cpu.infer(tensor), gpu.infer(tensor)
        result["frames"] += 1
        if expected.shape != actual.shape or not np.isfinite(actual).all():
            return result, "tensorrt output has the wrong shape or non-finite values"
        delta = float(np.abs(expected[0, 4:, :] - actual[0, 4:, :]).max()) if expected.shape[1] > 4 else 0.0
        result["max_score_delta"] = max(result["max_score_delta"], round(delta, 4))
        cpu_detections, gpu_detections = decode(expected), decode(actual)
        for ours, theirs, side in ((cpu_detections, gpu_detections, "the cpu"),
                                   (gpu_detections, cpu_detections, "tensorrt")):
            missing, worst = _unmatched(ours, theirs, threshold)
            result["min_iou"] = round(min(result["min_iou"], worst), 4)
            if missing is not None:
                return result, (f"tensorrt disagrees with the cpu: {side} found {missing['label']} "
                                f"{missing['score']:.2f} that the other has no match for "
                                f"(IoU >= {PARITY_IOU}, score within {PARITY_SCORE})")
        if delta > PARITY_RAW_SCORE:
            return result, f"tensorrt disagrees with the cpu: a class score differs by {delta:.3f} (> {PARITY_RAW_SCORE})"
    result["passed"] = True
    return result, None


# ---------------------------------------------------------------------------
# The accelerator in the worker


def plan_status(plan, runtime, precision="fp32"):
    """Status of a worker that only ever runs on the CPU, as the agent planned it."""
    plan = plan or {}
    return {"requested": plan.get("requested", "auto"), "active": "cpu",
            "candidates": plan.get("candidates") or ["cpu"], "degraded": bool(plan.get("degraded")),
            "reason": plan.get("reason", ""), "runtime": runtime, "precision": precision}


class Accelerator:
    """Chooses and runs the engine for one YOLO model.

    infer() is called from the worker's scoring loop. A background thread
    finds or builds the TensorRT engine, checks it against the CPU and swaps
    it in. The CPU engine stays loaded throughout, so a GPU failure costs one
    frame's latency, never a frame."""

    def __init__(self, plan, cpu, model, emit, exit=os._exit):
        """plan is the agent's accelerator_plan. model holds path, sha256,
        input_shape, output_shape, threshold and decode(output) for parity."""
        import numpy as np

        self.np = np
        self.plan = plan or {}
        self.tensorrt_plan = self.plan.get("tensorrt") or {}
        self.strict = self.plan.get("requested") == "tensorrt"
        self.cpu, self.model, self.emit, self.exit = cpu, model, emit, exit
        self.lock = threading.Lock()
        self.stopped = threading.Event()
        # The engine scoring frames. A tensorrt campaign has none until its
        # engine is ready: it never scores on the CPU.
        self.engine = None if self.strict else cpu
        self.gpu = None
        self.cuda = None
        self.offered = "tensorrt" in (self.plan.get("candidates") or []) and bool(self.tensorrt_plan)
        # Recent preprocessed frames, kept for the parity check until it ran.
        self.frames = deque(maxlen=PARITY_FRAMES)
        self.collecting = self.offered
        self.latencies = deque(maxlen=256)
        self.scored = deque(maxlen=512)
        self.count = 0
        # Whether this worker already waited out its build delay.
        self.staggered = False
        # The build's waits for memory and the CPU, by gate: see wait_clock.
        self.waits = {}
        # The tensorrt module, once an engine is about to load; and what
        # importing it took, as a growth() pair.
        self.trt = None
        self.import_cost = (0.0, 0.0)
        self.state = plan_status(self.plan, cpu.runtime, cpu.precision)
        self.state["model_sha256"] = model["sha256"]
        if self.strict:
            self.state.update(active="", runtime="", precision="")
        if self.offered:
            self.state["reason"] = NOT_READY

    # -- the scoring loop ---------------------------------------------------

    def infer(self, tensor):
        """The model output and the engine that produced it, or (None, None)
        while a tensorrt campaign waits for its engine."""
        if self.collecting:
            self.frames.append(tensor)
        engine = self.engine
        if engine is None:
            return None, None
        started = time.perf_counter()
        try:
            output = engine.infer(tensor)
        except Exception as exc:
            if engine is self.cpu or self.strict:
                raise
            self.fall_back(f"tensorrt inference failed ({type(exc).__name__}: {short(exc, 200)})")
            engine = self.cpu
            started = time.perf_counter()
            output = engine.infer(tensor)
        finished = time.perf_counter()
        with self.lock:
            if engine is self.engine:
                self.latencies.append((finished - started) * 1000)
                self.scored.append(time.monotonic())
                self.count += 1
        return output, engine

    # -- status -------------------------------------------------------------

    def status(self):
        with self.lock:
            state = json.loads(json.dumps(self.state))
        engine = state.get("engine")
        if engine:
            started = engine.pop("started", None)
            if engine.get("state") == "building" and started is not None:
                engine["elapsed_seconds"] = round(time.monotonic() - started, 1)
        return state

    def stats(self):
        with self.lock:
            latencies = sorted(self.latencies)
            scored = list(self.scored)
            count = self.count
        now = time.monotonic()
        recent = [t for t in scored if now - t <= 30]
        span = recent[-1] - recent[0] if len(recent) > 1 else 0

        def percentile(fraction):
            if not latencies:
                return 0.0
            return round(latencies[min(len(latencies) - 1, int(fraction * len(latencies)))], 2)
        return {"fps": round((len(recent) - 1) / span, 2) if span > 0 else 0.0,
                "infer_ms_p50": percentile(0.5), "infer_ms_p95": percentile(0.95), "frames_scored": count}

    def report(self):
        self.emit({"type": "accelerator", "accelerator": self.status(), "stats": self.stats()})

    def update(self, engine=None, **changes):
        """Changes the status and reports it at once."""
        with self.lock:
            self.state.update(changes)
            if engine is not None:
                current = self.state.setdefault("engine", {})
                if engine.get("state", current.get("state")) != current.get("state"):
                    for field in TRANSIENT_ENGINE_FIELDS:
                        current.pop(field, None)
                current.update(engine)
        self.report()

    def fall_back(self, reason):
        """Stops using TensorRT. On auto the CPU carries on, reported as
        degraded; on tensorrt the worker ends with reason as its error."""
        print(f"accelerator: {reason}", file=sys.stderr, flush=True)
        with self.lock:
            gpu, self.gpu = self.gpu, None
            self.engine = None if self.strict else self.cpu
            self.collecting = False
            self.frames.clear()
            self.latencies.clear()
            engine = self.state.get("engine")
            if engine is not None and engine.get("state") != "failed":
                engine.update(state="failed", error=reason)
        if gpu is not None:
            gpu.close()
        if self.cuda is not None:
            self.cuda.reset()
            self.cuda = None
        if self.strict:
            self.update(active="", degraded=True, reason=reason)
            self.emit({"type": "error", "error": short(reason, 500)})
            self.exit(1)
            return
        self.update(active="cpu", degraded=True, reason=reason, runtime=self.cpu.runtime,
                    precision=self.cpu.precision, gpu_memory_mib=0)

    # -- the background thread ----------------------------------------------

    def start(self):
        threading.Thread(target=self._report_periodically, daemon=True).start()
        if self.offered:
            threading.Thread(target=self._accelerate_safely, daemon=True).start()
        elif self.strict:
            self.fall_back("tensorrt is unavailable: " + (self.plan.get("reason") or "the agent offered no TensorRT"))

    def stop(self):
        self.stopped.set()

    def wait(self, seconds):
        """False when the worker is stopping."""
        return not self.stopped.wait(seconds)

    def _report_periodically(self):
        while self.wait(STATUS_SECONDS):
            self.report()

    def _accelerate_safely(self):
        try:
            self.accelerate()
        except Exception as exc:
            self.fall_back(f"tensorrt: {type(exc).__name__}: {short(exc)}")

    # The steps that touch TensorRT, CUDA or the machine are methods, so tests
    # can replace them.

    def probe(self):
        """probe() in a --probe child, so that this process loads neither
        TensorRT nor CUDA before an engine is about to load. Its fd 1 is
        claimed like the builder's, and it stays in the worker's process group.
        Raises EngineError with the reason."""
        job = {"bindings": self.tensorrt_plan["bindings"], "cuda_runtime": self.tensorrt_plan["cuda_runtime"]}
        child = subprocess.Popen([sys.executable, "-u", os.path.abspath(__file__), "--probe"],
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, close_fds=True)
        try:
            output, _ = child.communicate(json.dumps(job).encode(), timeout=PROBE_TIMEOUT_SECONDS)
        except subprocess.TimeoutExpired:
            child.kill()
            child.communicate()
            raise EngineError(f"loading TensorRT and CUDA took longer than {PROBE_TIMEOUT_SECONDS} s")
        message = {}
        for line in output.splitlines():
            try:
                parsed = json.loads(line)
            except ValueError:
                continue
            if isinstance(parsed, dict):
                message = parsed
        if child.returncode == 0 and all(isinstance(message.get(name), str) for name in ("tensorrt", "cuda", "gpu_arch")):
            return message
        if message.get("error"):
            raise EngineError(short(message["error"], 500))
        if child.returncode < 0:
            raise EngineError(f"the process loading TensorRT and CUDA was killed by signal {-child.returncode}")
        raise EngineError(f"the process loading TensorRT and CUDA exited with code {child.returncode}")

    def import_tensorrt(self):
        return import_tensorrt(self.tensorrt_plan["bindings"])

    def tensorrt(self):
        """The tensorrt module, imported on first use: when an engine is about
        to load, never while a build is scheduled or waits. The import is the
        first memory window (see memory_cost)."""
        if self.trt is None:
            before = self.memory_sample()
            self.trt = self.import_tensorrt()
            self.import_cost = self.growth(before, self.memory_sample())
        return self.trt

    def open_cuda(self):
        return Cuda(self.tensorrt_plan["cuda_runtime"])

    def available_mib(self):
        return available_mib()

    def memory_sample(self):
        """(MemAvailable, this process's RSS), in MiB, either None off Linux."""
        return available_mib(), rss_mib()

    def idle_cpus(self):
        """(idle CPUs, online CPUs) over the next CPU_SAMPLE_SECONDS, with this
        process's own CPU time counted as idle; None when unknown or stopping.
        Idle CPUs is a fraction: 2.5 is two and a half CPUs' worth of time."""
        try:
            first = cpu_times()
            if first is None or not self.wait(CPU_SAMPLE_SECONDS):
                return None
            second = cpu_times()
        except (OSError, ValueError, IndexError):
            return None
        if second is None or second[1] <= first[1] or not second[3]:
            return None
        total = second[1] - first[1]
        others = max(0, (second[0] - first[0]) - (second[2] - first[2]))
        return (total - others) / total * second[3], second[3]

    def build_delay(self):
        return random.uniform(0, BUILD_JITTER_SECONDS)

    def load_engine(self, trt, cuda, plan):
        return TensorRTEngine(trt, cuda, plan, self.model["input_shape"], self.model["output_shape"])

    def parity_tensors(self):
        noise = self.np.random.default_rng(0).random(tuple(self.model["input_shape"]), dtype=self.np.float32)
        return list(self.frames) + [noise]

    # -- memory ---------------------------------------------------------------
    #
    # What the accelerated path costs the device, which is what its budget
    # holds it to. On a Jetson the GPU has no memory of its own: CUDA and
    # TensorRT allocate from the same RAM as every process, so the cost is
    # measured as RAM, two ways, because each misses something:
    #
    #  - The drop in MemAvailable. It sees the GPU's allocations, the CUDA
    #    context and the engine's memory, which the GPU driver allocates and
    #    a process's RSS need not count. It does not see library code read
    #    from disk, which the kernel counts as available page cache.
    #  - The growth of this process's RSS. It sees TensorRT's and CUDA's
    #    libraries and their heap, and not the GPU's allocations.
    #
    # The cost is the larger of the two, and never less than the engine's own
    # plan and activations. Both are measured over two short windows and
    # added: importing TensorRT, then opening CUDA and loading the engine,
    # with the CUDA context in it. Both come just before the engine loads,
    # since this process loads neither earlier; the import comes only before
    # the first load, so a rebuild between the two windows is not counted.
    #
    # Measuring the load alone, as before, left out the import and most of the
    # context. On a Jetson Orin Nano it reported 47 to 88 MiB, while the
    # TensorRT worker's RSS was 885 MB against 238 MB for a CPU-only one, and
    # free memory fell by about 900 MiB.

    @staticmethod
    def growth(before, after):
        """(MemAvailable drop, RSS growth) between two samples, in MiB, never
        negative; None for a measure the system lacks."""
        (available_before, rss_before), (available_after, rss_after) = before, after
        drop = None if available_before is None or available_after is None else max(0.0, available_before - available_after)
        grew = None if rss_before is None or rss_after is None else max(0.0, rss_after - rss_before)
        return drop, grew

    def memory_cost(self, loaded, engine_bytes):
        """The accelerated path's cost in MiB, from the import window, the load
        window and the engine's own size."""
        drop = sum(window[0] or 0 for window in (self.import_cost, loaded))
        grew = sum(window[1] or 0 for window in (self.import_cost, loaded))
        return max(drop, grew, engine_bytes / (1 << 20))

    def accelerate(self):
        # TensorRT and CUDA load in a child for the cache key, and in this
        # process only in load(). Before, the worker loaded them here and kept
        # about 500 MB of them through the build delay and every wait: 632 to
        # 647 MB of RSS against 146 MB for a CPU-only worker.
        try:
            found = self.probe()
        except EngineError as exc:
            return self.fall_back(str(exc))
        plan = self.tensorrt_plan
        fields = key_fields(self.model["sha256"], found["tensorrt"], plan.get("library", ""), found["cuda"],
                            found["gpu_arch"], self.model["input_shape"], self.model["output_shape"],
                            plan["workspace_mib"])
        key = cache_key(fields)
        cache = EngineCache(plan["engine_dir"])
        os.makedirs(cache.root, mode=0o700, exist_ok=True)
        self.update(gpu_arch=found["gpu_arch"], engine={"state": "checking", "key": key[:16]})
        built = rebuilt = False
        while not self.stopped.is_set():
            engine_plan, record, problem = cache.lookup(key, fields)
            if engine_plan is None:
                if problem:
                    print(f"accelerator: rebuilding the cached engine: {problem}", file=sys.stderr, flush=True)
                failure = cache.failed(key)
                if failure:
                    return self.fall_back(failure)
                outcome, detail = self.build(key, fields)
            elif not self.admit_load():
                continue
            else:
                outcome = self.load(engine_plan, record, cached=not built)
                if outcome != "retry":
                    return
                # The plan matched its record and still did not deserialize.
                # Rebuild it once; a second failure is final.
                if rebuilt:
                    cache.tombstone(key, "a freshly built engine did not deserialize")
                    return self.fall_back("the TensorRT engine did not deserialize, even after a rebuild")
                rebuilt = True
                outcome, detail = self.build(key, fields, discard=record["plan_sha256"])
            if outcome == "stopped":
                return
            if outcome == "failed":
                cache.tombstone(key, detail)
                return self.fall_back(f"the TensorRT engine build failed: {detail}")
            built = built or outcome == "done"

    def admit_load(self):
        budget = self.tensorrt_plan["gpu_budget_mib"]
        available = self.available_mib()
        if available is None or available >= budget:
            self.waits.pop("load", None)
            return True
        # Degraded, since nothing says memory will ever free up. The reason
        # stays the same from one retry to the next, so it is logged once.
        _, since, _ = self.wait_clock("load")
        self.update(degraded=True, reason=f"too little free memory to load the TensorRT engine (it needs {budget} MiB); "
                    f"retrying every {MEMORY_RETRY_SECONDS // 60 or 1} minutes",
                    engine={"state": "waiting_for_memory", "waiting_since": since,
                            "error": f"{available} MiB of memory is available; loading the engine needs {budget} MiB"})
        self.wait(MEMORY_RETRY_SECONDS)
        return False

    def load(self, engine_plan, record, cached):
        """Imports TensorRT and opens CUDA, loads the engine, holds it to its
        memory budget and the CPU's detections, and swaps it in. Returns
        ready, failed, stopped, or retry when the plan would not deserialize."""
        budget = self.tensorrt_plan["gpu_budget_mib"]
        self.update(degraded=False, reason=NOT_READY,
                    engine={"state": "verifying", "cached": cached, "plan_bytes": record["plan_bytes"],
                            "build_seconds": record.get("build_seconds", 0)})
        try:
            trt = self.tensorrt()
        except Exception as exc:
            self.fall_back(f"the TensorRT Python bindings did not load ({type(exc).__name__}: {short(exc, 200)})")
            return "failed"
        # The second memory window: the CUDA context and the engine.
        before = self.memory_sample()
        try:
            self.cuda = self.open_cuda()
        except Exception as exc:
            self.fall_back(f"CUDA is unavailable ({short(exc, 200)})")
            return "failed"
        try:
            gpu = self.load_engine(trt, self.cuda, engine_plan)
        except EngineError as exc:
            if "deserialize" in str(exc):
                self.cuda.reset()
                self.cuda = None
                return "retry"
            self.fall_back(f"the TensorRT engine did not load ({short(exc, 200)})")
            return "failed"
        used = self.memory_cost(self.growth(before, self.memory_sample()),
                                gpu.device_bytes() + record["plan_bytes"])
        with self.lock:
            self.gpu = gpu
        # Such as "Using an engine plan file across different models of
        # devices", which TensorRT prints after a reboot. The parity check
        # below decides whether the engine is still right.
        self.update(engine={"warnings": list(getattr(gpu, "warnings", []))})
        if used > budget:
            self.fall_back(f"the TensorRT engine took {used:.0f} MiB with TensorRT and its CUDA context, "
                           f"over its {budget} MiB budget")
            return "failed"
        self.update(gpu_memory_mib=round(used, 1))
        if not self.frames:
            # Nothing to compare yet, and nothing to score either.
            self.update(engine={"error": "waiting for a camera frame to compare the cpu and tensorrt on"})
            while not self.frames:
                if not self.wait(1):
                    return "stopped"
        result, problem = parity(self.cpu, gpu, self.parity_tensors(), self.model["decode"], self.model["threshold"])
        with self.lock:
            self.state["parity"] = result
        if problem:
            self.fall_back(problem)
            return "failed"
        with self.lock:
            self.engine = gpu
            self.collecting = False
            self.frames.clear()
            self.latencies.clear()
        self.update(active="tensorrt", degraded=False, reason="", runtime=gpu.runtime,
                    precision=gpu.precision, engine={"state": "ready"})
        print(f"accelerator: scoring on {gpu.runtime} {gpu.precision}; parity over {result['frames']} frames, "
              f"max score delta {result['max_score_delta']}", file=sys.stderr, flush=True)
        return "ready"

    def build(self, key, fields, discard=None):
        """Builds the engine in a child process. (outcome, detail): outcome is
        done, failed, waiting_for_memory, waiting_for_cpu or stopped.

        A build costs minutes and gigabytes, so it waits for its turn: after
        a random delay on auto (BUILD_JITTER_SECONDS), then until the device
        has the memory to spare, and the CPU, for at most
        BUILD_MAX_CPU_WAIT_SECONDS. The CPU scores meanwhile."""
        plan = self.tensorrt_plan
        if not self.strict and not self.staggered:
            self.staggered = True
            delay = self.build_delay()
            if delay > 0:
                self.update(degraded=False, reason=NOT_READY,
                            engine={"state": "scheduled", "not_before": utc_now(after=delay)})
                if not self.wait(delay):
                    return "stopped", ""
        minimum = plan["build_min_available_mib"]
        available = self.available_mib()
        if available is not None and available < minimum:
            return self.wait_for_memory(available, minimum)
        sample = self.idle_cpus()
        if self.stopped.is_set():
            return "stopped", ""
        lowest = False
        if sample is not None and sample[0] < BUILD_MIN_IDLE_CPUS:
            started, since, build_by = self.wait_clock("cpu", BUILD_MAX_CPU_WAIT_SECONDS)
            remaining = BUILD_MAX_CPU_WAIT_SECONDS - (time.monotonic() - started)
            if remaining > 0:
                return self.wait_for_cpu(*sample, since, build_by, remaining)
            lowest = True
            print(f"accelerator: the CPU stayed busy for {BUILD_MAX_CPU_WAIT_SECONDS // 60} minutes; "
                  "building the TensorRT engine anyway, at the lowest priority", file=sys.stderr, flush=True)
        # The next build, if there is one, waits afresh.
        self.waits.clear()
        job = {"key": key, "fields": fields, "model_path": self.model["path"], "bindings": plan["bindings"],
               "engine_dir": plan["engine_dir"], "workspace_mib": plan["workspace_mib"],
               "build_min_available_mib": minimum, "discard_plan_sha256": discard, "lowest_priority": lowest}
        self.update(degraded=False, reason=NOT_READY, engine=dict({"state": "queued"}, **self.priority(job)))
        return self.run_builder(job, plan.get("build_timeout_seconds", 1800))

    @staticmethod
    def priority(job):
        """The engine status field that marks a build at the lowest priority."""
        return {"low_priority": True} if job.get("lowest_priority") else {}

    def wait_clock(self, gate, limit=None):
        """(monotonic start, UTC start, UTC end or None) of a wait on gate:
        cpu or memory before a build, load before an engine loads. It ends
        limit seconds after it started, if there is a limit. A build's clocks
        start at the gate's first refusal and run until a build is queued, so
        a wait for memory in between does not restart the wait for the CPU."""
        if gate not in self.waits:
            self.waits[gate] = (time.monotonic(), utc_now(), utc_now(after=limit) if limit else None)
        return self.waits[gate]

    def wait_for_memory(self, available, minimum):
        _, since, _ = self.wait_clock("memory")
        self.update(degraded=True, reason=f"too little free memory to build the TensorRT engine (it needs {minimum} MiB); "
                    f"retrying every {MEMORY_RETRY_SECONDS // 60 or 1} minutes",
                    engine={"state": "waiting_for_memory", "waiting_since": since,
                            "error": f"{available} MiB of memory is available; a build needs {minimum} MiB"})
        return ("waiting_for_memory", "") if self.wait(MEMORY_RETRY_SECONDS) else ("stopped", "")

    def wait_for_cpu(self, idle, cpus, since, build_by, remaining):
        # Not degraded, unlike a wait for memory: the CPU detects meanwhile,
        # and the engine is coming, at build_by at the latest. The reason
        # names that time, so it stays the same from one retry to the next.
        self.update(degraded=False,
                    reason=f"the CPU is busy, so the TensorRT engine build waits for {BUILD_MIN_IDLE_CPUS} idle CPUs, "
                           f"checking every {CPU_RETRY_SECONDS // 60 or 1} minutes; "
                           f"at {build_by} it starts anyway, at the lowest priority",
                    engine={"state": "waiting_for_cpu", "waiting_since": since, "build_by": build_by,
                            "error": f"{idle:.1f} of {cpus} CPUs are idle, counting this campaign's own inference "
                                     f"as idle; a build starts with {BUILD_MIN_IDLE_CPUS}"})
        return ("waiting_for_cpu", "") if self.wait(min(CPU_RETRY_SECONDS, remaining)) else ("stopped", "")

    def run_builder(self, job, timeout):
        """Runs accelerator.py --build-engine. The child takes the device-wide
        build lock itself, so the kernel releases it however the child ends,
        and no crash or restart can keep it. The child stays in the worker's
        process group, so stopping the worker stops the build too."""
        child = subprocess.Popen([sys.executable, "-u", os.path.abspath(__file__), "--build-engine"],
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, close_fds=True)
        last = {}
        building = threading.Event()

        def read():
            for line in child.stdout:
                try:
                    message = json.loads(line)
                except ValueError:
                    continue
                last.clear()
                last.update(message)
                if message.get("state") == "building":
                    building.set()
                    self.update(engine=dict({"state": "building", "started_at": message.get("started_at") or utc_now(),
                                             "started": time.monotonic()}, **self.priority(job)))
                elif message.get("state") == "queued":
                    self.update(engine={"state": "queued", "error": "another campaign's engine is building"})

        reader = threading.Thread(target=read, daemon=True)
        reader.start()
        try:
            child.stdin.write(json.dumps(job).encode())
            child.stdin.close()
        except OSError:
            pass
        # The timeout counts the build, not the wait for the build lock: a
        # child queued behind another campaign's build (11-14 minutes on an
        # Orin) used to time out there and be tombstoned for a build that
        # never started. The wait is bounded by that other build's own
        # timeout.
        deadline = None
        while child.poll() is None:
            if deadline is None and building.is_set():
                deadline = time.monotonic() + timeout
            if self.stopped.is_set() or (deadline is not None and time.monotonic() > deadline):
                child.kill()
                child.wait()
                reader.join(timeout=5)
                child.stdout.close()
                if self.stopped.is_set():
                    return "stopped", ""
                return "failed", f"the build took longer than {timeout // 60} minutes"
            time.sleep(0.5)
        reader.join(timeout=5)
        child.stdout.close()
        if child.returncode == 0:
            return "done", ""
        if child.returncode == EXIT_WAITING_FOR_MEMORY:
            return self.wait_for_memory(last.get("available_mib"), job["build_min_available_mib"])
        if last.get("error"):
            return "failed", last["error"]
        if child.returncode < 0:
            return "failed", f"the build process was killed by signal {-child.returncode}, possibly for memory"
        return "failed", f"the build process exited with code {child.returncode}"


# ---------------------------------------------------------------------------
# The --probe and --build-engine children


def _exit_with(parent):
    """Ends the child when the worker that started it is gone."""
    while True:
        time.sleep(5)
        if os.getppid() != parent:
            os._exit(1)


def probe_main(stdin=None, stdout=None):
    """Writes one line: probe()'s versions, or the error that stopped it."""
    stdin = stdin or sys.stdin.buffer
    stdout = stdout or sys.stdout
    threading.Thread(target=_exit_with, args=(os.getppid(),), daemon=True).start()
    try:
        job = json.loads(stdin.read(1 << 16))
        message = probe(job["bindings"], job["cuda_runtime"])
    except EngineError as exc:
        message = {"error": str(exc)}
    except Exception as exc:
        message = {"error": short(f"loading TensorRT and CUDA failed ({type(exc).__name__}: {exc})", 400)}
    print(json.dumps(message), file=stdout, flush=True)
    return 1 if "error" in message else 0


def idle_io_priority():
    """Puts this process in the idle I/O class, so its disk I/O waits for
    everyone else's. True when it is; off Linux, or on another architecture,
    it is not."""
    number = IOPRIO_SET.get(os.uname().machine) if sys.platform.startswith("linux") else None
    if number is None:
        return False
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        libc.syscall.restype = ctypes.c_long
        result = libc.syscall(ctypes.c_long(number), ctypes.c_long(IOPRIO_WHO_PROCESS), ctypes.c_long(0),
                              ctypes.c_long(IOPRIO_CLASS_IDLE << IOPRIO_CLASS_SHIFT))
    except (OSError, AttributeError):
        return False
    return result == 0


def lower_priority(lowest):
    """Lowers this process's priority for a build: by BUILD_NICE, or, for a
    build that stopped waiting for idle CPUs, to BUILD_LOWEST_NICE and the idle
    I/O class, so it runs on what other processes leave. Best effort: what the
    kernel refuses stays as it was."""
    try:
        os.nice(BUILD_LOWEST_NICE if lowest else BUILD_NICE)
    except OSError:
        pass
    if lowest and not idle_io_priority():
        print("accelerator: the build could not enter the idle I/O class", file=sys.stderr, flush=True)


def build_plan(trt, model, workspace_mib):
    logger = tensorrt_logger(trt)
    builder = trt.Builder(logger)
    network = builder.create_network(0)
    parser = trt.OnnxParser(network, logger)
    if not parser.parse(model):
        errors = "; ".join(str(parser.get_error(i)) for i in range(parser.num_errors))
        raise EngineError("TensorRT could not parse the ONNX model: " + short(errors, 300))
    config = builder.create_builder_config()
    config.set_memory_pool_limit(trt.MemoryPoolType.WORKSPACE, workspace_mib << 20)
    config.set_flag(trt.BuilderFlag.FP16)
    config.builder_optimization_level = OPTIMIZATION_LEVEL
    serialized = builder.build_serialized_network(network, config)
    if serialized is None:
        raise EngineError("TensorRT could not build an engine for this model")
    plan = bytes(memoryview(serialized))
    if not 0 < len(plan) <= MAX_PLAN_BYTES:
        raise EngineError("TensorRT built an empty or oversized engine")
    # Promote only a plan that deserializes.
    if trt.Runtime(logger).deserialize_cuda_engine(plan) is None:
        raise EngineError("the built engine did not deserialize")
    return plan


def build_main(stdin=None, stdout=None):
    stdin = stdin or sys.stdin.buffer
    stdout = stdout or sys.stdout

    def say(**message):
        print(json.dumps(message), file=stdout, flush=True)

    threading.Thread(target=_exit_with, args=(os.getppid(),), daemon=True).start()
    try:
        job = json.loads(stdin.read(1 << 20))
        lower_priority(bool(job.get("lowest_priority")))
        cache = EngineCache(job["engine_dir"])
        os.makedirs(cache.root, mode=0o700, exist_ok=True)
        lock = os.open(os.path.join(cache.root, ".build.lock"), os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            say(state="queued")
            fcntl.flock(lock, fcntl.LOCK_EX)
        # The lock is held until this process exits, whichever way it does.
        cache.remove_partial_builds()
        plan, record, _ = cache.lookup(job["key"], job["fields"])
        if plan is not None and record["plan_sha256"] != job.get("discard_plan_sha256"):
            # Another campaign built it while this one waited for the lock.
            say(state="done", cached=True)
            return 0
        cache.discard(job["key"])
        available = available_mib()
        if available is not None and available < job["build_min_available_mib"]:
            say(state="waiting_for_memory", available_mib=available)
            return EXIT_WAITING_FOR_MEMORY
        say(state="building", started_at=utc_now())
        started = time.monotonic()
        with open(job["model_path"], "rb") as handle:
            model = handle.read((512 << 20) + 1)
        if hashlib.sha256(model).hexdigest() != job["fields"]["model_sha256"]:
            raise EngineError("the model file changed since the worker loaded it")
        # TensorRT and CUDA load here, once the build is sure to start.
        tensorrt = import_tensorrt(job["bindings"])
        if tensorrt.__version__ != job["fields"]["tensorrt"]:
            raise EngineError(f"TensorRT {tensorrt.__version__} loaded, and the engine's key names "
                              f"{job['fields']['tensorrt']}")
        plan = build_plan(tensorrt, model, job["workspace_mib"])
        record = cache.store(job["key"], job["fields"], plan, time.monotonic() - started)
        say(state="done", build_seconds=record["build_seconds"], plan_bytes=record["plan_bytes"])
        return 0
    except Exception as exc:
        say(state="failed", error=short(f"{type(exc).__name__}: {exc}", 400))
        return 1


if __name__ == "__main__":
    # The worker reads these processes' stdout as JSON too, so fd 1 is claimed
    # before anything else, and before TensorRT or CUDA loads.
    if sys.argv[1:] == ["--probe"]:
        sys.exit(probe_main(stdout=claim_stdout()))
    if sys.argv[1:] == ["--build-engine"]:
        sys.exit(build_main(stdout=claim_stdout()))
    sys.exit("usage: accelerator.py --probe|--build-engine < job.json")
