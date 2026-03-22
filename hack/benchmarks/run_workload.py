#!/usr/bin/env python3
import yaml
import sys
import time
import os
import subprocess

def parse_duration(duration_str):
    if duration_str.endswith('m'):
        return int(duration_str[:-1]) * 60
    elif duration_str.endswith('s'):
        return int(duration_str[:-1])
    return int(duration_str)

def main():
    if len(sys.argv) < 2:
        print("Usage: run_workload.py <scenario.yaml>")
        sys.exit(1)
        
    scenario_file = sys.argv[1]
    with open(scenario_file, 'r') as f:
        scenario = yaml.safe_load(f)
        
    print(f"Running scenario: {scenario['name']}")
    print(f"Description: {scenario.get('description', '')}")
    
    # Needs TARGET_URL and MODEL_ID from environment
    target_url = os.environ.get("TARGET_URL", "http://localhost:8000/v1/chat/completions")
    model_id = os.environ.get("MODEL_ID", "unsloth/Meta-Llama-3.1-8B")
    
    burst_script = os.path.join(os.path.dirname(__file__), "..", "burst_load_generator.sh")
    
    for phase in scenario.get('phases', []):
        name = phase['name']
        duration_sec = parse_duration(phase['duration'])
        rate = phase.get('load', {}).get('rate', 0)
        
        print(f"\n--- Phase: {name} ---")
        print(f"Duration: {duration_sec}s, Rate: {rate} req/s")
        
        if rate == 0:
            print(f"Sleeping for {duration_sec} seconds...")
            time.sleep(duration_sec)
        else:
            total_requests = rate * duration_sec
            batch_size = rate
            batch_sleep = 1
            
            env = os.environ.copy()
            env["TOTAL_REQUESTS"] = str(total_requests)
            env["BATCH_SIZE"] = str(batch_size)
            env["BATCH_SLEEP"] = str(batch_sleep)
            env["TARGET_URL"] = target_url
            env["MODEL_ID"] = model_id
            
            print(f"Starting burst_load_generator.sh with {total_requests} total requests, batch size {batch_size}")
            
            # Run the bash script
            try:
                subprocess.run([burst_script], env=env, check=True)
            except subprocess.CalledProcessError as e:
                print(f"Error running load generator: {e}")
                sys.exit(1)
                
    print("\nWorkload complete!")

if __name__ == "__main__":
    main()
