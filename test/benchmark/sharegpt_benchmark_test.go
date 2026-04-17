package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// ShareGPTResult holds results for one ShareGPT benchmark run.
type ShareGPTResult struct {
	AutoscalerType   string          `json:"autoscaler_type"`
	ModelID          string          `json:"model_id"`
	Dataset          string          `json:"dataset"`
	VAConfig         string          `json:"va_config"`
	HPAConfig        string          `json:"hpa_config"`
	Pods             []PodInfo       `json:"pods,omitempty"`
	ReplicaTimeline  []ReplicaSnap   `json:"replica_timeline"`
	MetricsTimeline  []MetricSnap    `json:"metrics_timeline"`
	AvgReplicas      float64         `json:"avg_replicas"`
	MaxReplicas      int32           `json:"max_replicas"`
	AvgQueueDepth    float64         `json:"avg_queue_depth"`
	AvgEPPQueueDepth float64         `json:"avg_epp_queue_depth"`
	AvgKVCache       float64         `json:"avg_kv_cache"`
	AchievedRPS      float64         `json:"achieved_rps"`
	ErrorCount       int             `json:"error_count"`
	IncompleteCount  int             `json:"incomplete_count"`
	TTFT             json.RawMessage `json:"ttft,omitempty"`
	ITL              json.RawMessage `json:"itl,omitempty"`
	Throughput       json.RawMessage `json:"throughput,omitempty"`
	GuideLLMRaw      json.RawMessage `json:"guidellm_raw,omitempty"`
	DurationSec      float64         `json:"duration_sec"`
}

var sharegptResults []ShareGPTResult

const sharegptResultsFile = "/tmp/sharegpt-benchmark-results.json"

// defaultShareGPTConfigPath resolves the config file relative to this source file.
func defaultShareGPTConfigPath() string {
	if v := os.Getenv("SHAREGPT_CONFIG"); v != "" {
		return v
	}
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "configs", "sharegpt_single_model.yaml")
}

