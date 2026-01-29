#!/usr/bin/env python3
"""
DeepStream YOLO Detector with VLM Integration and Performance Instrumentation

This detector provides detailed metrics on:
- Per-frame inference latency
- Pipeline stage breakdown (decode, preprocess, inference, postprocess)
- GPU vs CPU usage by operation
- Detections per second per camera stream
- Resource utilization
- VLM-enhanced descriptions for high-confidence detections

Metrics are exposed on :9090/metrics in Prometheus format
VLM integration provides detailed scene understanding beyond basic detection
"""

import sys
import os
import gi
import gc
import json
import time
import logging
import threading
import numpy as np
import cv2
import io
from queue import Queue
from datetime import datetime
from typing import Dict, List, Optional
from collections import defaultdict

# Set environment variables for DeepStream BEFORE GStreamer initialization
os.environ['EGL_PLATFORM'] = 'device'
os.environ['LD_LIBRARY_PATH'] = '/opt/nvidia/deepstream/deepstream-7.1/lib:/usr/lib/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu:/usr/lib'
os.environ['GST_PLUGIN_PATH'] = '/usr/lib/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu/gstreamer-1.0'
os.environ['GST_DEBUG'] = '1'  # 0=errors, 1=warnings, 2=info

gi.require_version('Gst', '1.0')
from gi.repository import GLib, Gst

import pyds
from flask import Flask, Response, request
from prometheus_client import Counter, Gauge, Histogram, generate_latest, REGISTRY

# Import VLM client
try:
    from vlm_client import VLMClient, get_prompt_for_class
    VLM_AVAILABLE = True
except ImportError:
    VLM_AVAILABLE = False
    logging.warning("VLM client not available - running without descriptions")

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

# OpenTelemetry logging - ships logs to WendyOS OTel collector
try:
    from opentelemetry.sdk._logs import LoggerProvider
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry._logs import set_logger_provider
    import opentelemetry.sdk._logs as otel_logs

    resource = Resource.create({"service.name": "detector"})
    logger_provider = LoggerProvider(resource=resource)
    logger_provider.add_log_record_processor(
        BatchLogRecordProcessor(OTLPLogExporter(endpoint="http://127.0.0.1:4318/v1/logs"))
    )
    set_logger_provider(logger_provider)

    otel_handler = otel_logs.LoggingHandler(logger_provider=logger_provider)
    logging.getLogger().addHandler(otel_handler)
    logger.info("OpenTelemetry logging enabled")
except ImportError:
    logger.warning("OpenTelemetry SDK not available - logs will not be shipped to OTel")
except Exception as e:
    logger.warning(f"Failed to initialize OpenTelemetry logging: {e}")

# Prometheus metrics
fps_gauge = Gauge('deepstream_fps', 'Frames per second', ['stream'])
inference_latency = Histogram('deepstream_inference_latency_ms', 'Inference latency in milliseconds', ['stream'])
decode_latency = Histogram('deepstream_decode_latency_ms', 'Decode latency in milliseconds', ['stream'])
preprocess_latency = Histogram('deepstream_preprocess_latency_ms', 'Preprocessing latency in milliseconds', ['stream'])
postprocess_latency = Histogram('deepstream_postprocess_latency_ms', 'Postprocessing latency in milliseconds', ['stream'])
total_latency = Histogram('deepstream_total_latency_ms', 'Total pipeline latency in milliseconds', ['stream'])
detections_counter = Counter('deepstream_detections_total', 'Total detections', ['stream', 'class_'])
gpu_memory_gauge = Gauge('deepstream_gpu_memory_mb', 'GPU memory used in MB')
frames_processed = Counter('deepstream_frames_processed_total', 'Total frames processed', ['stream'])

# Pipeline element metrics
gpu_decode_gauge = Gauge('deepstream_gpu_usage_percent', 'GPU usage percentage by operation', ['operation'])
cpu_usage_gauge = Gauge('deepstream_cpu_usage_percent', 'CPU usage percentage by operation', ['operation'])

# VLM metrics
vlm_descriptions_counter = Counter('deepstream_vlm_descriptions_total', 'Total VLM descriptions generated', ['stream'])
vlm_latency = Histogram('deepstream_vlm_latency_ms', 'VLM description latency in milliseconds', ['stream'])
vlm_cache_hits = Counter('deepstream_vlm_cache_hits_total', 'VLM description cache hits', ['stream'])
vlm_queue_size = Gauge('deepstream_vlm_queue_size', 'Number of pending VLM requests')

# MJPEG streaming - global queue for latest frame with bounding boxes
# Only stores the most recent frame to minimize memory usage
latest_frame_queue = Queue(maxsize=1)

# Track active MJPEG clients to avoid unnecessary CPU copies
mjpeg_client_count = 0
mjpeg_client_lock = threading.Lock()


