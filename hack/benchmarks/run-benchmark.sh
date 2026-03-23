#!/usr/bin/env bash
set -euo pipefail

PLATFORM=${1:-kind}
SCENARIO=${2:-scale-up-latency}
DURATION=${3:-15m}

echo "Starting benchmark on ${PLATFORM}..."
echo "Scenario: ${SCENARIO}"
echo "Duration: ${DURATION}"

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
ROOT_DIR="$(cd "${DIR}/../.." >/dev/null 2>&1 && pwd)"

# 1. Setup Cluster & Deploy WVA
if [ "$PLATFORM" == "kind" ]; then
    echo "Setting up Kind cluster and deploying WVA..."
    cd "${ROOT_DIR}"
    # Clean up existing cluster if any
    make destroy-kind-cluster || true
    make create-kind-cluster
    export INSTALL_GATEWAY_CTRLPLANE=true
    export CREATE_CLUSTER=false
    export VLLM_MAX_NUM_SEQS=8
    make deploy-wva-emulated-on-kind
    
    # Import llm-d dashboard into Grafana
    echo "Importing llm-d dashboard into Grafana..."
    curl -sO https://raw.githubusercontent.com/llm-d/llm-d/main/docs/monitoring/grafana/dashboards/llm-d-dashboard.json
    kubectl create configmap llm-d-dashboard --from-file=llm-d-dashboard.json=llm-d-dashboard.json -n workload-variant-autoscaler-monitoring
    kubectl label configmap llm-d-dashboard grafana_dashboard=1 -n workload-variant-autoscaler-monitoring
else
    echo "OpenShift not yet implemented in Phase 1."
    exit 1
fi

# 2. Wait for system to be ready
echo "Waiting for WVA and model service to be ready..."
kubectl wait --for=condition=available deployment/workload-variant-autoscaler-controller-manager -n workload-variant-autoscaler-system --timeout=300s
# The emulated install deploys a model service named 'ms-sim-llm-d-modelservice-decode' in 'llm-d-sim' namespace by default
kubectl wait --for=condition=available deployment/ms-sim-llm-d-modelservice-decode -n llm-d-sim --timeout=300s || echo "Model service not ready yet, proceeding anyway..."

# 3. Run Workload
echo "Running load generator..."
# We will port-forward the gateway/service to run the workload locally
kubectl port-forward svc/infra-sim-inference-gateway-istio -n llm-d-sim 8000:80 &
PF_PID=$!
sleep 5 # Wait for port-forward to establish

export TARGET_URL="http://localhost:8000/v1/chat/completions"
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"

echo "Executing scenario: ${SCENARIO}"
python3 "${DIR}/run_workload.py" "${DIR}/${SCENARIO}.yaml"

# Cleanup port-forward
kill $PF_PID || true

# 4. Collect Results
echo "Collecting results from Prometheus..."
# Port-forward Prometheus to collect metrics
kubectl port-forward svc/kube-prometheus-stack-prometheus -n workload-variant-autoscaler-monitoring 9090:9090 &
PROM_PF_PID=$!
sleep 5 # Wait for port-forward

export PROMETHEUS_URL="http://localhost:9090"
python3 "${DIR}/collect_metrics.py"

# 4.1 Export Grafana Dashboard Snapshot
echo "Exporting Grafana dashboard snapshot..."
kubectl port-forward svc/kube-prometheus-stack-grafana -n workload-variant-autoscaler-monitoring 3000:80 &
GRAFANA_PF_PID=$!
sleep 5 # Wait for port-forward

# Create service account and token for API access
curl -s -X POST -u admin:admin -H "Content-Type: application/json" -d '{"name":"snapshot-sa", "role":"Admin"}' http://localhost:3000/api/serviceaccounts > /dev/null || true
SA_ID=$(curl -s -u admin:admin http://localhost:3000/api/serviceaccounts/search | jq -r '.serviceAccounts[] | select(.name=="snapshot-sa") | .id')
TOKEN_JSON=$(curl -s -X POST -u admin:admin -H "Content-Type: application/json" -d '{"name":"token-'$(date +%s)'"}' http://localhost:3000/api/serviceaccounts/${SA_ID}/tokens)
GRAFANA_TOKEN=$(echo $TOKEN_JSON | jq -r '.key')

# Wait for dashboard to be provisioned
echo "Waiting for Grafana dashboard to be provisioned..."
sleep 30
# Create dashboard manually via API to guarantee it exists
DASHBOARD_JSON=$(cat llm-d-dashboard.json | jq '.id = null | .uid = "llm-d-dashboard"')
curl -s -X POST -u admin:admin -H "Content-Type: application/json" -d "{\"dashboard\": ${DASHBOARD_JSON}, \"overwrite\": true}" http://localhost:3000/api/dashboards/db > /dev/null || true

DASHBOARD_UID=$(curl -s -u admin:admin http://localhost:3000/api/search | jq -r '.[] | select(.title=="llm-d dashboard") | .uid')

if [ -z "$DASHBOARD_UID" ] || [ "$DASHBOARD_UID" == "null" ]; then
    echo "Dashboard not found by title, trying to find by uid..."
    DASHBOARD_UID=$(curl -s -u admin:admin http://localhost:3000/api/search | jq -r '.[] | select(.uid=="llm-d-dashboard") | .uid')
fi

if [ -n "$DASHBOARD_UID" ] && [ "$DASHBOARD_UID" != "null" ]; then
    echo "Found dashboard UID: $DASHBOARD_UID"
    # Install grafana-snapshots if not present
    pip3 install grafana-snapshots > /dev/null 2>&1 || true
    
    # Create config file
    cat << EOF > grafana-config.yaml
general:
  debug: false
grafana:
  default:
    host: localhost
    port: 3000
    protocol: http
    token: ${GRAFANA_TOKEN}
EOF

    # Export snapshot
    grafana-snapshots export -c grafana-config.yaml -d "llm-d dashboard" || echo "Warning: Failed to export dashboard snapshot"
    
    if [ -f "llm-d_dashboard.json" ]; then
        mv "llm-d_dashboard.json" "grafana_snapshot.json"
        echo "Successfully exported grafana_snapshot.json"
    fi
else
    echo "Warning: Could not find 'llm-d dashboard' in Grafana"
fi

kill $PROM_PF_PID || true
kill $GRAFANA_PF_PID || true

# 5. Generate Summary
echo "Generating PR comment summary..."
export SCENARIO="${SCENARIO}"
export PLATFORM="${PLATFORM}"
python3 "${DIR}/post_pr_comment.py" "benchmark_summary.json"

echo "Benchmark complete!"
