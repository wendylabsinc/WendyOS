#!/usr/bin/env python3
"""
Jetson GPU Stats Prometheus Exporter

Parses tegrastats output and exposes metrics in Prometheus format.
Falls back to psutil for non-Jetson platforms.
"""

import subprocess
import re
import time
import logging
from typing import Dict, Optional
from flask import Flask, Response
from prometheus_client import Counter, Gauge, generate_latest, REGISTRY
import psutil

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

    resource = Resource.create({"service.name": "gpu-stats"})
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

app = Flask(__name__)


@app.after_request
def add_cors_headers(response):
    response.headers['Access-Control-Allow-Origin'] = '*'
    response.headers['Access-Control-Allow-Methods'] = 'GET, POST, OPTIONS'
    response.headers['Access-Control-Allow-Headers'] = 'Content-Type'
    return response


# Prometheus metrics
gpu_utilization = Gauge('jetson_gpu_utilization_percent', 'GPU utilization percentage')
gpu_frequency = Gauge('jetson_gpu_frequency_mhz', 'GPU frequency in MHz')
gpu_memory_used = Gauge('jetson_gpu_memory_used_mb', 'GPU memory used in MB')
gpu_memory_total = Gauge('jetson_gpu_memory_total_mb', 'GPU memory total in MB')
gpu_temperature = Gauge('jetson_gpu_temperature_celsius', 'GPU temperature in Celsius')
cpu_utilization = Gauge('jetson_cpu_utilization_percent', 'CPU utilization by core', ['core'])
temperature_gauge = Gauge('jetson_temperature_celsius', 'Temperature by sensor', ['sensor'])
power_gauge = Gauge('jetson_power_watts', 'Power consumption by rail', ['rail'])


class TegrastatsParser:
    """Parse tegrastats output for Jetson metrics"""

    def __init__(self):
        self.is_jetson = self._check_jetson()

    def _check_jetson(self) -> bool:
        """Check if running on Jetson"""
        try:
            result = subprocess.run(['tegrastats', '--help'], 
                                    capture_output=True, timeout=2)
            return result.returncode == 0
        except:
            return False

    def parse_tegrastats_line(self, line: str) -> Dict:
        """Parse a single line of tegrastats output"""
        metrics = {}

        try:
            # GPU utilization: GR3D_FREQ 99%@1300 or GR3D_FREQ 48%@[611]
            gpu_match = re.search(r'GR3D_FREQ\s+(\d+)%@\[?(\d+)\]?', line)
            if gpu_match:
                metrics['gpu_utilization'] = int(gpu_match.group(1))
                metrics['gpu_frequency'] = int(gpu_match.group(2))

            # GPU memory: EMC_FREQ 1%@204
            mem_match = re.search(r'EMC_FREQ\s+\d+%@(\d+)', line)
            if mem_match:
                metrics['emc_frequency'] = int(mem_match.group(1))

            # RAM: RAM 2048/8192MB
            ram_match = re.search(r'RAM\s+(\d+)/(\d+)MB', line)
            if ram_match:
                metrics['ram_used_mb'] = int(ram_match.group(1))
                metrics['ram_total_mb'] = int(ram_match.group(2))

            # CPU utilization: [12%@1190,14%@1190,11%@1190,13%@1190,8%@345,9%@345]
            cpu_match = re.findall(r'(\d+)%@(\d+)', line)
            if cpu_match:
                metrics['cpu_cores'] = [(int(util), int(freq)) for util, freq in cpu_match]

            # Temperature: CPU@52C GPU@54C
            temp_matches = re.findall(r'(\w+)@([\d.]+)C', line)
            if temp_matches:
                metrics['temperatures'] = {name: float(temp) for name, temp in temp_matches}

            # Power: VDD_IN 8456/8456 VDD_CPU_GPU_CV 3456/3456
            power_matches = re.findall(r'(\w+)\s+(\d+)/(\d+)', line)
            if power_matches:
                metrics['power'] = {name: int(current) / 1000.0 for name, current, avg in power_matches}

        except Exception as e:
            logger.error(f"Error parsing tegrastats: {e}")

        return metrics

    def get_metrics(self) -> Dict:
        """Get current metrics from tegrastats or psutil"""
        if self.is_jetson:
            return self._get_tegrastats_metrics()
        else:
            return self._get_psutil_metrics()

    def _get_tegrastats_metrics(self) -> Dict:
        """Get metrics from tegrastats (Jetson)"""
        try:
            # Run tegrastats once with --interval 1 --logfile /dev/stdout and capture one line
            proc = subprocess.Popen(
                ['tegrastats', '--interval', '1000'],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True
            )

            # Read one line of output
            line = proc.stdout.readline()
            proc.terminate()

            if line:
                return self.parse_tegrastats_line(line)

        except Exception as e:
            logger.error(f"Error reading tegrastats: {e}")

        return {}

    def _get_psutil_metrics(self) -> Dict:
        """Fallback metrics using psutil (non-Jetson)"""
        metrics = {}

        try:
            # CPU utilization per core
            cpu_percent = psutil.cpu_percent(interval=0.1, percpu=True)
            cpu_freq = psutil.cpu_freq(percpu=True)

            if cpu_freq:
                metrics['cpu_cores'] = [(percent, int(freq.current)) 
                                        for percent, freq in zip(cpu_percent, cpu_freq)]
            else:
                metrics['cpu_cores'] = [(percent, 0) for percent in cpu_percent]

            # Memory
            mem = psutil.virtual_memory()
            metrics['ram_used_mb'] = int(mem.used / 1024 / 1024)
            metrics['ram_total_mb'] = int(mem.total / 1024 / 1024)

            # Temperature (if available)
            try:
                temps = psutil.sensors_temperatures()
                if temps:
                    metrics['temperatures'] = {}
                    for name, entries in temps.items():
                        if entries:
                            metrics['temperatures'][name] = entries[0].current
            except:
                pass

        except Exception as e:
            logger.error(f"Error reading psutil metrics: {e}")

        return metrics


