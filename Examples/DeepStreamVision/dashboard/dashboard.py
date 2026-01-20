#!/usr/bin/env python3
"""
DeepStream Vision Metrics Dashboard with Backend Proxy

Acts as a proxy between browser and all services:
- DeepStream detector (:9090)
- VLM service (:8090)
- GPU stats exporter (:9091)

Dashboard available on :8080
"""

import logging
import time
import re
import requests
from flask import Flask, jsonify, render_template_string, Response

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Add CORS headers to allow browser access
@app.after_request
def add_cors_headers(response):
    response.headers['Access-Control-Allow-Origin'] = '*'
    response.headers['Access-Control-Allow-Methods'] = 'GET, POST, OPTIONS'
    response.headers['Access-Control-Allow-Headers'] = 'Content-Type'
    return response

# Service endpoints (use localhost - dashboard runs on same host as services)
DETECTOR_METRICS_URL = "http://127.0.0.1:9090/metrics"
DETECTOR_HEALTH_URL = "http://127.0.0.1:9090/health"
VLM_HEALTH_URL = "http://127.0.0.1:8090/health"
VLM_STATS_URL = "http://127.0.0.1:8090/stats"
GPU_METRICS_URL = "http://127.0.0.1:9091/metrics"
GPU_HEALTH_URL = "http://127.0.0.1:9091/health"


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
    """Dashboard health check"""
    return jsonify({'status': 'healthy', 'service': 'dashboard'})


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


def parse_prometheus_metrics(text):
    """Parse Prometheus text format into dict"""
    metrics = {}
    for line in text.split('\n'):
        line = line.strip()
        if not line or line.startswith('#'):
            continue

        try:
            match = re.match(r'([a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[^}]*\})?\s+)([-+]?[0-9]*\.?[0-9]+(?:[eE][-+]?[0-9]+)?)', line)
            if match:
                metric_key = match.group(1).strip()
                value = float(match.group(2))
                metrics[metric_key] = value
        except:
            pass

    return metrics


@app.route('/api/aggregated')
def api_aggregated():
    """Get all metrics aggregated as JSON"""
    detector_response = fetch_with_timeout(DETECTOR_METRICS_URL)
    gpu_response = fetch_with_timeout(GPU_METRICS_URL)
    vlm_health = fetch_with_timeout(VLM_HEALTH_URL)

    detector_metrics = parse_prometheus_metrics(detector_response.text) if detector_response else {}
    gpu_metrics = parse_prometheus_metrics(gpu_response.text) if gpu_response else {}
    vlm_status = vlm_health.json() if vlm_health else {'status': 'unavailable'}

    return jsonify({
        'detector': detector_metrics,
        'gpu': gpu_metrics,
        'vlm': vlm_status,
        'timestamp': time.time()
    })


