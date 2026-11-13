// Recorded verbatim on 2026-10-01 from hopeful-glider (Jetson Orin Nano, agent 2026.10.01-013419)
// over LAN (link-local IPv6) with Wendy CLI / MCP server 2026.10.01-013419. Do not edit by hand:
// re-record from a device instead. The test apps live in the session scratchpad (wm-*).

// wendy_status after device_connect
export const LIVE_STATUS = {
  "cli_update": {
    "available": false,
    "last_checked_at": "2026-10-01T15:37:00Z"
  },
  "cli_version": "2026.10.01-013419",
  "command_target": {
    "device": "[fe80::720f:67c5:9653:4450%en11]:50052",
    "transport": "direct"
  },
  "connected": true,
  "connection_type": "direct",
  "device": "fe80::720f:67c5:9653:4450%en11",
  "installation_jobs": true,
  "installation_planning": true,
  "proxy_diagnostics": [],
  "simulator_management": true,
  "suggested_next_step": "Connected to fe80::720f:67c5:9653:4450%en11 via direct. Use run or inspect containers and logs; enable specialist groups with wendy_tools.",
  "tool_groups": [
    "core"
  ]
}

// run of wm-healthy (python http.server --bind :: 8931, TCP readiness probe)
export const LIVE_RUN = {
  "output": "Registered sh.wendy.wendymods.healthy in Cloud Apps (organization 2).\nNotifications for sh.wendy.wendymods.healthy are disabled; an owner or admin can enable its grant in the Cloud app settings.\nBuilding image (OCI layout) for linux/arm64...\n  done    preparing buildx builder  0.3s\n  done    load build definition  0.0s\n  done    load metadata  0.3s\n  cached  [1/1] FROM docker.io/library/python:3.12-slim@sha256:eeb8088e67610b37583880c7627e3931f087cba55a35810819e34a398f624a47\n✓ Built & pushed (1 cached, 0 rebuilt) in 518ms\nDiffing 4 layer(s) (46.9MB compressed) against device...\nReusing 4 layer(s) already on device; chunking 0.\nAll 4 layer(s) already on device; nothing to send.\nApplication sh.wendy.wendymods.healthy running in detached mode.\n{\n  \"status\": \"started\",\n  \"app\": \"sh.wendy.wendymods.healthy\",\n  \"device\": \"[fe80::720f:67c5:9653:4450%en11]:50052\",\n  \"readiness\": \"not_checked\",\n  \"endpoints\": []\n}",
  "readiness": "not_checked",
  "status": "started",
  "suggested_next_step": "Connect to the returned target, check container_list and telemetry_logs, then test the app's health endpoint or ROS interface. Deployment alone does not verify behavior.",
  "target": {
    "device": "[fe80::720f:67c5:9653:4450%en11]:50052",
    "transport": "direct"
  },
  "truncated": false
}

// app_inspect of wm-healthy, readiness passed
export const LIVE_INSPECT_HEALTHY = {
  "app_name": "sh.wendy.wendymods.healthy",
  "readiness": {
    "checks": [
      {
        "kind": "tcp_socket",
        "port": 8931,
        "status": "passed"
      }
    ],
    "configuration_source": "local_project",
    "deployed_configuration_verified": false,
    "scope": "declared TCP connectivity only",
    "status": "passed"
  },
  "recent_logs": {
    "batches_collected": 0,
    "collected": 0,
    "collection_limited": true,
    "min_severity": 13,
    "omitted": 0,
    "reason": "no warning/error records observed in this bounded collection",
    "records": [],
    "status": "unknown"
  },
  "state": {
    "all_services_running": true,
    "failure_count": 0,
    "last_exit": {
      "status": "unknown"
    },
    "running_state": "RUNNING",
    "services": [],
    "version": "0.1.0"
  },
  "usage": {
    "containers": [
      {
        "container_name": "sh.wendy.wendymods.healthy",
        "cpu_usage_nanos": 585572000,
        "image_content_bytes": 156803182,
        "memory_bytes": 18423808,
        "status": "reported"
      }
    ],
    "note": "Zero counters are unknown because the agent also uses zero when measurements are unavailable; CPU is cumulative nanoseconds.",
    "warnings": []
  }
}

// app_inspect of wm-healthy while it listened on IPv4 only: probe over link-local IPv6 refused
export const LIVE_INSPECT_READINESS_FAILED = {
  "app_name": "sh.wendy.wendymods.healthy",
  "readiness": {
    "checks": [
      {
        "kind": "tcp_socket",
        "port": 8931,
        "reason": "TCP connection could not be established: dial tcp [fe80::720f:67c5:9653:4450%en11]:8931: connect: connection refused",
        "status": "failed"
      }
    ],
    "configuration_source": "local_project",
    "deployed_configuration_verified": false,
    "scope": "declared TCP connectivity only",
    "status": "failed"
  },
  "recent_logs": {
    "batches_collected": 0,
    "collected": 0,
    "collection_limited": true,
    "min_severity": 13,
    "omitted": 0,
    "reason": "no warning/error records observed in this bounded collection",
    "records": [],
    "status": "unknown"
  },
  "state": {
    "all_services_running": true,
    "failure_count": 0,
    "last_exit": {
      "status": "unknown"
    },
    "running_state": "RUNNING",
    "services": [],
    "version": "0.1.0"
  },
  "usage": {
    "containers": [
      {
        "container_name": "sh.wendy.wendymods.healthy",
        "cpu_usage_nanos": 580980000,
        "image_content_bytes": 156803150,
        "memory_bytes": 18481152,
        "status": "reported"
      }
    ],
    "note": "Zero counters are unknown because the agent also uses zero when measurements are unavailable; CPU is cumulative nanoseconds.",
    "warnings": []
  }
}

