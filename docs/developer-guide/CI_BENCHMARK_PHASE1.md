# CI Benchmark - Phase 1 Implementation Guide

This document outlines the end-to-end implementation of Phase 1 for the automated Workload Variant Autoscaler (WVA) benchmarking system in GitHub Actions.

## Goal

The objective of Phase 1 was to automate the deployment of a Kind cluster, install WVA and its dependencies (including Prometheus and Grafana), run a simulated load generation scenario (`scale-up-latency`), collect key metrics, and post a summary of the results as a PR comment with a Grafana dashboard snapshot artifact.

## End-to-End Workflow

1. **Trigger:** The workflow (`.github/workflows/ci-benchmark.yaml`) is triggered. For testing purposes, it was temporarily configured to run on `push` to the feature branch, but its final state is designed to run on `issue_comment` (e.g., typing `/benchmark` in a PR comment) or `workflow_dispatch`.
2. **Setup:** The GitHub Action provisions an Ubuntu runner, checks out the code, and sets up Go.
3. **Cluster Creation:** The script `hack/benchmarks/run-benchmark.sh` destroys any existing Kind cluster and creates a fresh one (`make create-kind-cluster`).
4. **Infrastructure Deployment:** It runs `make deploy-wva-emulated-on-kind`, which executes `deploy/install.sh`. This script installs:
   * Gateway API CRDs
   * Istio (Gateway Control Plane)
   * `kube-prometheus-stack` (Prometheus + Grafana)
   * The WVA Controller
   * `llm-d-inference-sim` (a simulated vLLM server that responds to requests instantly or with configured latency)
5. **Dashboard Import:** The `llm-d-dashboard.json` is downloaded from the main `llm-d` repository and injected into Grafana via a labeled Kubernetes ConfigMap (`grafana_dashboard=1`).
6. **Load Generation:** The python script `hack/benchmarks/run_workload.py` parses `hack/benchmarks/scale-up-latency.yaml` and orchestrates `hack/burst_load_generator.sh` to send a burst of concurrent `curl` requests to the simulated endpoint.
7. **Metric Collection:** After the load test, `hack/benchmarks/collect_metrics.py` queries Prometheus for metrics like scale-up latency, scale-down latency, max replicas, avg queue depth, and P99 TTFT.
8. **Grafana Snapshot:** The `grafana-snapshots` Python CLI tool connects to the local Grafana instance, finds the "llm-d dashboard", and exports it as a JSON file (`grafana_snapshot.json`).
9. **Artifact Upload & PR Comment:** The snapshot JSON is uploaded as a GitHub Actions artifact, and a markdown summary of the metrics is generated and posted to the PR (or workflow summary).

## Key Fixes & Modifications Applied

During the implementation, several issues were encountered and resolved:

### 1. Fork Bomb in Load Generator
* **Issue:** `burst_load_generator.sh` was spawning thousands of background `curl` processes simultaneously, causing a `fork: Resource temporarily unavailable` error on the GitHub Actions runner.
* **Fix:** Added a `MAX_CONCURRENT=500` limit and a `while` loop to ensure no more than 500 background jobs run at once.

### 2. Helm Deployment Timeouts
* **Issue:** The `helm upgrade` command for the WVA controller was failing with `Error: context deadline exceeded`.
* **Fix:** The `--wait` and `--timeout=10m` arguments were placed in the middle of the argument list, causing bash conditional variables (`${CONTROLLER_INSTANCE:+...}`) to be parsed incorrectly. Moved the wait flags to the absolute end of the command.

### 3. Hardcoded Namespaces in CI
* **Issue:** WVA deployment failed because it was looking for `kube-prometheus-stack` in the `default` namespace instead of `workload-variant-autoscaler-monitoring`.
* **Fix:** Explicitly added `--set wva.prometheus.monitoringNamespace=$MONITORING_NAMESPACE` and `--set llmd.namespace=$LLM_D_NS` to the `helm upgrade` command to override hardcoded values in `values-dev.yaml`.

### 4. Zero Latency / Empty Queues in Simulator
* **Issue:** The `Avg queue depth` and `P99 TTFT` metrics were returning `0.00` because the simulator was processing requests instantly.
* **Fix:** 
  * Set `VLLM_MAX_NUM_SEQS=8` in the benchmark script to force the simulator to queue requests when bombarded with a burst of 20 req/s.
  * Updated `deploy/install.sh` to append `ms` to the latency arguments (`--time-to-first-token=${TTFT_AVERAGE_LATENCY_MS}ms`) because the `latest` tag of the simulator expects Go duration strings, not integers.

### 5. Grafana API Connection Issues
* **Issue:** The `grafana-snapshots` tool failed to connect to Grafana because Prometheus was serving over HTTPS with a self-signed cert, causing Grafana's default datasource health check to fail.
* **Fix:** Configured Grafana with a new `Prometheus-HTTPS` datasource that has `tlsSkipVerify: true` enabled.

### 6. Missing Grafana Dashboard
* **Issue:** The `grafana_snapshot.json` artifact wasn't being uploaded because the "llm-d dashboard" didn't exist in the fresh Grafana instance.
* **Fix:** Added a step in `run-benchmark.sh` to download `llm-d-dashboard.json` and create a labeled ConfigMap so the Grafana sidecar automatically imports it.