class VLMIntegration:
    """
    Manages VLM integration with DeepStream detector

    Strategy:
    - Process only high-confidence detections (>0.8)
    - Rate limit to ~5-10 VLM calls per second
    - Cache descriptions by spatial location (don't re-describe same region)
    - Priority: person > vehicle > other
    - ASYNC: VLM calls run in background thread to not block pipeline
    """

    def __init__(self, vlm_client: Optional['VLMClient'] = None):
        self.vlm = vlm_client
        self.descriptions_cache = {}  # track_id -> (description, timestamp)
        self.spatial_cache = {}  # spatial_key -> (description, timestamp)
        self.last_vlm_call = 0
        self.min_interval = 10.0  # Minimum 10s between VLM calls - VLM is slow on Jetson
        self.confidence_threshold = 0.7  # Lowered to 0.7 since we raised nvinfer threshold
        self.interesting_classes = {0, 1, 2, 3, 7}  # person, bicycle, car, motorcycle, truck
        self.descriptions_cache_ttl = 600.0  # Cache track descriptions for 10 minutes
        self.spatial_cache_ttl = 300.0  # Cache spatial descriptions for 5 minutes (for parked cars)
        self.spatial_iou_threshold = 0.5  # Consider same region if IOU > 50%
        self.last_cache_cleanup = 0
        self.cache_cleanup_interval = 60.0  # Cleanup expired entries every 60 seconds

        # Async processing
        self.request_queue = Queue(maxsize=3)  # Reduced from 5 to prevent memory buildup
        self.processing = False
        self.worker_thread = None
        self.consecutive_timeouts = 0
        self.max_consecutive_timeouts = 3  # Disable VLM after 3 consecutive timeouts

        # Custom prompt override (set via API)
        self.custom_prompt = None  # When set, overrides class-specific prompts

        # Auto-reconnection
        self.last_reconnect_attempt = 0
        self.reconnect_interval = 30.0  # Try to reconnect every 30 seconds

        # Recent descriptions storage for dashboard display
        self.recent_descriptions = []  # List of {timestamp, class, track_id, description, image_b64}
        self.max_recent_descriptions = 20  # Keep last 20 descriptions
        self.recent_descriptions_lock = threading.Lock()

        self._start_worker()

    def _start_worker(self):
        """Start background worker thread for VLM processing"""
        self.processing = True
        self.worker_thread = threading.Thread(target=self._worker_loop, daemon=True)
        self.worker_thread.start()
        logger.info("VLM async worker started")

    def stop(self, timeout: float = 5.0):
        """Gracefully stop the VLM worker thread"""
        if not self.processing:
            return

        logger.info("Stopping VLM worker...")
        self.processing = False

        # Send shutdown signal to wake the worker
        try:
            self.request_queue.put_nowait(None)
        except Exception:
            pass

        # Wait for worker to finish
        if self.worker_thread and self.worker_thread.is_alive():
            self.worker_thread.join(timeout=timeout)
            if self.worker_thread.is_alive():
                logger.warning(f"VLM worker did not stop within {timeout}s timeout")
            else:
                logger.info("VLM worker stopped gracefully")

        # Drain any remaining items from queue
        drained = 0
        while not self.request_queue.empty():
            try:
                self.request_queue.get_nowait()
                drained += 1
            except Exception:
                break
        if drained > 0:
            logger.info(f"Drained {drained} pending VLM requests from queue")

    def _worker_loop(self):
        """Background worker that processes VLM requests"""
        while self.processing:
            try:
                # Wait for a request (with timeout to allow clean shutdown)
                request = self.request_queue.get(timeout=1.0)
                if request is None:  # Shutdown signal
                    break

                crop, class_name, prompt, spatial_key, track_id, stream_name = request

                # Call VLM (this is the slow part)
                vlm_start = time.time()
                try:
                    description = self.vlm.describe(crop, prompt=prompt)
                    vlm_elapsed_ms = (time.time() - vlm_start) * 1000

                    if description:
                        # Reset timeout counter on success
                        self.consecutive_timeouts = 0

                        # Cache by track_id if available, otherwise by spatial location
                        # Both caches store (description, timestamp) tuples for TTL expiration
                        current_time = time.time()
                        if track_id is not None:
                            self.descriptions_cache[track_id] = (description, current_time)
                        else:
                            self.spatial_cache[spatial_key] = (description, current_time)

                        # Update metrics
                        vlm_descriptions_counter.labels(stream=stream_name).inc()
                        vlm_latency.labels(stream=stream_name).observe(vlm_elapsed_ms)

                        track_info = f"track {track_id}" if track_id else f"spatial {spatial_key}"
                        logger.info(f"[{stream_name}] VLM Description for {class_name} ({track_info}, {vlm_elapsed_ms:.0f}ms): {description}")

                        # Store for dashboard display
                        self._store_recent_description(
                            crop=crop,
                            class_name=class_name,
                            track_id=track_id,
                            stream_name=stream_name,
                            description=description,
                            latency_ms=vlm_elapsed_ms
                        )
                    else:
                        # No description returned (likely timeout)
                        self.consecutive_timeouts += 1
                        if self.consecutive_timeouts >= self.max_consecutive_timeouts:
                            logger.warning(f"VLM disabled after {self.consecutive_timeouts} consecutive failures")
                            self.vlm.available = False

                except Exception as e:
                    self.consecutive_timeouts += 1
                    if self.consecutive_timeouts >= self.max_consecutive_timeouts:
                        logger.warning(f"VLM disabled after {self.consecutive_timeouts} consecutive errors: {e}")
                        self.vlm.available = False
                    else:
                        logger.error(f"VLM worker error ({self.consecutive_timeouts}/{self.max_consecutive_timeouts}): {e}")

                # Update queue size metric
                vlm_queue_size.set(self.request_queue.qsize())

                # Periodically cleanup expired cache entries
                self._cleanup_expired_caches()

            except Exception:
                # Queue timeout - run periodic cleanup and try reconnection
                self._cleanup_expired_caches()
                self._try_reconnect()

    def _get_spatial_key(self, obj_meta) -> str:
        """Create a spatial key based on grid position (divide frame into 8x8 grid)"""
        # Use 100px grid cells for spatial bucketing
        grid_x = int(obj_meta.rect_params.left / 100)
        grid_y = int(obj_meta.rect_params.top / 100)
        return f"{obj_meta.class_id}_{grid_x}_{grid_y}"

    def _is_cached_spatially(self, obj_meta) -> bool:
        """Check if this detection region was recently described"""
        spatial_key = self._get_spatial_key(obj_meta)
        if spatial_key in self.spatial_cache:
            _, timestamp = self.spatial_cache[spatial_key]
            if time.time() - timestamp < self.spatial_cache_ttl:
                return True
            else:
                # Expired, remove from cache
                del self.spatial_cache[spatial_key]
        return False

    def _is_track_cached(self, track_id: int) -> bool:
        """Check if this track_id was recently described (with TTL)"""
        if track_id in self.descriptions_cache:
            _, timestamp = self.descriptions_cache[track_id]
            if time.time() - timestamp < self.descriptions_cache_ttl:
                return True
            else:
                # Expired, remove from cache
                del self.descriptions_cache[track_id]
        return False

    def _cleanup_expired_caches(self):
        """Remove expired entries from both caches (called periodically)"""
        current_time = time.time()

        # Only cleanup if enough time has passed
        if current_time - self.last_cache_cleanup < self.cache_cleanup_interval:
            return

        self.last_cache_cleanup = current_time

        # Cleanup descriptions_cache
        expired_tracks = [
            track_id for track_id, (_, timestamp) in self.descriptions_cache.items()
            if current_time - timestamp >= self.descriptions_cache_ttl
        ]
        for track_id in expired_tracks:
            del self.descriptions_cache[track_id]

        # Cleanup spatial_cache
        expired_spatial = [
            key for key, (_, timestamp) in self.spatial_cache.items()
            if current_time - timestamp >= self.spatial_cache_ttl
        ]
        for key in expired_spatial:
            del self.spatial_cache[key]

        if expired_tracks or expired_spatial:
            logger.debug(f"Cache cleanup: removed {len(expired_tracks)} tracks, {len(expired_spatial)} spatial entries")

    def _try_reconnect(self):
        """Try to reconnect to VLM service if it was disabled"""
        if not self.vlm:
            return

        # Only attempt reconnection if VLM is currently unavailable
        if self.vlm.available:
            return

        # Rate limit reconnection attempts
        current_time = time.time()
        if current_time - self.last_reconnect_attempt < self.reconnect_interval:
            return

        self.last_reconnect_attempt = current_time
        logger.debug("Attempting to reconnect to VLM service...")

        # Try to check availability
        self.vlm._check_availability()

        if self.vlm.available:
            # Successfully reconnected!
            self.consecutive_timeouts = 0
            logger.info("✅ VLM service reconnected - descriptions re-enabled")

    def _store_recent_description(self, crop: np.ndarray, class_name: str, track_id: Optional[int],
                                   stream_name: str, description: str, latency_ms: float):
        """Store a VLM description with its image for dashboard display"""
        import base64
        from PIL import Image

        try:
            # Convert crop to JPEG base64
            # crop is BGR format from OpenCV, convert to RGB for PIL
            if len(crop.shape) == 3 and crop.shape[2] == 3:
                crop_rgb = crop[:, :, ::-1]
            else:
                crop_rgb = crop

            pil_image = Image.fromarray(crop_rgb.astype('uint8'))

            # Resize if too large (max 400px on longest side for dashboard)
            max_size = 400
            if max(pil_image.size) > max_size:
                ratio = max_size / max(pil_image.size)
                new_size = (int(pil_image.size[0] * ratio), int(pil_image.size[1] * ratio))
                pil_image = pil_image.resize(new_size, Image.LANCZOS)

            # Encode as JPEG base64
            buffer = io.BytesIO()
            pil_image.save(buffer, format='JPEG', quality=85)
            image_b64 = base64.b64encode(buffer.getvalue()).decode('utf-8')

            # Create description entry
            entry = {
                'timestamp': datetime.utcnow().isoformat() + 'Z',
                'class_name': class_name,
                'track_id': track_id,
                'stream_name': stream_name,
                'description': description,
                'latency_ms': round(latency_ms, 1),
                'image_b64': image_b64
            }

            # Add to recent descriptions (thread-safe)
            with self.recent_descriptions_lock:
                self.recent_descriptions.insert(0, entry)
                # Keep only the most recent entries
                if len(self.recent_descriptions) > self.max_recent_descriptions:
                    self.recent_descriptions = self.recent_descriptions[:self.max_recent_descriptions]

        except Exception as e:
            logger.warning(f"Error storing VLM description for dashboard: {e}")

    def get_recent_descriptions(self) -> List[Dict]:
        """Get recent VLM descriptions for dashboard API"""
        with self.recent_descriptions_lock:
            return list(self.recent_descriptions)

    def try_claim_for_description(self, obj_meta, track_id: Optional[int] = None) -> bool:
        """
        Atomically check if detection should be described AND claim it.

        This prevents race conditions where two frames could both pass the check
        before either marks the detection as pending.

        Returns True if claimed (caller should queue VLM request), False otherwise.
        """
        if not self.vlm or not self.vlm.available:
            return False

        # Low confidence - check early to avoid cache operations
        if obj_meta.confidence < self.confidence_threshold:
            return False

        # Only describe certain classes
        if obj_meta.class_id not in self.interesting_classes:
            return False

        # Rate limiting
        current_time = time.time()
        if current_time - self.last_vlm_call < self.min_interval:
            return False

        # ATOMIC CHECK AND CLAIM
        # Check cache and mark as pending in one operation to prevent race conditions
        spatial_key = self._get_spatial_key(obj_meta)

        if track_id is not None:
            # Check if already cached or pending
            if self._is_track_cached(track_id):
                return False
            # Claim it by marking as pending immediately
            self.descriptions_cache[track_id] = ("[pending]", current_time)
        else:
            # Check spatial cache
            if self._is_cached_spatially(obj_meta):
                return False
            # Claim it by marking as pending immediately
            self.spatial_cache[spatial_key] = ("[pending]", current_time)

        # Update rate limit timestamp
        self.last_vlm_call = current_time

        return True

    def release_claim(self, track_id: Optional[int], spatial_key: str):
        """Release a claim if queueing fails (e.g., queue full)"""
        if track_id is not None:
            if track_id in self.descriptions_cache:
                del self.descriptions_cache[track_id]
        else:
            if spatial_key in self.spatial_cache:
                del self.spatial_cache[spatial_key]

    def describe_detection_async(self, frame_data: np.ndarray, obj_meta, track_id: Optional[int], stream_name: str) -> bool:
        """
        Queue a detection for async VLM description (non-blocking).

        IMPORTANT: Caller must call try_claim_for_description() first to atomically
        check and claim the detection. This method assumes the claim is already held.
        """
        if not self.vlm or not self.vlm.available:
            return False

        spatial_key = self._get_spatial_key(obj_meta)

        try:
            # Extract crop coordinates
            x1 = int(obj_meta.rect_params.left)
            y1 = int(obj_meta.rect_params.top)
            x2 = x1 + int(obj_meta.rect_params.width)
            y2 = y1 + int(obj_meta.rect_params.height)

            # Add padding (10% on each side)
            padding = 0.1
            width = x2 - x1
            height = y2 - y1
            x1 = max(0, int(x1 - width * padding))
            y1 = max(0, int(y1 - height * padding))
            x2 = min(frame_data.shape[1], int(x2 + width * padding))
            y2 = min(frame_data.shape[0], int(y2 + height * padding))

            # Crop detection - make a copy since frame_data may be reused
            crop = frame_data[y1:y2, x1:x2].copy()

            # Get appropriate prompt for class (or use custom prompt if set)
            class_name = obj_meta.obj_label if obj_meta.obj_label else 'object'
            prompt = self.custom_prompt if self.custom_prompt else get_prompt_for_class(class_name)

            # Queue the request (non-blocking)
            # Note: Pending marker already set by try_claim_for_description()
            try:
                self.request_queue.put_nowait((crop, class_name, prompt, spatial_key, track_id, stream_name))
                vlm_queue_size.set(self.request_queue.qsize())
                logger.debug(f"Queued VLM request for {class_name} (track {track_id})")
                return True
            except Exception:
                # Queue full - release the claim and skip
                self.release_claim(track_id, spatial_key)
                logger.debug(f"VLM queue full, skipping {class_name}")
                return False

        except Exception as e:
            # Error during processing - release the claim
            self.release_claim(track_id, spatial_key)
            logger.error(f"Error queuing VLM description: {e}")
            return False


