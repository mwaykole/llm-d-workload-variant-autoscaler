#!/usr/bin/env python3
import json
import os
import sys

def main():
    if len(sys.argv) < 2:
        print("Usage: post_pr_comment.py <summary.json>")
        sys.exit(1)
        
    summary_file = sys.argv[1]
    with open(summary_file, 'r') as f:
        summary = json.load(f)
        
    metrics = summary.get("metrics", {})
    
    # Read environment variables provided by GitHub Actions
    pr_number = os.environ.get("PR_NUMBER", "unknown")
    commit_sha = os.environ.get("GITHUB_SHA", "unknown")[:7]
    scenario = os.environ.get("SCENARIO", "scale-up-latency")
    platform = os.environ.get("PLATFORM", "kind")
    
    comment_body = f"""## Benchmark Results: `{scenario}` on `{platform}`

| Metric | Value |
|--------|-------|
| Scale-up latency | {metrics.get('scale_up_time_seconds', 'N/A')}s |
| Scale-down latency | {metrics.get('scale_down_time_seconds', 'N/A')}s |
| Max replicas reached | {metrics.get('max_replicas_reached', 'N/A')} |
| Steady-state replica oscillation | ±{metrics.get('steady_state_replica_oscillation', 'N/A')} |
| Avg queue depth | {metrics.get('avg_queue_depth', 'N/A'):.2f} |
| P99 TTFT | {metrics.get('p99_ttft_ms', 'N/A'):.2f}ms |

**Grafana Dashboard**: A snapshot of the Grafana dashboard containing all metric graphs for this run is attached to the GitHub Actions run artifacts as `grafana_snapshot.json`. You can import this JSON file into any Grafana instance to view the interactive graphs.

<details>
<summary>Run details</summary>

- PR: #{pr_number} ({commit_sha})
- Platform: {platform}
- Scenario: {scenario}
- Timestamp: {summary.get('timestamp', 'unknown')}

</details>
"""
    
    # In GitHub Actions, we write this to the step summary or a file to be used by another step
    output_file = os.environ.get("GITHUB_STEP_SUMMARY")
    if output_file:
        with open(output_file, "a") as f:
            f.write(comment_body)
    
    # Also write to a local file so the next step can post it to the PR
    with open("pr_comment.md", "w") as f:
        f.write(comment_body)
        
    print("Generated PR comment markdown:")
    print(comment_body)

if __name__ == "__main__":
    main()