@app.route('/')
def index():
    """Main dashboard with hostname configuration"""
    return render_template_string('''
<!DOCTYPE html>
<html>
<head>
    <title>DeepStream Vision Dashboard</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            background: linear-gradient(135deg, #0a0e27 0%, #1a1f3a 100%);
            color: #e2e8f0;
            min-height: 100vh;
            padding: 20px;
        }

        .container {
            max-width: 1600px;
            margin: 0 auto;
        }

        .header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 30px;
            padding: 20px;
            background: rgba(26, 31, 58, 0.6);
            border-radius: 12px;
            border: 1px solid rgba(100, 116, 139, 0.2);
        }

        h1 {
            font-size: 28px;
            font-weight: 600;
            color: #00d4ff;
            display: flex;
            align-items: center;
            gap: 12px;
        }

        .hostname-config {
            display: flex;
            align-items: center;
            gap: 10px;
        }

        .hostname-config label {
            color: #94a3b8;
            font-size: 14px;
            font-weight: 500;
        }

        .hostname-config input {
            padding: 8px 14px;
            border: 2px solid rgba(100, 116, 139, 0.3);
            border-radius: 6px;
            background: rgba(15, 23, 42, 0.6);
            color: #e2e8f0;
            font-size: 14px;
            width: 250px;
            transition: all 0.2s;
        }

        .hostname-config input:focus {
            outline: none;
            border-color: #00d4ff;
            background: rgba(15, 23, 42, 0.8);
        }

        .btn {
            padding: 8px 16px;
            border: none;
            border-radius: 6px;
            background: linear-gradient(135deg, #00d4ff 0%, #0099cc 100%);
            color: #fff;
            font-size: 14px;
            font-weight: 500;
            cursor: pointer;
            transition: all 0.2s;
        }

        .btn:hover {
            transform: translateY(-1px);
            box-shadow: 0 4px 12px rgba(0, 212, 255, 0.4);
        }

        .status-badge {
            display: inline-flex;
            align-items: center;
            gap: 6px;
            padding: 4px 12px;
            border-radius: 20px;
            font-size: 12px;
            font-weight: 500;
        }

        .status-badge.online {
            background: rgba(34, 197, 94, 0.2);
            color: #22c55e;
            border: 1px solid rgba(34, 197, 94, 0.3);
        }

        .status-badge.offline {
            background: rgba(239, 68, 68, 0.2);
            color: #ef4444;
            border: 1px solid rgba(239, 68, 68, 0.3);
        }

        .status-indicator {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            animation: pulse 2s infinite;
        }

        .status-indicator.online {
            background: #22c55e;
            box-shadow: 0 0 8px rgba(34, 197, 94, 0.6);
        }

        .status-indicator.offline {
            background: #ef4444;
            animation: none;
        }

        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.5; }
        }

        .services-status {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
            gap: 15px;
            margin-bottom: 30px;
        }

        .service-card {
            padding: 16px;
            background: rgba(26, 31, 58, 0.6);
            border-radius: 10px;
            border: 1px solid rgba(100, 116, 139, 0.2);
        }

        .service-name {
            font-size: 14px;
            color: #94a3b8;
            margin-bottom: 8px;
        }

        .metrics-grid {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
            gap: 20px;
        }

        .metric-card {
            background: rgba(26, 31, 58, 0.6);
            padding: 24px;
            border-radius: 12px;
            border: 1px solid rgba(100, 116, 139, 0.2);
            transition: all 0.2s;
        }

        .metric-card:hover {
            border-color: rgba(0, 212, 255, 0.4);
            box-shadow: 0 4px 20px rgba(0, 212, 255, 0.1);
        }

        .metric-title {
            font-size: 13px;
            color: #94a3b8;
            margin-bottom: 12px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            font-weight: 500;
        }

        .metric-value {
            font-size: 36px;
            font-weight: 700;
            color: #00d4ff;
            line-height: 1;
        }

        .metric-unit {
            font-size: 20px;
            color: #64748b;
            margin-left: 6px;
            font-weight: 400;
        }

        .metric-subtitle {
            font-size: 12px;
            color: #64748b;
            margin-top: 8px;
        }

        .logs {
            margin-top: 30px;
            padding: 20px;
            background: rgba(15, 23, 42, 0.6);
            border-radius: 12px;
            border: 1px solid rgba(100, 116, 139, 0.2);
            max-height: 200px;
            overflow-y: auto;
        }

        .logs h3 {
            font-size: 14px;
            color: #94a3b8;
            margin-bottom: 12px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }

        .log-entry {
            font-family: 'Monaco', 'Menlo', 'Courier New', monospace;
            font-size: 12px;
            color: #cbd5e1;
            padding: 4px 0;
            border-bottom: 1px solid rgba(100, 116, 139, 0.1);
        }

        .log-entry:last-child {
            border-bottom: none;
        }

        .log-entry .time {
            color: #64748b;
            margin-right: 10px;
        }

        .log-entry.success {
            color: #22c55e;
        }

        .log-entry.error {
            color: #ef4444;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>
                <span class="status-indicator online" id="overall-status"></span>
                DeepStream Vision Dashboard
            </h1>
            <div class="hostname-config">
                <label>Device:</label>
                <input type="text" id="hostname-input" value="" placeholder="wendyos-tender-oar.local">
                <button class="btn" onclick="updateHostname()">Update</button>
            </div>
        </div>

        <div class="services-status">
            <div class="service-card">
                <div class="service-name">Detector</div>
                <span class="status-badge offline" id="detector-status">
                    <span class="status-indicator offline"></span>
                    Checking...
                </span>
            </div>
            <div class="service-card">
                <div class="service-name">VLM (Qwen2.5-VL)</div>
                <span class="status-badge offline" id="vlm-status">
                    <span class="status-indicator offline"></span>
                    Checking...
                </span>
            </div>
            <div class="service-card">
                <div class="service-name">GPU Stats</div>
                <span class="status-badge offline" id="gpu-status">
                    <span class="status-indicator offline"></span>
                    Checking...
                </span>
            </div>
        </div>

        <div class="metrics-grid" id="metrics">
            <div class="metric-card">
                <div class="metric-title">Loading metrics...</div>
            </div>
        </div>

        <div class="logs">
            <h3>System Log</h3>
            <div id="logs-content"></div>
        </div>
    </div>

    <script>
        // Get current hostname from URL or use default
        let BASE_URL = window.location.origin;
        const hostnameInput = document.getElementById('hostname-input');
        hostnameInput.value = window.location.hostname;

        const logs = [];

        function log(message, type = 'info') {
            const now = new Date();
            const time = now.toLocaleTimeString();
            logs.push({ time, message, type });

            // Keep only last 50 logs
            if (logs.length > 50) logs.shift();

            const logsContent = document.getElementById('logs-content');
            logsContent.innerHTML = logs.reverse().map(l =>
                `<div class="log-entry ${l.type}"><span class="time">${l.time}</span>${l.message}</div>`
            ).join('');
            logs.reverse();
        }

        function updateHostname() {
            const hostname = hostnameInput.value.trim();
            if (!hostname) return;

            // Update base URL
            const protocol = window.location.protocol;
            const port = window.location.port;
            BASE_URL = `${protocol}//${hostname}${port ? ':' + port : ''}`;

            log(`Dashboard URL updated to: ${BASE_URL}`, 'info');
            localStorage.setItem('dashboardHostname', hostname);

            // Refresh data
            checkServices();
            updateMetrics();
        }

        async function checkService(endpoint, name) {
            try {
                const response = await fetch(`${BASE_URL}${endpoint}`, { timeout: 2000 });
                const data = await response.json();
                return { online: response.ok, data };
            } catch (error) {
                return { online: false, error: error.message };
            }
        }

        async function checkServices() {
            // Check detector
            const detector = await checkService('/api/detector/health', 'Detector');
            const detectorStatus = document.getElementById('detector-status');
            if (detector.online) {
                detectorStatus.className = 'status-badge online';
                detectorStatus.innerHTML = '<span class="status-indicator online"></span>Online';
            } else {
                detectorStatus.className = 'status-badge offline';
                detectorStatus.innerHTML = '<span class="status-indicator offline"></span>Offline';
            }

            // Check VLM
            const vlm = await checkService('/api/vlm/health', 'VLM');
            const vlmStatus = document.getElementById('vlm-status');
            if (vlm.online) {
                vlmStatus.className = 'status-badge online';
                vlmStatus.innerHTML = '<span class="status-indicator online"></span>Online';
            } else {
                vlmStatus.className = 'status-badge offline';
                vlmStatus.innerHTML = '<span class="status-indicator offline"></span>Offline';
            }

            // Check GPU
            const gpu = await checkService('/api/gpu/health', 'GPU Stats');
            const gpuStatus = document.getElementById('gpu-status');
            if (gpu.online) {
                gpuStatus.className = 'status-badge online';
                gpuStatus.innerHTML = '<span class="status-indicator online"></span>Online';
            } else {
                gpuStatus.className = 'status-badge offline';
                gpuStatus.innerHTML = '<span class="status-indicator offline"></span>Offline';
            }

            // Update overall status
            const overallStatus = document.getElementById('overall-status');
            const allOnline = detector.online && vlm.online && gpu.online;
            overallStatus.className = allOnline ? 'status-indicator online' : 'status-indicator offline';
        }

        async function updateMetrics() {
            try {
                const response = await fetch(`${BASE_URL}/api/aggregated`);
                const data = await response.json();

                let html = '';

                // GPU Utilization
                const gpuUtil = Object.entries(data.gpu).find(([k]) => k.includes('jetson_gpu_utilization'));
                if (gpuUtil) {
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">GPU Utilization</div>
                            <div class="metric-value">${gpuUtil[1].toFixed(1)}<span class="metric-unit">%</span></div>
                        </div>
                    `;
                }

                // GPU Temperature
                const gpuTemp = Object.entries(data.gpu).find(([k]) => k.includes('jetson_gpu_temperature'));
                if (gpuTemp) {
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">GPU Temperature</div>
                            <div class="metric-value">${gpuTemp[1].toFixed(1)}<span class="metric-unit">°C</span></div>
                        </div>
                    `;
                }

                // FPS
                const fps = Object.entries(data.detector).find(([k]) => k.includes('deepstream_fps'));
                if (fps) {
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">Detector FPS</div>
                            <div class="metric-value">${fps[1].toFixed(1)}<span class="metric-unit">fps</span></div>
                        </div>
                    `;
                }

                // Detections
                const detections = Object.entries(data.detector).filter(([k]) => k.includes('deepstream_detections_total'));
                if (detections.length > 0) {
                    const total = detections.reduce((sum, [, v]) => sum + v, 0);
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">Total Detections</div>
                            <div class="metric-value">${Math.floor(total)}</div>
                        </div>
                    `;
                }

                // VLM Info
                if (data.vlm && data.vlm.model_loaded) {
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">VLM Model</div>
                            <div class="metric-value" style="font-size: 18px;">${data.vlm.model_name || 'Qwen2.5-VL-3B'}</div>
                            <div class="metric-subtitle">${data.vlm.quantization || 'INT4'} quantization</div>
                        </div>
                    `;
                }

                // VLM Descriptions
                const vlmDesc = Object.entries(data.detector).find(([k]) => k.includes('vlm_descriptions_total'));
                if (vlmDesc) {
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">VLM Descriptions</div>
                            <div class="metric-value">${Math.floor(vlmDesc[1])}</div>
                        </div>
                    `;
                }

                // Memory
                const memUsed = Object.entries(data.gpu).find(([k]) => k.includes('jetson_gpu_memory_used'));
                const memTotal = Object.entries(data.gpu).find(([k]) => k.includes('jetson_gpu_memory_total'));
                if (memUsed && memTotal) {
                    const usedGB = (memUsed[1]/1024).toFixed(1);
                    const totalGB = (memTotal[1]/1024).toFixed(1);
                    const percent = ((memUsed[1] / memTotal[1]) * 100).toFixed(0);
                    html += `
                        <div class="metric-card">
                            <div class="metric-title">GPU Memory</div>
                            <div class="metric-value">${usedGB}<span class="metric-unit">GB</span></div>
                            <div class="metric-subtitle">${percent}% of ${totalGB} GB</div>
                        </div>
                    `;
                }

                if (html === '') {
                    html = '<div class="metric-card"><div class="metric-title">No metrics available yet...</div></div>';
                }

                document.getElementById('metrics').innerHTML = html;
            } catch (e) {
                log(`Error fetching metrics: ${e.message}`, 'error');
            }
        }

        // Load saved hostname
        const savedHostname = localStorage.getItem('dashboardHostname');
        if (savedHostname) {
            hostnameInput.value = savedHostname;
            updateHostname();
        }

        // Initial update
        log('Dashboard started', 'success');
        checkServices();
        updateMetrics();

        // Auto-refresh
        setInterval(() => {
            checkServices();
            updateMetrics();
        }, 3000);
    </script>
</body>
</html>
    ''')


if __name__ == '__main__':
    logger.info("Starting DeepStream Vision Dashboard")
    logger.info("Dashboard available at http://0.0.0.0:8080")
    logger.info("Access from browser at http://<jetson-hostname>:8080")

    app.run(host='0.0.0.0', port=8080, debug=False)