class PerformanceTracker:
    """Tracks performance metrics for each stream"""

    def __init__(self):
        self.frame_times = defaultdict(list)
        self.last_update = defaultdict(float)
        self.fps_window = 30  # Calculate FPS over 30 frames

    def update_frame(self, stream_id: str, timestamp: float):
        """Update frame timing for FPS calculation"""
        self.frame_times[stream_id].append(timestamp)

        # Keep only last N frames
        if len(self.frame_times[stream_id]) > self.fps_window:
            self.frame_times[stream_id].pop(0)

        # Update FPS every second
        current_time = time.time()
        if current_time - self.last_update[stream_id] >= 1.0:
            fps = self.calculate_fps(stream_id)
            if fps > 0:
                fps_gauge.labels(stream=stream_id).set(fps)
            self.last_update[stream_id] = current_time

    def calculate_fps(self, stream_id: str) -> float:
        """Calculate current FPS for a stream"""
        times = self.frame_times[stream_id]
        if len(times) < 2:
            return 0.0

        time_diff = times[-1] - times[0]
        if time_diff == 0:
            return 0.0

        return (len(times) - 1) / time_diff


class MetricsServer:
    """Flask server for Prometheus metrics, MJPEG streaming, and VLM API"""

    def __init__(self, port=9090, vlm_integration=None):
        self.app = Flask(__name__)
        self.port = port
        self.vlm_integration = vlm_integration

        @self.app.after_request
        def add_cors_headers(response):
            response.headers['Access-Control-Allow-Origin'] = '*'
            response.headers['Access-Control-Allow-Methods'] = 'GET, POST, OPTIONS'
            response.headers['Access-Control-Allow-Headers'] = 'Content-Type'
            return response

        @self.app.route('/metrics')
        def metrics():
            return Response(generate_latest(REGISTRY), mimetype='text/plain')

        @self.app.route('/health')
        def health():
            return {'status': 'healthy', 'timestamp': datetime.utcnow().isoformat()}

        @self.app.route('/api/vlm_descriptions')
        def vlm_descriptions():
            """API endpoint for VLM descriptions with images"""
            if self.vlm_integration is None:
                return {'error': 'VLM not configured', 'descriptions': []}, 503

            descriptions = self.vlm_integration.get_recent_descriptions()
            return {
                'count': len(descriptions),
                'descriptions': descriptions
            }

        @self.app.route('/api/vlm_status')
        def vlm_status():
            """API endpoint for VLM service status"""
            if self.vlm_integration is None:
                return {'available': False, 'reason': 'VLM not configured'}

            vlm = self.vlm_integration.vlm
            if vlm is None:
                return {'available': False, 'reason': 'VLM client not initialized'}

            return {
                'available': vlm.available,
                'service_url': vlm.service_url,
                'queue_size': self.vlm_integration.request_queue.qsize(),
                'descriptions_count': len(self.vlm_integration.recent_descriptions),
                'consecutive_timeouts': self.vlm_integration.consecutive_timeouts
            }

        @self.app.route('/api/vlm_prompt', methods=['GET'])
        def get_vlm_prompt():
            """Get the current VLM prompt"""
            if self.vlm_integration is None:
                return {'error': 'VLM not configured'}, 503

            return {
                'custom_prompt': self.vlm_integration.custom_prompt,
                'using_custom': self.vlm_integration.custom_prompt is not None
            }

        @self.app.route('/api/vlm_prompt', methods=['POST'])
        def set_vlm_prompt():
            """Set a custom VLM prompt (or clear it)"""
            if self.vlm_integration is None:
                return {'error': 'VLM not configured'}, 503

            data = request.get_json() or {}
            prompt = data.get('prompt')

            # Empty string or None clears the custom prompt
            if not prompt or prompt.strip() == '':
                self.vlm_integration.custom_prompt = None
                logger.info("VLM custom prompt cleared - using class-specific prompts")
                return {'success': True, 'custom_prompt': None, 'using_custom': False}
            else:
                self.vlm_integration.custom_prompt = prompt.strip()
                logger.info(f"VLM custom prompt set: {self.vlm_integration.custom_prompt[:50]}...")
                return {'success': True, 'custom_prompt': self.vlm_integration.custom_prompt, 'using_custom': True}

        @self.app.route('/stream')
        def stream():
            """MJPEG stream endpoint with bounding boxes"""
            global mjpeg_client_count

            def generate():
                global mjpeg_client_count
                # Track client connection
                with mjpeg_client_lock:
                    mjpeg_client_count += 1
                    logger.info(f"MJPEG client connected (total: {mjpeg_client_count})")

                try:
                    while True:
                        try:
                            # Get latest frame from queue (non-blocking with timeout)
                            frame = latest_frame_queue.get(timeout=1.0)

                            # Encode frame as JPEG
                            ret, jpeg = cv2.imencode('.jpg', frame, [cv2.IMWRITE_JPEG_QUALITY, 80])
                            if not ret:
                                continue

                            # Yield frame in MJPEG format
                            yield (b'--frame\r\n'
                                   b'Content-Type: image/jpeg\r\n\r\n' +
                                   jpeg.tobytes() + b'\r\n')

                        except Exception as e:
                            # Queue empty or other error - send a placeholder or continue
                            logger.debug(f"Stream frame error: {e}")
                            time.sleep(0.1)
                            continue
                finally:
                    # Track client disconnection
                    with mjpeg_client_lock:
                        mjpeg_client_count -= 1
                        logger.info(f"MJPEG client disconnected (total: {mjpeg_client_count})")

            return Response(generate(),
                          mimetype='multipart/x-mixed-replace; boundary=frame')

    def start(self):
        """Start metrics server in background thread"""
        thread = threading.Thread(target=self._run, daemon=True)
        thread.start()
        logger.info(f"Metrics server started on port {self.port}")
        logger.info(f"MJPEG stream available at http://0.0.0.0:{self.port}/stream")

    def _run(self):
        self.app.run(host='0.0.0.0', port=self.port, debug=False, threaded=True)