# Global parser instance
parser = TegrastatsParser()


def update_metrics():
    """Update Prometheus metrics from tegrastats"""
    metrics = parser.get_metrics()

    # GPU metrics
    if 'gpu_utilization' in metrics:
        gpu_utilization.set(metrics['gpu_utilization'])
    if 'gpu_frequency' in metrics:
        gpu_frequency.set(metrics['gpu_frequency'])

    # Memory metrics
    if 'ram_used_mb' in metrics:
        gpu_memory_used.set(metrics['ram_used_mb'])
    if 'ram_total_mb' in metrics:
        gpu_memory_total.set(metrics['ram_total_mb'])

    # CPU metrics
    if 'cpu_cores' in metrics:
        for idx, (util, freq) in enumerate(metrics['cpu_cores']):
            cpu_utilization.labels(core=f"cpu{idx}").set(util)

    # Temperature metrics
    if 'temperatures' in metrics:
        for sensor, temp in metrics['temperatures'].items():
            temperature_gauge.labels(sensor=sensor).set(temp)
            if sensor.lower() == 'gpu':
                gpu_temperature.set(temp)

    # Power metrics
    if 'power' in metrics:
        for rail, watts in metrics['power'].items():
            power_gauge.labels(rail=rail).set(watts)


@app.route('/metrics')
def metrics():
    """Prometheus metrics endpoint"""
    update_metrics()
    return Response(generate_latest(REGISTRY), mimetype='text/plain')


@app.route('/health')
def health():
    """Health check endpoint"""
    return {'status': 'healthy', 'is_jetson': parser.is_jetson}


if __name__ == '__main__':
    logger.info("Starting Jetson GPU Stats Exporter")
    logger.info(f"Platform: {'Jetson' if parser.is_jetson else 'Generic (psutil)'}")
    logger.info("Metrics available at http://0.0.0.0:9091/metrics")

    app.run(host='0.0.0.0', port=9091, debug=False)
