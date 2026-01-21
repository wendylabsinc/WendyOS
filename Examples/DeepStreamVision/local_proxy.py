#!/usr/bin/env python3
"""
Local proxy server for DeepStream Vision dashboard

Runs on your laptop, proxies requests to the Jetson device.
This eliminates CORS issues when monitor.html is opened locally.

Usage:
    python3 local_proxy.py [device-hostname-or-ip]

Example:
    python3 local_proxy.py wendyos-tender-oar.local
    python3 local_proxy.py 10.42.0.1

Then open monitor.html in your browser.
"""

import sys
import logging
import requests
from flask import Flask, jsonify, Response, request

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Add CORS headers manually
@app.after_request
def add_cors_headers(response):
    response.headers['Access-Control-Allow-Origin'] = '*'
    response.headers['Access-Control-Allow-Methods'] = 'GET, POST, OPTIONS'
    response.headers['Access-Control-Allow-Headers'] = 'Content-Type'
    return response

# Default device - can be overridden via command line
DEVICE = "wendyos-tender-oar.local"

# Service endpoints on the device
DETECTOR_METRICS_URL = f"http://{DEVICE}:9090/metrics"
DETECTOR_HEALTH_URL = f"http://{DEVICE}:9090/health"
VLM_HEALTH_URL = f"http://{DEVICE}:8090/health"
VLM_STATS_URL = f"http://{DEVICE}:8090/stats"
GPU_METRICS_URL = f"http://{DEVICE}:9091/metrics"
GPU_HEALTH_URL = f"http://{DEVICE}:9091/health"


def update_device(device_hostname):
    """Update device hostname and regenerate URLs"""
    global DEVICE, DETECTOR_METRICS_URL, DETECTOR_HEALTH_URL, VLM_HEALTH_URL, VLM_STATS_URL, GPU_METRICS_URL, GPU_HEALTH_URL
    DEVICE = device_hostname
    DETECTOR_METRICS_URL = f"http://{DEVICE}:9090/metrics"
    DETECTOR_HEALTH_URL = f"http://{DEVICE}:9090/health"
    VLM_HEALTH_URL = f"http://{DEVICE}:8090/health"
    VLM_STATS_URL = f"http://{DEVICE}:8090/stats"
    GPU_METRICS_URL = f"http://{DEVICE}:9091/metrics"
    GPU_HEALTH_URL = f"http://{DEVICE}:9091/health"
    logger.info(f"Device updated to: {DEVICE}")


def fetch_with_timeout(url, timeout=2):
    """Fetch URL with timeout and error handling"""
    try:
        response = requests.get(url, timeout=timeout)
        response.raise_for_status()
        return response
    except Exception as e:
        logger.warning(f"Failed to fetch {url}: {e}")
        return None


@app.route('/health')
def health():
    """Proxy server health check"""
    return jsonify({
        'status': 'healthy',
        'service': 'local-proxy',
        'device': DEVICE
    })


@app.route('/api/detector/metrics')
def proxy_detector_metrics():
    """Proxy to detector metrics endpoint"""
    response = fetch_with_timeout(DETECTOR_METRICS_URL)
    if response:
        return Response(response.text, mimetype='text/plain')
    return Response("Service unavailable", status=503)


@app.route('/api/detector/health')
def proxy_detector_health():
    """Proxy to detector health endpoint"""
    response = fetch_with_timeout(DETECTOR_HEALTH_URL)
    if response:
        return jsonify(response.json())
    return jsonify({'status': 'unavailable'}), 503


@app.route('/api/vlm/health')
def proxy_vlm_health():
    """Proxy to VLM health endpoint"""
    response = fetch_with_timeout(VLM_HEALTH_URL)
    if response:
        return jsonify(response.json())
    return jsonify({'status': 'unavailable'}), 503


@app.route('/api/vlm/stats')
def proxy_vlm_stats():
    """Proxy to VLM stats endpoint"""
    response = fetch_with_timeout(VLM_STATS_URL)
    if response:
        return jsonify(response.json())
    return jsonify({'error': 'unavailable'}), 503


@app.route('/api/gpu/metrics')
def proxy_gpu_metrics():
    """Proxy to GPU metrics endpoint"""
    response = fetch_with_timeout(GPU_METRICS_URL)
    if response:
        return Response(response.text, mimetype='text/plain')
    return Response("Service unavailable", status=503)


@app.route('/api/gpu/health')
def proxy_gpu_health():
    """Proxy to GPU health endpoint"""
    response = fetch_with_timeout(GPU_HEALTH_URL)
    if response:
        return jsonify(response.json())
    return jsonify({'status': 'unavailable'}), 503


@app.route('/api/detector/stream')
def proxy_detector_stream():
    """Proxy to detector MJPEG stream endpoint"""
    import requests
    try:
        # Stream the MJPEG data directly from detector to client
        stream_url = f"http://{DEVICE}:9090/stream"

        def generate():
            try:
                with requests.get(stream_url, stream=True, timeout=5) as r:
                    for chunk in r.iter_content(chunk_size=1024):
                        if chunk:
                            yield chunk
            except Exception as e:
                logger.warning(f"Stream error: {e}")

        return Response(generate(), mimetype='multipart/x-mixed-replace; boundary=frame')
    except Exception as e:
        logger.error(f"Failed to proxy stream: {e}")
        return Response("Stream unavailable", status=503)


@app.route('/api/detector/vlm_descriptions')
def proxy_vlm_descriptions():
    """Proxy to detector VLM descriptions API endpoint"""
    vlm_url = f"http://{DEVICE}:9090/api/vlm_descriptions"
    response = fetch_with_timeout(vlm_url, timeout=5)
    if response:
        return jsonify(response.json())
    return jsonify({'error': 'unavailable', 'descriptions': []}), 503


@app.route('/api/detector/vlm_status')
def proxy_vlm_status():
    """Proxy to detector VLM status API endpoint"""
    vlm_url = f"http://{DEVICE}:9090/api/vlm_status"
    response = fetch_with_timeout(vlm_url, timeout=5)
    if response:
        return jsonify(response.json())
    return jsonify({'available': False, 'reason': 'unavailable'}), 503


@app.route('/api/detector/vlm_prompt', methods=['GET'])
def proxy_get_vlm_prompt():
    """Proxy to get current VLM prompt"""
    url = f"http://{DEVICE}:9090/api/vlm_prompt"
    response = fetch_with_timeout(url, timeout=5)
    if response:
        return jsonify(response.json())
    return jsonify({'error': 'unavailable'}), 503


@app.route('/api/detector/vlm_prompt', methods=['POST'])
def proxy_set_vlm_prompt():
    """Proxy to set VLM prompt"""
    url = f"http://{DEVICE}:9090/api/vlm_prompt"
    try:
        response = requests.post(url, json=request.get_json(), timeout=5)
        if response.status_code == 200:
            return jsonify(response.json())
        return jsonify({'error': f'HTTP {response.status_code}'}), response.status_code
    except Exception as e:
        logger.warning(f"Failed to set VLM prompt: {e}")
        return jsonify({'error': str(e)}), 503


if __name__ == '__main__':
    # Allow device to be specified via command line
    if len(sys.argv) > 1:
        update_device(sys.argv[1])

    logger.info("="*60)
    logger.info("DeepStream Vision Local Proxy Server")
    logger.info("="*60)
    logger.info(f"Device: {DEVICE}")
    logger.info(f"Proxy server: http://localhost:8080")
    logger.info("")
    logger.info("Now open monitor.html in your browser")
    logger.info("="*60)

    app.run(host='localhost', port=8080, debug=False)