def get_frame_data_from_buffer(gst_buffer, frame_meta):
    """
    Extract frame data as numpy array from GStreamer buffer

    Uses DeepStream's pyds.get_nvds_buf_surface() for efficient GPU->CPU transfer

    CRITICAL FOR JETSON: On Jetson platforms, you MUST call pyds.unmap_nvds_buf_surface()
    after get_nvds_buf_surface() to release the GPU memory mapping. Failure to do this
    causes severe memory leaks (memory grows to 100%+ within minutes).

    See: https://forums.developer.nvidia.com/t/unmapping-buffer-surface-in-python-bindings-of-ds6-0-1/277493
    """
    frame_copy = None
    np_frame = None
    bgr_frame = None
    buffer_hash = hash(gst_buffer)
    batch_id = frame_meta.batch_id

    try:
        # Get buffer surface from GPU memory
        # This creates a CPU-accessible mapping of the GPU buffer
        frame_copy = pyds.get_nvds_buf_surface(buffer_hash, batch_id)

        if frame_copy is None:
            return None

        # Convert to numpy array immediately and make a deep copy
        # CRITICAL: We must copy the data BEFORE unmapping the buffer
        np_frame = np.array(frame_copy, copy=True)

        # CRITICAL FOR JETSON: Unmap the buffer surface to release GPU memory
        # This MUST be called after get_nvds_buf_surface on Jetson platforms
        # The frame_copy array is invalid after this call
        pyds.unmap_nvds_buf_surface(buffer_hash, batch_id)
        frame_copy = None  # Mark as invalid

        # Calculate actual dimensions from buffer size
        # RGBA has 4 channels, so total_pixels = buffer_size / 4
        buffer_size = np_frame.size
        total_pixels = buffer_size // 4

        # Common resolutions for RTSP streams: 1920x1080, 1280x720, 640x360
        # Try to match the buffer size to a known resolution
        if total_pixels == 1920 * 1080:
            frame_height, frame_width = 1080, 1920
        elif total_pixels == 1280 * 720:
            frame_height, frame_width = 720, 1280
        elif total_pixels == 640 * 360:
            frame_height, frame_width = 360, 640
        else:
            # Fallback: try using frame_meta dimensions
            frame_height = frame_meta.source_frame_height
            frame_width = frame_meta.source_frame_width

        # Reshape to RGBA format
        np_frame = np_frame.reshape((frame_height, frame_width, 4))

        # Convert RGBA to BGR (OpenCV format)
        bgr_frame = cv2.cvtColor(np_frame, cv2.COLOR_RGBA2BGR)

        return bgr_frame

    except Exception as e:
        logger.warning(f"Error extracting frame data: {e}")
        # Try to unmap even on error to prevent memory leak
        try:
            if frame_copy is not None:
                pyds.unmap_nvds_buf_surface(buffer_hash, batch_id)
        except Exception:
            pass  # Unmap failed, but nothing we can do
        return None


