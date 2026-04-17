package benchmark

import (
	"context"
	"fmt"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// CreateGuideLLMJobWithArgs launches a GuideLLM Job with the specified arguments.
func CreateGuideLLMJobWithArgs(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL, modelID string,
) error {
	image := "ghcr.io/vllm-project/guidellm:v0.5.4"

	args := []string{
		"benchmark",
		"--target", targetServiceURL,
		"--model", modelID,
		"--profile", "poisson",
		"--rate", "20",
		"--max-seconds", "600",
		"--random-seed", "42",
		"--request-type", "text_completions",
		"--data", "prompt_tokens=4000,output_tokens=1000",
		"--output-path", "/tmp/benchmarks.json",
		"--backend-kwargs", `'{"validate_backend": false}'`,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-load",
			Namespace: namespace,
			Labels: map[string]string{
				"app":           name + "-load",
				"test-resource": "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":           name + "-load",
						"test-resource": "true",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "load-gen",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c"},
							Args: []string{
								fmt.Sprintf("echo 'Waiting 30s for gateway routing to propagate...' && sleep 30 && guidellm %s && echo '=== BENCHMARK JSON ===' && cat /tmp/benchmarks.json", strings.Join(args, " ")),
							},
							Env: []corev1.EnvVar{
								{Name: "HF_HOME", Value: "/tmp"},
								{
									Name: "HF_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "llm-d-hf-token"},
											Key:                  "HF_TOKEN",
											Optional:             ptr.To(true),
										},
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, name+"-load", metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})

	_, createErr := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	return createErr
}

// ShareGPTBenchmarkConfig mirrors the YAML config file for the ShareGPT benchmark.
type ShareGPTBenchmarkConfig struct {
	Benchmark ShareGPTBenchmarkSpec `json:"benchmark"`
}

// ShareGPTBenchmarkSpec holds the GuideLLM parameters for a ShareGPT benchmark run.
type ShareGPTBenchmarkSpec struct {
	Image       string                   `json:"image"`
	Dataset     string                   `json:"dataset"`
	RequestType string                   `json:"request_type"`
	Profile     string                   `json:"profile"`
	Rate        int                      `json:"rate"`
	MaxSeconds  int                      `json:"max_seconds"`
	RandomSeed  int                      `json:"random_seed"`
	OutputPath  string                   `json:"output_path"`
	Resources   ShareGPTResourceRequests `json:"resources"`
}

// ShareGPTResourceRequests defines CPU and memory for the load-gen pod.
type ShareGPTResourceRequests struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// LoadShareGPTConfig reads and parses a ShareGPT benchmark YAML config file.
// Environment variable overrides: SHAREGPT_DATASET, SHAREGPT_RATE, SHAREGPT_MAX_SECONDS.
func LoadShareGPTConfig(path string) (*ShareGPTBenchmarkConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg ShareGPTBenchmarkConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if v := os.Getenv("SHAREGPT_DATASET"); v != "" {
		cfg.Benchmark.Dataset = v
	}
	if v := os.Getenv("SHAREGPT_REQUEST_TYPE"); v != "" {
		cfg.Benchmark.RequestType = v
	}
	if v := os.Getenv("SHAREGPT_RATE"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Benchmark.Rate)
	}
	if v := os.Getenv("SHAREGPT_MAX_SECONDS"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Benchmark.MaxSeconds)
	}

	return &cfg, nil
}

// CreateGuideLLMShareGPTJob launches a GuideLLM Job configured for the ShareGPT dataset.
func CreateGuideLLMShareGPTJob(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL, modelID string,
	cfg *ShareGPTBenchmarkConfig,
) error {
	spec := cfg.Benchmark

	dataArg := spec.Dataset
	downloadCmd := ""
	localDataPath := "/tmp/sharegpt_data.json"
	convertedPath := "/tmp/sharegpt_prompts.jsonl"
	if strings.HasPrefix(spec.Dataset, "http://") || strings.HasPrefix(spec.Dataset, "https://") {
		pyScript := "/tmp/convert_sharegpt.py"
		writeScript := fmt.Sprintf(
			`cat > %s << 'PYEOF'
import json, urllib.request, sys
urllib.request.urlretrieve(sys.argv[1], sys.argv[2])
with open(sys.argv[2]) as f:
    data = json.load(f)
count = 0
with open(sys.argv[3], 'w') as f:
    for item in data:
        convs = item.get('conversations', [])
        for turn in convs:
            if turn.get('from') == 'human' and turn.get('value', '').strip():
                f.write(json.dumps({'prompt': turn['value'].strip()}) + '\n')
                count += 1
                break
print(f'Converted {count} prompts to JSONL')
PYEOF`, pyScript)
		downloadCmd = fmt.Sprintf(
			"echo 'Downloading and converting ShareGPT dataset...' && %s\npython3 %s %s %s %s && ",
			writeScript, pyScript, spec.Dataset, localDataPath, convertedPath,
		)
		dataArg = convertedPath
	}

	args := []string{
		"benchmark",
		"--target", targetServiceURL,
		"--model", modelID,
		"--profile", spec.Profile,
		"--rate", fmt.Sprintf("%d", spec.Rate),
		"--max-seconds", fmt.Sprintf("%d", spec.MaxSeconds),
		"--random-seed", fmt.Sprintf("%d", spec.RandomSeed),
		"--request-type", spec.RequestType,
		"--data", dataArg,
		"--output-path", spec.OutputPath,
		"--backend-kwargs", `'{"validate_backend": false}'`,
	}

	cpuReq := spec.Resources.CPU
	if cpuReq == "" {
		cpuReq = "2"
	}
	memReq := spec.Resources.Memory
	if memReq == "" {
		memReq = "4Gi"
	}

	shellScript := fmt.Sprintf(
		"echo 'Waiting 30s for gateway routing to propagate...' && sleep 30 && %sguidellm %s && echo '=== BENCHMARK JSON ===' && cat %s",
		downloadCmd, strings.Join(args, " "), spec.OutputPath,
	)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-load",
			Namespace: namespace,
			Labels: map[string]string{
				"app":           name + "-load",
				"test-resource": "true",
				"benchmark":     "sharegpt",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":           name + "-load",
						"test-resource": "true",
						"benchmark":     "sharegpt",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "load-gen",
							Image:           spec.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c"},
							Args:            []string{shellScript},
							Env: []corev1.EnvVar{
								{Name: "HF_HOME", Value: "/tmp"},
								{
									Name: "HF_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "llm-d-hf-token"},
											Key:                  "HF_TOKEN",
											Optional:             ptr.To(true),
										},
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cpuReq),
									corev1.ResourceMemory: resource.MustParse(memReq),
								},
							},
						},
					},
				},
			},
		},
	}

	_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, name+"-load", metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})

	_, createErr := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	return createErr
}
