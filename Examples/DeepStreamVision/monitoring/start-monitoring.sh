#!/bin/bash
# Start Grafana + Loki for log viewing
# Logs are retained for 2 hours

set -e

cd "$(dirname "$0")"

echo "Starting Grafana + Loki..."
docker-compose up -d

echo ""
echo "========================================="
echo "  Monitoring Stack Started"
echo "========================================="
echo ""
echo "Grafana:  http://localhost:3000"
echo "          Login: admin / admin"
echo ""
echo "Loki:     http://localhost:3100"
echo "          (receives logs from devices)"
echo ""
echo "To view logs:"
echo "  1. Open http://localhost:3000"
echo "  2. Go to Explore (compass icon)"
echo "  3. Select 'Loki' datasource"
echo "  4. Query: {job=\"deepstream-vision\"}"
echo ""
echo "To stop: docker-compose down"
echo ""