def osd_sink_pad_buffer_probe(pad, info, user_data):
    """
    Probe callback on OSD sink pad to extract performance metadata and integrate VLM
    """

    perf_tracker = user_data['perf_tracker']
    stream_names = user_data['stream_names']
    vlm_integration = user_data.get('vlm_integration')

    gst_buffer = info.get_buffer()
    if not gst_buffer:
        return Gst.PadProbeReturn.OK

    batch_meta = pyds.gst_buffer_get_nvds_batch_meta(hash(gst_buffer))
    l_frame = batch_meta.frame_meta_list

    while l_frame is not None:
        try:
            frame_meta = pyds.NvDsFrameMeta.cast(l_frame.data)
        except StopIteration:
            break

        # Get stream name
        stream_id = frame_meta.source_id
        stream_name = stream_names.get(stream_id, f"stream_{stream_id}")

        # Update frame counter
        frames_processed.labels(stream=stream_name).inc()

        # Track FPS
        current_time = time.time()
        perf_tracker.update_frame(stream_name, current_time)

        # Only extract frames when there's actual demand:
        # 1. MJPEG clients are connected, OR
        # 2. VLM is enabled and queue has room
        # This avoids unnecessary GPU->CPU copies that consume memory
        frame_data = None
        should_extract = False

        with mjpeg_client_lock:
            if mjpeg_client_count > 0:
                should_extract = True

        if vlm_integration and vlm_integration.vlm and vlm_integration.vlm.available:
            if not vlm_integration.request_queue.full():
                should_extract = True

        if should_extract:
            frame_data = get_frame_data_from_buffer(gst_buffer, frame_meta)

        # Periodic garbage collection to help release any lingering buffers
        # Run every 500 frames to avoid overhead
        frame_num_raw = int(frames_processed.labels(stream=stream_name)._value.get())
        if frame_num_raw % 500 == 0 and frame_num_raw > 0:
            gc.collect()

        # Extract detections
        l_obj = frame_meta.obj_meta_list
        detection_count = defaultdict(int)
        detections_for_drawing = []  # Store detections to draw on frame later

        while l_obj is not None:
            try:
                obj_meta = pyds.NvDsObjectMeta.cast(l_obj.data)
            except StopIteration:
                break

            # Count detections by class
            class_name = obj_meta.obj_label if obj_meta.obj_label else f"class_{obj_meta.class_id}"
            detection_count[class_name] += 1

            # Store detection for drawing on frame
            detections_for_drawing.append({
                'x': int(obj_meta.rect_params.left),
                'y': int(obj_meta.rect_params.top),
                'width': int(obj_meta.rect_params.width),
                'height': int(obj_meta.rect_params.height),
                'class': class_name,
                'confidence': obj_meta.confidence
            })

            # VLM Integration: Describe high-confidence detections (async - non-blocking)
            if vlm_integration and frame_data is not None:
                # Get tracking ID (if available - None if tracker disabled)
                track_id = obj_meta.object_id if hasattr(obj_meta, 'object_id') and obj_meta.object_id > 0 else None

                # Atomically check and claim detection for VLM description
                # This prevents race conditions between frames
                if vlm_integration.try_claim_for_description(obj_meta, track_id):
                    # Claim successful - queue VLM description (async - doesn't block pipeline)
                    vlm_integration.describe_detection_async(
                        frame_data, obj_meta, track_id, stream_name
                    )

            try:
                l_obj = l_obj.next
            except StopIteration:
                break

        # Update detection counters (log every 100 frames to reduce memory churn)
        total_detections = sum(detection_count.values())
        if frame_num_raw % 100 == 0:
            logger.info(f"Frame {frame_num_raw}: {total_detections} detections - {dict(detection_count)}")

        for class_name, count in detection_count.items():
            detections_counter.labels(stream=stream_name, class_=class_name).inc(count)

        # Draw bounding boxes on frame for MJPEG streaming
        if frame_data is not None and len(detections_for_drawing) > 0:
            try:
                # Make a copy to avoid modifying the original
                display_frame = frame_data.copy()

                for det in detections_for_drawing:
                    # Draw bounding box
                    x, y, w, h = det['x'], det['y'], det['width'], det['height']
                    cv2.rectangle(display_frame, (x, y), (x + w, y + h), (0, 255, 0), 2)

                    # Draw label with class name and confidence
                    label = f"{det['class']}: {det['confidence']:.2f}"
                    label_size, baseline = cv2.getTextSize(label, cv2.FONT_HERSHEY_SIMPLEX, 0.5, 1)

                    # Draw background for label
                    cv2.rectangle(display_frame,
                                (x, y - label_size[1] - baseline),
                                (x + label_size[0], y),
                                (0, 255, 0), -1)

                    # Draw label text
                    cv2.putText(display_frame, label, (x, y - baseline),
                               cv2.FONT_HERSHEY_SIMPLEX, 0.5, (0, 0, 0), 1)

                # Update queue with latest frame (non-blocking)
                # If queue is full, remove old frame and add new one
                if latest_frame_queue.full():
                    try:
                        latest_frame_queue.get_nowait()
                    except Exception:
                        pass  # Queue became empty between check and get
                latest_frame_queue.put_nowait(display_frame)

            except Exception as e:
                logger.warning(f"Error drawing bounding boxes: {e}")
        elif frame_data is not None:
            # No detections, but still update the frame for streaming
            try:
                if latest_frame_queue.full():
                    try:
                        latest_frame_queue.get_nowait()
                    except Exception:
                        pass  # Queue became empty between check and get
                latest_frame_queue.put_nowait(frame_data.copy())
            except Exception as e:
                logger.warning(f"Error updating frame queue: {e}")

        # CRITICAL: Clean up frame data to prevent memory leak
        # Explicitly delete references to allow garbage collection
        if frame_data is not None:
            del frame_data
            frame_data = None

        try:
            l_frame = l_frame.next
        except StopIteration:
            break

    return Gst.PadProbeReturn.OK