// app_inspect of wm-noprobe, no readiness probe declared
export const LIVE_INSPECT_NO_PROBES = {
  "app_name": "sh.wendy.wendymods.noprobe",
  "readiness": {
    "checks": [],
    "configuration_source": "local_project",
    "deployed_configuration_verified": false,
    "reason": "project declares no TCP readiness probes",
    "status": "unknown"
  },
  "recent_logs": {
    "batches_collected": 0,
    "collected": 0,
    "collection_limited": true,
    "min_severity": 13,
    "omitted": 0,
    "reason": "no warning/error records observed in this bounded collection",
    "records": [],
    "status": "unknown"
  },
  "state": {
    "all_services_running": true,
    "failure_count": 0,
    "last_exit": {
      "status": "unknown"
    },
    "running_state": "RUNNING",
    "services": [],
    "version": "0.1.0"
  },
  "usage": {
    "containers": [
      {
        "container_name": "sh.wendy.wendymods.noprobe",
        "cpu_usage_nanos": 580403000,
        "image_content_bytes": 156803182,
        "memory_bytes": 18436096,
        "status": "reported"
      }
    ],
    "note": "Zero counters are unknown because the agent also uses zero when measurements are unavailable; CPU is cumulative nanoseconds.",
    "warnings": []
  }
}

// app_inspect of wm-crash ~40 s after deploy: exits 1 every ~10 s, agent restarts it
export const LIVE_INSPECT_CRASHED = {
  "app_name": "sh.wendy.wendymods.crash",
  "readiness": {
    "checks": [],
    "configuration_source": "local_project",
    "deployed_configuration_verified": false,
    "reason": "project declares no TCP readiness probes",
    "status": "unknown"
  },
  "recent_logs": {
    "batches_collected": 3,
    "collected": 3,
    "collection_limited": true,
    "min_severity": 13,
    "omitted": 0,
    "records": [
      {
        "attributes": {
          "stream": "stderr"
        },
        "body": "fatal: simulated crash\n",
        "is_history": true,
        "observedTimeUnixNano": "1790905527356005473",
        "resource": {
          "service.instance.id": "wendyos-hopeful-glider",
          "service.name": "sh.wendy.wendymods.crash",
          "service.namespace": "wendy",
          "service.version": "0.1.0"
        },
        "scope": {
          "name": "wendy.container"
        },
        "severityNumber": "SEVERITY_NUMBER_FATAL",
        "severityText": "FATAL",
        "timeUnixNano": "1790905527356005473"
      },
      {
        "attributes": {
          "stream": "stderr"
        },
        "body": "fatal: simulated crash\n",
        "is_history": true,
        "observedTimeUnixNano": "1790905548147826721",
        "resource": {
          "service.instance.id": "wendyos-hopeful-glider",
          "service.name": "sh.wendy.wendymods.crash",
          "service.namespace": "wendy",
          "service.version": "0.1.0"
        },
        "scope": {
          "name": "wendy.container"
        },
        "severityNumber": "SEVERITY_NUMBER_FATAL",
        "severityText": "FATAL",
        "timeUnixNano": "1790905548147826721"
      },
      {
        "attributes": {
          "stream": "stderr"
        },
        "body": "fatal: simulated crash\n",
        "is_history": true,
        "observedTimeUnixNano": "1790905563174205757",
        "resource": {
          "service.instance.id": "wendyos-hopeful-glider",
          "service.name": "sh.wendy.wendymods.crash",
          "service.namespace": "wendy",
          "service.version": "0.1.0"
        },
        "scope": {
          "name": "wendy.container"
        },
        "severityNumber": "SEVERITY_NUMBER_FATAL",
        "severityText": "FATAL",
        "timeUnixNano": "1790905563174205757"
      }
    ],
    "status": "observed"
  },
  "state": {
    "all_services_running": false,
    "failure_count": 2,
    "last_exit": {
      "code": 1,
      "reason": "crashed",
      "status": "recorded"
    },
    "running_state": "CRASH_LOOPING",
    "services": [],
    "version": "0.1.0"
  },
  "usage": {
    "containers": [
      {
        "container_name": "sh.wendy.wendymods.crash",
        "cpu_usage_nanos": 28559000,
        "image_content_bytes": 156803388,
        "memory_bytes": 53248,
        "status": "reported"
      }
    ],
    "note": "Zero counters are unknown because the agent also uses zero when measurements are unavailable; CPU is cumulative nanoseconds.",
    "warnings": []
  }
}
// container_list while wm-crash was stopped between restarts
export const LIVE_CONTAINERS = {
  "containers": [
    {
      "app_name": "badge-test",
      "app_version": "0.1.0",
      "failure_count": 0,
      "running_state": "STOPPED"
    },
    {
      "app_name": "sh.wendy.wendymods.crash",
      "app_version": "0.1.0",
      "exit_code": 1,
      "failure_count": 0,
      "running_state": "STOPPED",
      "termination_reason": "crashed"
    },
    {
      "app_name": "sh.wendy.wendymods.healthy",
      "app_version": "0.1.0",
      "failure_count": 0,
      "running_state": "RUNNING"
    },
    {
      "app_name": "sh.wendy.wendymods.noprobe",
      "app_version": "0.1.0",
      "failure_count": 0,
      "running_state": "RUNNING"
    }
  ]
}
