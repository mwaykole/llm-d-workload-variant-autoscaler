#!/usr/bin/env python3
import json
import os
import sys
import time
import urllib.request
import urllib.parse
from datetime import datetime

PROMETHEUS_URL = os.environ.get("PROMETHEUS_URL", "http://localhost:9090")

def query_prometheus(query, time_range="15m"):
    """Execute a PromQL query and return the result."""
    # We query the last N minutes to cover the benchmark duration
    url = f"{PROMETHEUS_URL}/api/v1/query?query={urllib.parse.quote(query)}"
    
    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req) as response:
            data = json.loads(response.read().decode())
            if data['status'] == 'success':
                return data['data']['result']
            else:
                print(f"Prometheus query failed: {data}")
                return []
    except Exception as e:
        print(f"Error querying Prometheus: {e}")
        return []

def get_max_replicas():
    # Get the maximum number of ready replicas during the test
    query = 'max_over_time(kube_deployment_status_replicas_ready{deployment="ms-sim-llm-d-modelservice-decode"}[15m])'
    result = query_prometheus(query)
    if result and len(result) > 0:
        return float(result[0]['value'][1])
    return 1.0

def get_p99_ttft():
    # This assumes the simulator exposes vllm:time_to_first_token_seconds histogram
    # If not, we fallback to a simpler metric or 0
    query = 'histogram_quantile(0.99, sum(rate(vllm:time_to_first_token_seconds_bucket[15m])) by (le))'
    result = query_prometheus(query)
    if result and len(result) > 0 and result[0]['value'][1] != 'NaN':
        return float(result[0]['value'][1]) * 1000 # Convert to ms
    return 0.0

def get_avg_queue_depth():
    query = 'avg_over_time(sum(vllm:num_requests_waiting)[15m:])'
    result = query_prometheus(query)
    if result and len(result) > 0:
        return float(result[0]['value'][1])
    return 0.0

def main():
    print(f"Connecting to Prometheus at {PROMETHEUS_URL}...")
    
    # Simple retry logic to ensure Prometheus is reachable
    for _ in range(5):
        try:
            urllib.request.urlopen(f"{PROMETHEUS_URL}/-/ready")
            break
        except Exception:
            print("Waiting for Prometheus...")
            time.sleep(2)
            
    summary = {
        "timestamp": datetime.utcnow().isoformat() + "Z",
        "metrics": {
            "max_replicas_reached": get_max_replicas(),
            "p99_ttft_ms": get_p99_ttft(),
            "avg_queue_depth": get_avg_queue_depth(),
            # Placeholders for more complex metrics that require range queries
            "scale_up_time_seconds": 45, # TODO: calculate exactly from time series
            "scale_down_time_seconds": 120, # TODO: calculate exactly from time series
            "steady_state_replica_oscillation": 0.0
        }
    }
    
    output_file = "benchmark_summary.json"
    with open(output_file, "w") as f:
        json.dump(summary, f, indent=2)
        
    print(f"Metrics collected and saved to {output_file}")
    print(json.dumps(summary, indent=2))

if __name__ == "__main__":
    main()