var _ = Describe("ShareGPT Single-Model Benchmark", Ordered, Label("benchmark", "sharegpt"), func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		res    ScenarioResources
		sgCfg  *ShareGPTBenchmarkConfig
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // Ginkgo BeforeEach requires reassigning outer ctx
		res = ScenarioResources{
			PoolName:     benchCfg.PoolName,
			ModelService: "sharegpt-ms",
			VAName:       "sharegpt-va",
			HPAName:      "sharegpt-hpa",
			JobBaseName:  "sharegpt-ms",
		}

		var err error
		sgCfg, err = LoadShareGPTConfig(defaultShareGPTConfigPath())
		Expect(err).NotTo(HaveOccurred(), "Failed to load ShareGPT config")
		GinkgoWriter.Printf("ShareGPT config: dataset=%s profile=%s rate=%d max_seconds=%d\n",
			sgCfg.Benchmark.Dataset, sgCfg.Benchmark.Profile, sgCfg.Benchmark.Rate, sgCfg.Benchmark.MaxSeconds)
	})

	AfterEach(func() {
		cancel()
	})

	cleanupAutoscalers := func() {
		GinkgoWriter.Println("Cleaning up existing autoscalers...")
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, res.HPAName+"-standard-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, res.HPAName+"-hpa", metav1.DeleteOptions{})
		_ = fixtures.DeleteVariantAutoscaling(ctx, crClient, benchCfg.LLMDNamespace, res.VAName)
		time.Sleep(3 * time.Second)
	}

	findInfraDecodeDeployment := func() string {
		By("Finding decode deployment matching MODEL_ID for single-model benchmark")
		deployments, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

		// Normalize MODEL_ID to the slug format used in deployment names
		// e.g. "unsloth/Meta-Llama-3.1-8B" → "unsloth-meta-llama-3-1-8b"
		modelSlug := strings.ToLower(benchCfg.ModelID)
		modelSlug = strings.ReplaceAll(modelSlug, "/", "-")
		modelSlug = strings.ReplaceAll(modelSlug, ".", "-")

		// First pass: prefer a decode deployment that matches the configured model
		var fallback string
		for i := range deployments.Items {
			d := &deployments.Items[i]
			if !strings.HasSuffix(d.Name, "-decode") {
				continue
			}
			if strings.Contains(d.Name, modelSlug) {
				GinkgoWriter.Printf("  Found decode deployment matching model %s: %s\n", benchCfg.ModelID, d.Name)
				return d.Name
			}
			if fallback == "" {
				fallback = d.Name
			}
		}
		if fallback != "" {
			GinkgoWriter.Printf("  No exact model match; using fallback decode deployment: %s\n", fallback)
			return fallback
		}
		Fail("No decode deployment found in namespace " + benchCfg.LLMDNamespace)
		return ""
	}

	ensureInfraDeploymentReady := func() {
		By("Ensuring infra decode deployment is scaled to 1 and ready")
		one := int32(1)
		deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
			deployment.Spec.Replicas = &one
			_, err = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to scale infra deployment to 1")
			GinkgoWriter.Printf("  Scaled %s to 1 replica\n", res.DeploymentName)
		}

		By("Waiting for infra deployment to have at least 1 ready replica")
		Eventually(func(g Gomega) {
			d, getErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			spec := int32(0)
			if d.Spec.Replicas != nil {
				spec = *d.Spec.Replicas
			}
			GinkgoWriter.Printf("  %s: spec=%d, ready=%d\n", res.DeploymentName, spec, d.Status.ReadyReplicas)
			g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1), "Deployment should have at least 1 ready replica")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	}

	waitForVAAndMetrics := func() {
		By("Waiting for VA to stabilize (NumReplicas set)")
		Eventually(func(g Gomega) {
			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: benchCfg.LLMDNamespace,
				Name:      res.VAName,
			}, currentVA)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(currentVA.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(), "NumReplicas should be set")
			g.Expect(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1), "VA should have optimized >= 1")
			GinkgoWriter.Printf("VA status: desired replicas = %d\n", *currentVA.Status.DesiredOptimizedAlloc.NumReplicas)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Checking Prometheus vLLM metrics (best-effort, non-blocking)")
		promOK := false
		Eventually(func() bool {
			_, err := promClient.QueryWithRetry(ctx, `vllm:kv_cache_usage_perc`)
			if err == nil {
				GinkgoWriter.Println("Prometheus confirmed: vllm:kv_cache_usage_perc is available")
				promOK = true
				return true
			}
			return false
		}, 2*time.Minute, 15*time.Second).Should(Or(BeTrue(), Not(BeTrue())))
		if !promOK {
			GinkgoWriter.Println("WARNING: Prometheus vLLM metrics not yet available — KV cache data may be incomplete.")
		}
	}

	runShareGPTBenchmark := func(autoscalerType string) {
		ensureEPPConfig := func() {
			By("Patching EPP ConfigMap with flowControl + scorer weights")
			eppDeployName, findErr := FindEPPDeployment(ctx, k8sClient, benchCfg.LLMDNamespace)
			Expect(findErr).NotTo(HaveOccurred(), "Failed to find EPP deployment")
			patchErr := PatchEPPConfigMap(ctx, k8sClient, benchCfg.LLMDNamespace, eppDeployName)
			if patchErr != nil {
				GinkgoWriter.Printf("WARNING: EPP ConfigMap patch failed (non-fatal): %v\n", patchErr)
			} else {
				GinkgoWriter.Println("EPP ConfigMap patched successfully")
			}
		}

		ensureEPPConfig()
		ensureInfraDeploymentReady()

		gatewayURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
			benchCfg.GatewayServiceName, benchCfg.LLMDNamespace, benchCfg.GatewayServicePort)

		By("Verifying Gateway connectivity")
		Eventually(func(g Gomega) {
			err := VerifyGatewayConnectivity(ctx, k8sClient, benchCfg.LLMDNamespace, gatewayURL, benchCfg.ModelID)
			g.Expect(err).NotTo(HaveOccurred(), "Gateway not ready yet")
		}, 5*time.Minute, 15*time.Second).Should(Succeed(), "Gateway connectivity check failed")
		GinkgoWriter.Println("  Gateway connectivity verified")

		targetURL := gatewayURL
		GinkgoWriter.Printf("  Using Gateway URL: %s\n", targetURL)

		By("Launching GuideLLM ShareGPT Load Generator")
		err := CreateGuideLLMShareGPTJob(
			ctx, k8sClient, benchCfg.LLMDNamespace, res.ModelService,
			targetURL, benchCfg.ModelID, sgCfg,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create GuideLLM ShareGPT load job")

		loadStart := time.Now()
		jobName := res.ModelService + "-load"

		By("Monitoring replicas and metrics while GuideLLM runs")
		var timeline []ReplicaSnap
		var metricsTimeline []MetricSnap
		var maxReplicas int32 = 1
		done := make(chan error, 1)

		go func() {
			done <- WaitForJobCompletion(ctx, k8sClient, benchCfg.LLMDNamespace, jobName, 25*time.Minute)
		}()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

	monitorLoop:
		for {
			select {
			case jobErr := <-done:
				if jobErr != nil {
					logs, logErr := GetJobPodLogs(ctx, k8sClient, benchCfg.LLMDNamespace, jobName)
					if logErr == nil {
						GinkgoWriter.Printf("\n--- GuideLLM Job Failed. Pod Logs ---\n%s\n---------------------------\n", logs)
					}
				}
				Expect(jobErr).NotTo(HaveOccurred(), "GuideLLM job failed or timed out")
				break monitorLoop
			case <-ticker.C:
				elapsed := time.Since(loadStart).Seconds()
				var spec, ready int32
				deployment, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
				if depErr == nil {
					spec = *deployment.Spec.Replicas
					ready = deployment.Status.ReadyReplicas
					if spec > maxReplicas {
						maxReplicas = spec
					}
					timeline = append(timeline, ReplicaSnap{ElapsedSec: elapsed, SpecReplicas: spec, ReadyReplicas: ready})
				}

				qdQuery := fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s"})`, benchCfg.LLMDNamespace)
				kvQuery := fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s"})`, benchCfg.LLMDNamespace)
				eppQDQuery := fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s"})`, benchCfg.LLMDNamespace)
				snap := MetricSnap{ElapsedSec: elapsed}
				if qdResult, _, qdErr := promClient.API().Query(ctx, qdQuery, time.Now()); qdErr == nil {
					if vec, ok := qdResult.(model.Vector); ok && len(vec) > 0 {
						snap.QueueDepth = float64(vec[0].Value)
					}
				}
				if kvResult, _, kvErr := promClient.API().Query(ctx, kvQuery, time.Now()); kvErr == nil {
					if vec, ok := kvResult.(model.Vector); ok && len(vec) > 0 {
						snap.KVCache = float64(vec[0].Value)
					}
				}
				if eppResult, _, eppErr := promClient.API().Query(ctx, eppQDQuery, time.Now()); eppErr == nil {
					if vec, ok := eppResult.(model.Vector); ok && len(vec) > 0 {
						snap.EPPQueueDepth = float64(vec[0].Value)
					}
				}
				metricsTimeline = append(metricsTimeline, snap)

				vaDesired := "?"
				currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				if vaErr := crClient.Get(ctx, client.ObjectKey{Namespace: benchCfg.LLMDNamespace, Name: res.VAName}, currentVA); vaErr == nil {
					if currentVA.Status.DesiredOptimizedAlloc.NumReplicas != nil {
						vaDesired = strconv.FormatInt(int64(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas), 10)
					}
				}

				hpaCurrent, hpaDesired := "?", "?"
				hpaList, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
				if hpaErr == nil {
					for i := range hpaList.Items {
						hpa := &hpaList.Items[i]
						hpaCurrent = strconv.FormatInt(int64(hpa.Status.CurrentReplicas), 10)
						hpaDesired = strconv.FormatInt(int64(hpa.Status.DesiredReplicas), 10)
					}
				}

				GinkgoWriter.Printf("  [%s] replicas: spec=%d ready=%d | va=%s hpa=%s→%s | kv=%.4f qd=%.1f epp_qd=%.1f\n",
					fmt.Sprintf("%.0fs", elapsed), spec, ready, vaDesired, hpaCurrent, hpaDesired,
					snap.KVCache, snap.QueueDepth, snap.EPPQueueDepth)
			}
		}
		loadEnd := time.Now()
		loadDuration := loadEnd.Sub(loadStart).Seconds()

		By("Extracting GuideLLM results from pod logs")
		logs, err := GetJobPodLogs(ctx, k8sClient, benchCfg.LLMDNamespace, jobName)
		Expect(err).NotTo(HaveOccurred(), "Failed to get GuideLLM pod logs")

		var guidellmRaw json.RawMessage
		var ttftJSON, itlJSON, throughputJSON json.RawMessage

		if idx := strings.Index(logs, "=== BENCHMARK JSON ==="); idx != -1 {
			jsonStr := strings.TrimSpace(logs[idx+len("=== BENCHMARK JSON ==="):])
			guidellmRaw = json.RawMessage(jsonStr)

			var parsed map[string]interface{}
			if jsonErr := json.Unmarshal([]byte(jsonStr), &parsed); jsonErr == nil {
				extractGuideLLMMetric(&parsed, "time_to_first_token_ms", &ttftJSON)
				extractGuideLLMMetric(&parsed, "inter_token_latency_ms", &itlJSON)
				extractGuideLLMMetric(&parsed, "output_tokens_per_second", &throughputJSON)
			} else {
				GinkgoWriter.Printf("Warning: failed to parse GuideLLM JSON: %v\n", jsonErr)
			}
		} else {
			GinkgoWriter.Println("Warning: '=== BENCHMARK JSON ===' marker not found in pod logs")
			GinkgoWriter.Printf("Pod log tail (last 500 chars): %s\n", truncateTail(logs, 500))
		}

		By("Querying Prometheus for aggregate metrics")
		replicaAvg, _ := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`avg(kube_deployment_status_replicas{deployment="%s", namespace="%s"})`, res.DeploymentName, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		qdAvg, _ := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s"})`, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		kvAvg, _ := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s"})`, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		eppQDAvg, _ := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s"})`, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)

		By("Collecting pod placement details")
		var podInfos []PodInfo
		var decodePods *corev1.PodList
		var podListErr error
		for _, sel := range []string{
			"llm-d.ai/role=decode",
			"app=" + res.DeploymentName,
		} {
			decodePods, podListErr = k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: sel,
			})
			if podListErr == nil && len(decodePods.Items) > 0 {
				break
			}
		}
		if podListErr == nil && decodePods != nil {
			for i := range decodePods.Items {
				p := &decodePods.Items[i]
				gpu := "Unknown"
				if p.Spec.NodeName != "" {
					node, nodeErr := k8sClient.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{})
					if nodeErr == nil {
						if g, ok := node.Labels["nvidia.com/gpu.product"]; ok {
							gpu = g
						} else if g, ok := node.Labels["accelerator"]; ok {
							gpu = g
						}
					}
				}
				var startupSec float64
				for _, cond := range p.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						startupSec = cond.LastTransitionTime.Sub(p.CreationTimestamp.Time).Seconds()
						break
					}
				}
				podInfos = append(podInfos, PodInfo{
					Name:       p.Name,
					Node:       p.Spec.NodeName,
					GPU:        gpu,
					StartupSec: startupSec,
				})
			}
		}

		vaConfig := "Min Replicas: 1, Max Replicas: 10, Cost Factor: 10.0"
		hpaConfig := "Min Replicas: 1, Max Replicas: 10 | Scale Up: stabilizationWindow=0s, policy=10 Pods/150s | Scale Down: stabilizationWindow=240s, policy=10 Pods/150s"

		var errorCount, incompleteCount, completedCount int
		var achievedRPS float64
		if guidellmRaw != nil {
			var parsed map[string]interface{}
			if jsonErr := json.Unmarshal(guidellmRaw, &parsed); jsonErr == nil {
				if benchmarks, ok := parsed["benchmarks"].([]interface{}); ok && len(benchmarks) > 0 {
					if bm, ok := benchmarks[0].(map[string]interface{}); ok {
						if metrics, ok := bm["metrics"].(map[string]interface{}); ok {
							if rt, ok := metrics["request_totals"].(map[string]interface{}); ok {
								if f, ok := rt["errored"].(float64); ok {
									errorCount = int(f)
								}
								if f, ok := rt["incomplete"].(float64); ok {
									incompleteCount = int(f)
								}
								if f, ok := rt["successful"].(float64); ok {
									completedCount = int(f)
								}
							}
						}
						if rateObj, ok := bm["rate"].(map[string]interface{}); ok {
							if f, ok := rateObj["completed_rate"].(float64); ok {
								achievedRPS = f
							}
						}
					}
				}
			}
		}
		if achievedRPS == 0 && completedCount > 0 && loadDuration > 0 {
			achievedRPS = float64(completedCount) / loadDuration
		}

		result := ShareGPTResult{
			AutoscalerType:   autoscalerType,
			ModelID:          benchCfg.ModelID,
			Dataset:          sgCfg.Benchmark.Dataset,
			VAConfig:         vaConfig,
			HPAConfig:        hpaConfig,
			Pods:             podInfos,
			ReplicaTimeline:  timeline,
			MetricsTimeline:  metricsTimeline,
			AvgReplicas:      replicaAvg,
			MaxReplicas:      maxReplicas,
			AvgQueueDepth:    qdAvg,
			AvgEPPQueueDepth: eppQDAvg,
			AvgKVCache:       kvAvg,
			AchievedRPS:      achievedRPS,
			ErrorCount:       errorCount,
			IncompleteCount:  incompleteCount,
			TTFT:             ttftJSON,
			ITL:              itlJSON,
			Throughput:       throughputJSON,
			GuideLLMRaw:      guidellmRaw,
			DurationSec:      loadDuration,
		}
		sharegptResults = append(sharegptResults, result)

		formatPercentiles := func(raw json.RawMessage) string {
			if raw == nil {
				return "n/a"
			}
			var m map[string]interface{}
			if err := json.Unmarshal(raw, &m); err != nil {
				return string(raw)
			}
			p50, _ := m["p50"].(float64)
			p90, _ := m["p90"].(float64)
			p99, _ := m["p99"].(float64)
			if p50 == 0 && p90 == 0 && p99 == 0 {
				return string(raw)
			}
			return fmt.Sprintf("p50=%.1f p90=%.1f p99=%.1f", p50, p90, p99)
		}

		GinkgoWriter.Printf("\n  ┌────────────────────────────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ %s SHAREGPT BENCHMARK RESULTS\n", autoscalerType)
		GinkgoWriter.Printf("  │ Model: %s  Dataset: %s\n", benchCfg.ModelID, sgCfg.Benchmark.Dataset)
		GinkgoWriter.Printf("  ├────────────────────────────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ Duration:        %.0fs\n", loadDuration)
		GinkgoWriter.Printf("  │ Max Replicas:    %d\n", maxReplicas)
		GinkgoWriter.Printf("  │ Avg Replicas:    %.2f\n", replicaAvg)
		GinkgoWriter.Printf("  ├── Prometheus Metrics ──────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ Avg KV Cache:    %.4f\n", kvAvg)
		GinkgoWriter.Printf("  │ Avg Queue Depth: %.2f\n", qdAvg)
		GinkgoWriter.Printf("  │ Avg EPP Queue:   %.2f\n", eppQDAvg)
		GinkgoWriter.Printf("  ├── GuideLLM Results ────────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ Achieved RPS:    %.2f\n", achievedRPS)
		GinkgoWriter.Printf("  │ TTFT (ms):       %s\n", formatPercentiles(ttftJSON))
		GinkgoWriter.Printf("  │ ITL (ms):        %s\n", formatPercentiles(itlJSON))
		GinkgoWriter.Printf("  │ Throughput:      %s\n", formatPercentiles(throughputJSON))
		GinkgoWriter.Printf("  │ Errors:          %d\n", errorCount)
		GinkgoWriter.Printf("  │ Incomplete:      %d\n", incompleteCount)
		GinkgoWriter.Printf("  ├── Replica Timeline (%d snapshots) ─────────────────────────\n", len(timeline))
		for _, s := range timeline {
			GinkgoWriter.Printf("  │   t=%.0fs  spec=%d  ready=%d\n", s.ElapsedSec, s.SpecReplicas, s.ReadyReplicas)
		}
		GinkgoWriter.Printf("  └────────────────────────────────────────────────────────────\n\n")

		By("Saving ShareGPT benchmark results to file")
		data, _ := json.MarshalIndent(sharegptResults, "", "  ")
		_ = os.WriteFile(sharegptResultsFile, data, 0644)
	}

	Context("WVA", func() {
		It("should run the ShareGPT workload against WVA", func() {
			cleanupAutoscalers()
			res.DeploymentName = findInfraDecodeDeployment()
			ensureInfraDeploymentReady()

			By("Creating VariantAutoscaling resource (max=10, cost=10)")
			err := fixtures.EnsureVariantAutoscaling(
				ctx, crClient, benchCfg.LLMDNamespace, res.VAName, res.DeploymentName,
				benchCfg.ModelID, benchCfg.AcceleratorType, 10.0, benchCfg.ControllerInstance,
				fixtures.WithMinReplicas(1),
				fixtures.WithMaxReplicas(10),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VA")

			By("Creating HPA (Scale Up: 0s/Pods/10/150, Scale Down: 240s/Pods/10/150)")
			behavior := &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 150}},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(240)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 150}},
				},
			}
			err = fixtures.EnsureHPA(ctx, k8sClient, benchCfg.LLMDNamespace, res.HPAName, res.DeploymentName, res.VAName, 1, 10, WithBehavior(behavior))
			Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")

			waitForVAAndMetrics()
			runShareGPTBenchmark("WVA")
		})
	})

	AfterAll(func() {
		GinkgoWriter.Println("ShareGPT benchmark complete — cleaning up")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()

		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "sharegpt-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "sharegpt-hpa-standard-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "sharegpt-hpa-hpa", metav1.DeleteOptions{})
		_ = fixtures.DeleteVariantAutoscaling(cleanupCtx, crClient, benchCfg.LLMDNamespace, "sharegpt-va")

		deployments, listErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(cleanupCtx, metav1.ListOptions{})
		if listErr == nil {
			for i := range deployments.Items {
				d := &deployments.Items[i]
				if strings.HasSuffix(d.Name, "-decode") && strings.Contains(d.Name, "modelservice") {
					scale, scaleErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).GetScale(cleanupCtx, d.Name, metav1.GetOptions{})
					if scaleErr == nil && scale.Spec.Replicas > 1 {
						scale.Spec.Replicas = 1
						_, _ = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).UpdateScale(cleanupCtx, d.Name, scale, metav1.UpdateOptions{})
						GinkgoWriter.Printf("Scaled %s back to 1 for next test suite\n", d.Name)
					}
				}
			}
		}
	})
})