def bus_call(bus, message, loop):
    """Handle GStreamer bus messages"""
    t = message.type

    if t == Gst.MessageType.EOS:
        logger.info("End-of-stream")
        loop.quit()
    elif t == Gst.MessageType.WARNING:
        err, debug = message.parse_warning()
        logger.warning(f"Warning: {err}: {debug}")
    elif t == Gst.MessageType.ERROR:
        err, debug = message.parse_error()
        logger.error(f"Error: {err}: {debug}")
        loop.quit()

    return True


def main():
    """Main DeepStream pipeline with VLM integration and performance instrumentation"""

    # Initialize GStreamer
    Gst.init(None)

    # Load configuration
    streams_file = '/app/streams.json'
    config_file = '/app/nvinfer_config.txt'

    with open(streams_file, 'r') as f:
        streams_config = json.load(f)

    # Filter enabled streams
    enabled_streams = [s for s in streams_config['streams'] if s.get('enabled', True)]
    logger.info(f"Starting DeepStream detector with {len(enabled_streams)} streams")

    # Create stream name mapping
    stream_names = {i: s['name'] for i, s in enumerate(enabled_streams)}

    # Initialize performance tracker
    perf_tracker = PerformanceTracker()

    # Initialize VLM integration if available
    vlm_integration = None
    if VLM_AVAILABLE:
        try:
            # VLM configuration from environment variables
            vlm_url = os.environ.get('VLM_SERVICE_URL', 'http://10.42.0.1:8090')
            vlm_timeout = float(os.environ.get('VLM_TIMEOUT', '60.0'))
            vlm_client = VLMClient(service_url=vlm_url, timeout=vlm_timeout)
            vlm_integration = VLMIntegration(vlm_client)

            if vlm_client.available:
                logger.info("✅ VLM service connected - descriptions enabled")
                stats = vlm_client.get_stats()
                if stats:
                    logger.info(f"   VLM GPU Memory: {stats.get('memory_allocated_gb', 0):.2f}GB")
            else:
                logger.warning("⚠️  VLM service not available - running without descriptions")
                logger.info("   To enable VLM: cd vlm && wendy run --device <device>")

        except Exception as e:
            logger.warning(f"Could not initialize VLM: {e}")
            vlm_integration = None
    else:
        logger.info("VLM client not installed - running without descriptions")

    # Start metrics server (with VLM integration for API endpoints)
    metrics_server = MetricsServer(port=9090, vlm_integration=vlm_integration)
    metrics_server.start()
    logger.info("VLM descriptions API available at http://0.0.0.0:9090/api/vlm_descriptions")

    # Create pipeline
    logger.info("Creating DeepStream pipeline...")
    pipeline = Gst.Pipeline()
    if not pipeline:
        logger.error("Failed to create pipeline")
        return

    # Create elements
    streammux = Gst.ElementFactory.make("nvstreammux", "stream-muxer")
    pgie = Gst.ElementFactory.make("nvinfer", "primary-inference")
    tracker = Gst.ElementFactory.make("nvtracker", "tracker")  # NvDCF tracker for object tracking
    nvvidconv = Gst.ElementFactory.make("nvvideoconvert", "convertor")
    nvosd = Gst.ElementFactory.make("nvdsosd", "onscreendisplay")
    sink = Gst.ElementFactory.make("fakesink", "fake-sink")

    if not all([streammux, pgie, tracker, nvvidconv, nvosd, sink]):
        logger.error("Failed to create pipeline elements")
        return

    # Configure main nvvideoconvert (workaround for JetPack 6.2 bug)
    # See: https://forums.developer.nvidia.com/t/nvbufsurftransform-copy-cpp-failed-in-mem-copy/334720
    nvvidconv.set_property('copy-hw', 2)

    # Configure streammux (GPU-based batching)
    streammux.set_property('width', 1920)
    streammux.set_property('height', 1080)
    streammux.set_property('batch-size', len(enabled_streams))
    streammux.set_property('batched-push-timeout', 40000)
    streammux.set_property('live-source', True)

    # Configure inference engine (TensorRT)
    pgie.set_property('config-file-path', config_file)

    # Configure tracker (NvDCF for object tracking)
    # VPI library is now mounted from host via CDI
    tracker_config = '/app/tracker_config.yml'
    tracker.set_property('ll-lib-file', '/opt/nvidia/deepstream/deepstream-7.1/lib/libnvds_nvmultiobjecttracker.so')
    tracker.set_property('ll-config-file', tracker_config)
    tracker.set_property('tracker-width', 640)
    tracker.set_property('tracker-height', 384)
    tracker.set_property('display-tracking-id', 1)
    logger.info(f"Tracker configured with config: {tracker_config}")

    # Configure sink
    sink.set_property('sync', False)
    sink.set_property('enable-last-sample', False)

    # Add elements to pipeline
    pipeline.add(streammux)
    pipeline.add(pgie)
    pipeline.add(tracker)
    pipeline.add(nvvidconv)
    pipeline.add(nvosd)
    pipeline.add(sink)

    # Link elements: streammux -> pgie -> tracker -> nvvidconv -> nvosd -> sink
    logger.info("Linking pipeline elements...")
    if not streammux.link(pgie):
        logger.error("Failed to link streammux to pgie")
        return
    if not pgie.link(tracker):
        logger.error("Failed to link pgie to tracker")
        return
    if not tracker.link(nvvidconv):
        logger.error("Failed to link tracker to nvvidconv")
        return
    if not nvvidconv.link(nvosd):
        logger.error("Failed to link nvvidconv to nvosd")
        return
    if not nvosd.link(sink):
        logger.error("Failed to link nvosd to sink")
        return

    # Add sources
    logger.info("Adding video sources...")
    for i, stream in enumerate(enabled_streams):
        source = Gst.ElementFactory.make("uridecodebin", f"source-{i}")
        if not source:
            logger.error(f"Failed to create source for stream {i}")
            continue

        source.set_property('uri', stream['url'])

        # Create converter bin for this source
        nvvidconv = Gst.ElementFactory.make("nvvideoconvert", f"nvvidconv-{i}")
        # Workaround for JetPack 6.2 bug: set copy-hw=2 to avoid buffer copy failures
        # See: https://forums.developer.nvidia.com/t/nvbufsurftransform-copy-cpp-failed-in-mem-copy/334720
        nvvidconv.set_property('copy-hw', 2)

        caps_filter = Gst.ElementFactory.make("capsfilter", f"filter-{i}")
        caps = Gst.Caps.from_string("video/x-raw(memory:NVMM), format=RGBA")
        caps_filter.set_property("caps", caps)

        pipeline.add(source)
        pipeline.add(nvvidconv)
        pipeline.add(caps_filter)

        # Link converter to capsfilter
        if not nvvidconv.link(caps_filter):
            logger.error(f"Failed to link nvvidconv to capsfilter for stream {i}")
            continue

        # Link capsfilter to streammux
        sinkpad = streammux.request_pad_simple(f"sink_{i}")
        capsfilter_srcpad = caps_filter.get_static_pad("src")
        if capsfilter_srcpad.link(sinkpad) != Gst.PadLinkReturn.OK:
            logger.error(f"Failed to link capsfilter to streammux for stream {i}")
            continue

        # Connect to converter when pad is available
        def pad_added_handler(src, pad, stream_idx=i, converter=nvvidconv):
            # Only link video pads
            caps = pad.query_caps(None)
            if not caps or caps.is_empty():
                return

            struct = caps.get_structure(0)
            if struct and struct.get_name().startswith("video/"):
                conv_sinkpad = converter.get_static_pad("sink")
                if conv_sinkpad and not conv_sinkpad.is_linked():
                    ret = pad.link(conv_sinkpad)
                    if ret == Gst.PadLinkReturn.OK:
                        logger.info(f"✓ Linked video source {stream_idx} ({stream['name']}) to converter")
                    else:
                        logger.error(f"✗ Failed to link source {stream_idx} to converter: {ret}")

        source.connect("pad-added", pad_added_handler)

        # Configure RTSP source when uridecodebin creates it
        def source_setup_handler(uridecodebin, source_element, stream_name=stream['name']):
            source_name = source_element.get_factory().get_name()
            if source_name == "rtspsrc":
                # Force TCP transport (more reliable than UDP)
                # protocols: 0x4 = TCP only, 0x7 = UDP+TCP+HTTP
                source_element.set_property('protocols', 0x4)
                # Buffer 500ms of data for network jitter
                source_element.set_property('latency', 500)
                # Timeout for TCP connection (microseconds)
                source_element.set_property('timeout', 10000000)  # 10 seconds
                # Retry on disconnect
                source_element.set_property('retry', 5)
                logger.info(f"Configured RTSP source for {stream_name}: TCP transport, 500ms latency")

        source.connect("source-setup", source_setup_handler)
        logger.info(f"Added stream: {stream['name']} ({stream['url']})")

    # Add probe to OSD sink pad for performance monitoring
    osd_sink_pad = nvosd.get_static_pad("sink")
    if not osd_sink_pad:
        logger.error("Unable to get sink pad of nvosd")
        return

    user_data = {
        'perf_tracker': perf_tracker,
        'stream_names': stream_names,
        'vlm_integration': vlm_integration
    }

    osd_sink_pad.add_probe(
        Gst.PadProbeType.BUFFER,
        osd_sink_pad_buffer_probe,
        user_data
    )

    # Create event loop and bus watcher
    loop = GLib.MainLoop()
    bus = pipeline.get_bus()
    bus.add_signal_watch()
    bus.connect("message", bus_call, loop)

    # Start pipeline
    logger.info("Starting pipeline...")
    logger.info("Metrics available at http://0.0.0.0:9090/metrics")
    ret = pipeline.set_state(Gst.State.PLAYING)

    if ret == Gst.StateChangeReturn.FAILURE:
        logger.error("Unable to set pipeline to PLAYING state")
        return

    # Run
    try:
        loop.run()
    except KeyboardInterrupt:
        logger.info("Interrupted by user")
    except Exception as e:
        logger.error(f"Error in main loop: {e}")

    # Cleanup
    logger.info("Stopping pipeline...")

    # Stop VLM worker gracefully before pipeline shutdown
    if vlm_integration:
        vlm_integration.stop()

    pipeline.set_state(Gst.State.NULL)
    logger.info("Pipeline stopped")


if __name__ == "__main__":
    main()
