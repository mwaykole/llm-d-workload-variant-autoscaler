package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// PodInfo records pod placement and startup details for the report.
type PodInfo struct {
	Name       string  `json:"name"`
	Node       string  `json:"node"`
	GPU        string  `json:"gpu"`
	StartupSec float64 `json:"startup_sec"`
}

// PrefillResult holds results for one prefill benchmark run (HPA or WVA).
type PrefillResult struct {
	AutoscalerType   string          `json:"autoscaler_type"`
	ModelID          string          `json:"model_id"`
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

// ReplicaSnap records replica count at a point in time.
type ReplicaSnap struct {
	ElapsedSec    float64 `json:"elapsed_sec"`
	SpecReplicas  int32   `json:"spec_replicas"`
	ReadyReplicas int32   `json:"ready_replicas"`
}

// MetricSnap records KV cache and queue depth at a point in time.
type MetricSnap struct {
	ElapsedSec    float64 `json:"elapsed_sec"`
	QueueDepth    float64 `json:"queue_depth"`
	EPPQueueDepth float64 `json:"epp_queue_depth"`
	KVCache       float64 `json:"kv_cache"`
}

var prefillResults []PrefillResult

const prefillResultsFile = "/tmp/prefill-benchmark-results.json"

var _ = Describe("Prefill Heavy Workload Benchmark", Ordered, Label("benchmark", "phase3a"), func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		res    ScenarioResources
	)

	var modelsList []string

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // Ginkgo BeforeEach requires reassigning outer ctx
		res = ScenarioResources{
			PoolName:     benchCfg.PoolName,
			ModelService: "prefill-ms",
			VAName:       "prefill-va",
			HPAName:      "prefill-hpa",
			JobBaseName:  "prefill-ms",
		}

		ml := os.Getenv("MODELS_LIST")
		if ml != "" {
			modelsList = strings.Split(ml, ",")
		} else {
			modelsList = []string{benchCfg.ModelID}
		}
	})

	AfterEach(func() {
		cancel()
	})

	// cleanupAutoscalers removes leftover HPAs and VAs from previous tests to avoid conflicts.
	cleanupAutoscalers := func() {
		GinkgoWriter.Println("Cleaning up existing autoscalers...")
		for _, m := range modelsList {
			safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
			hpaName := res.HPAName + "-" + safePostfix
			vaName := res.VAName + "-" + safePostfix
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, hpaName+"-standard-hpa", metav1.DeleteOptions{})
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
			_ = fixtures.DeleteVariantAutoscaling(ctx, crClient, benchCfg.LLMDNamespace, vaName)
		}
		time.Sleep(3 * time.Second)
	}

	// findInfraDecodeDeployments discovers the Helm-deployed decode deployments.
	// We reuse these deployments instead of creating new ones.
	findInfraDecodeDeployments := func() map[string]string {
		By("Finding Helm-deployed decode deployments for Gateway-compatible routing")
		deployments, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

		modelDeployMap := make(map[string]string)
		for _, m := range modelsList {
			safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
			found := false
			for i := range deployments.Items {
				d := &deployments.Items[i]
				// If strictly multi-model, we check safe postfix
				if strings.HasSuffix(d.Name, "-decode") && strings.Contains(d.Name, safePostfix) {
					GinkgoWriter.Printf("  Found infra decode deployment for %s: %s\n", m, d.Name)
					modelDeployMap[m] = d.Name
					found = true
					break
				}
			}
			if !found && len(modelsList) == 1 {
				// Fallback to legacy single-model string match
				for i := range deployments.Items {
					d := &deployments.Items[i]
					if strings.HasSuffix(d.Name, "-decode") && strings.Contains(d.Name, "modelservice") {
						GinkgoWriter.Printf("  Found fallback core infra decode deployment: %s\n", d.Name)
						modelDeployMap[m] = d.Name
						found = true
						break
					}
				}
			}
			if !found {
				Fail("No Helm-deployed decode deployment found in namespace " + benchCfg.LLMDNamespace + " for model " + m)
			}
		}
		return modelDeployMap
	}

	// ensureInfraDeploymentsReady scales the Helm-deployed model services to 1 replica and waits for readiness.
	ensureInfraDeploymentsReady := func(deployMap map[string]string) {
		By("Ensuring infra decode deployments are scaled to 1 and ready")
		one := int32(1)

		for _, depName := range deployMap {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, depName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
				deployment.Spec.Replicas = &one
				_, err = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred(), "Failed to scale infra deployment to 1")
				GinkgoWriter.Printf("  Scaled %s to 1 replica\n", depName)
			}
		}

		By("Waiting for infra deployments to have at least 1 ready replica")
		Eventually(func(g Gomega) {
			for _, depName := range deployMap {
				d, getErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, depName, metav1.GetOptions{})
				g.Expect(getErr).NotTo(HaveOccurred())
				spec := int32(0)
				if d.Spec.Replicas != nil {
					spec = *d.Spec.Replicas
				}
				GinkgoWriter.Printf("  %s: spec=%d, ready=%d\n", depName, spec, d.Status.ReadyReplicas)
				g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1), "Deployment should have at least 1 ready replica")
			}
		}, 6*time.Minute, 10*time.Second).Should(Succeed())
	}

	// dumpExternalMetricsDiagnostics logs the state of pods serving the external metrics API.
	dumpExternalMetricsDiagnostics := func() {
		GinkgoWriter.Println("--- External Metrics API Diagnostics ---")

		// Check APIService health
		result, err := k8sClient.RESTClient().
			Get().
			AbsPath("/apis/external.metrics.k8s.io/v1beta1").
			DoRaw(ctx)
		if err != nil {
			GinkgoWriter.Printf("  external.metrics.k8s.io/v1beta1 discovery: ERROR %v\n", err)
		} else {
			GinkgoWriter.Printf("  external.metrics.k8s.io/v1beta1 discovery: OK (%d bytes)\n", len(result))
		}

		// Check prometheus-adapter pods across common namespaces
		for _, ns := range []string{benchCfg.WVANamespace, benchCfg.LLMDNamespace, "kube-system", "monitoring", "custom-metrics"} {
			pods, podErr := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if podErr != nil {
				continue
			}
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "prometheus-adapter") || strings.Contains(p.Name, "custom-metrics") || strings.Contains(p.Name, "metrics-server") {
					phase := string(p.Status.Phase)
					ready := false
					restarts := int32(0)
					for _, c := range p.Status.ContainerStatuses {
						if c.Ready {
							ready = true
						}
						restarts += c.RestartCount
					}
					GinkgoWriter.Printf("  [%s] %s: phase=%s ready=%v restarts=%d\n", ns, p.Name, phase, ready, restarts)
				}
			}
		}
		GinkgoWriter.Println("--- End External Metrics Diagnostics ---")
	}

	// waitForVAAndMetrics waits for the VA to stabilize. External metrics and Prometheus
	// checks are best-effort warnings — the benchmark proceeds even if they fail, since
	// the prometheus-adapter may be transiently unavailable.
	waitForVAsAndMetrics := func() {
		By("Waiting for VAs to stabilize natively across all targets (NumReplicas >= 1)")
		Eventually(func(g Gomega) {
			for _, m := range modelsList {
				safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
				vaName := res.VAName + "-" + safePostfix
				currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Namespace: benchCfg.LLMDNamespace,
					Name:      vaName,
				}, currentVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(currentVA.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(), "NumReplicas should be set for "+vaName)
				g.Expect(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1), "VA should have optimized >= 1 for "+vaName)
				GinkgoWriter.Printf("VA status %s: desired replicas = %d\n", vaName, *currentVA.Status.DesiredOptimizedAlloc.NumReplicas)
			}
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Checking external metrics API (best-effort, non-blocking)")
		externalMetricsOK := false
		Eventually(func() bool {
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + benchCfg.LLMDNamespace + "/wva_desired_replicas").
				DoRaw(ctx)
			if err != nil {
				GinkgoWriter.Printf("  External metrics API check: %v\n", err)
				return false
			}
			s := string(result)
			// check if wva_desired_replicas serves results for ALL targets
			allFound := true
			for _, m := range modelsList {
				safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
				vaName := res.VAName + "-" + safePostfix
				if !(strings.Contains(s, "wva_desired_replicas") && strings.Contains(s, vaName)) {
					allFound = false
					GinkgoWriter.Printf("  External metrics API responded but metric not found for %s\n", vaName)
				}
			}
			if allFound {
				GinkgoWriter.Printf("External metrics API confirmed: wva_desired_replicas available for all VAs\n")
				externalMetricsOK = true
				return true
			}
			return false
		}, 3*time.Minute, 10*time.Second).Should(Or(BeTrue(), Not(BeTrue())))
		if !externalMetricsOK {
			GinkgoWriter.Println("WARNING: External metrics API not available — HPA may not scale. Proceeding with benchmark.")
			dumpExternalMetricsDiagnostics()
		}

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

	// dumpInfrastructureDiagnostics captures EPP, InferencePool, InferenceModel, HTTPRoute
	// state for debugging Gateway 500 errors.
	dumpInfrastructureDiagnostics := func() {
		By("Dumping infrastructure diagnostics for Gateway debugging")

		GinkgoWriter.Println("--- EPP Pod Status ---")
		pods, err := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "epp") || strings.Contains(p.Name, "inference-scheduler") {
					phase := string(p.Status.Phase)
					ready := false
					for _, c := range p.Status.ContainerStatuses {
						if c.Ready {
							ready = true
						}
					}
					GinkgoWriter.Printf("  %s: phase=%s ready=%v restarts=%d\n", p.Name, phase, ready, func() int32 {
						for _, c := range p.Status.ContainerStatuses {
							return c.RestartCount
						}
						return 0
					}())
				}
			}
		}

		GinkgoWriter.Println("--- All Services (all ports) ---")
		svcs, svcErr := k8sClient.CoreV1().Services(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if svcErr == nil {
			for i := range svcs.Items {
				s := &svcs.Items[i]
				ports := make([]string, 0, len(s.Spec.Ports))
				for _, port := range s.Spec.Ports {
					ports = append(ports, fmt.Sprintf("%s:%d→%s", port.Name, port.Port, port.TargetPort.String()))
				}
				GinkgoWriter.Printf("  svc/%s  type=%s  ports=[%s]  selector=%v\n",
					s.Name, s.Spec.Type, strings.Join(ports, ", "), s.Spec.Selector)
			}
		}

		GinkgoWriter.Println("--- All Deployments ---")
		deps, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if depErr == nil {
			for i := range deps.Items {
				d := &deps.Items[i]
				spec := int32(0)
				if d.Spec.Replicas != nil {
					spec = *d.Spec.Replicas
				}
				GinkgoWriter.Printf("  deploy/%s  spec=%d  ready=%d  selector=%v\n",
					d.Name, spec, d.Status.ReadyReplicas, d.Spec.Selector.MatchLabels)
			}
		}

		GinkgoWriter.Println("--- EPP Pod Logs (last 50 lines) ---")
		if pods, pErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{}); pErr == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "epp") || strings.Contains(p.Name, "inference-scheduler") {
					tailLines := int64(50)
					logOpts := &corev1.PodLogOptions{TailLines: &tailLines}
					logReq := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).GetLogs(p.Name, logOpts)
					logBytes, logErr := logReq.DoRaw(ctx)
					if logErr != nil {
						GinkgoWriter.Printf("  [%s] failed to get logs: %v\n", p.Name, logErr)
					} else {
						GinkgoWriter.Printf("  [%s] logs:\n%s\n", p.Name, string(logBytes))
					}
				}
			}
		}

		GinkgoWriter.Println("--- Gateway Pod Logs (last 30 lines) ---")
		if pods, pErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{}); pErr == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "gateway") {
					tailLines := int64(30)
					logOpts := &corev1.PodLogOptions{TailLines: &tailLines}
					logReq := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).GetLogs(p.Name, logOpts)
					logBytes, logErr := logReq.DoRaw(ctx)
					if logErr != nil {
						GinkgoWriter.Printf("  [%s] failed to get logs: %v\n", p.Name, logErr)
					} else {
						GinkgoWriter.Printf("  [%s] logs:\n%s\n", p.Name, string(logBytes))
					}
				}
			}
		}

		GinkgoWriter.Println("--- InferencePool / InferenceModel (via unstructured) ---")
		if crClient != nil {
			poolList := &unstructured.UnstructuredList{}
			poolList.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "inference.networking.k8s.io", Version: "v1", Kind: "InferencePoolList",
			})
			if err := crClient.List(ctx, poolList, client.InNamespace(benchCfg.LLMDNamespace)); err == nil {
				for _, item := range poolList.Items {
					data, _ := json.MarshalIndent(item.Object, "  ", "  ")
					GinkgoWriter.Printf("  InferencePool/%s (v1):\n  %s\n", item.GetName(), string(data))
				}
			} else {
				GinkgoWriter.Printf("  Failed to list InferencePool (v1): %v\n", err)
				poolList.SetGroupVersionKind(schema.GroupVersionKind{
					Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Kind: "InferencePoolList",
				})
				if err2 := crClient.List(ctx, poolList, client.InNamespace(benchCfg.LLMDNamespace)); err2 == nil {
					for _, item := range poolList.Items {
						data, _ := json.MarshalIndent(item.Object, "  ", "  ")
						GinkgoWriter.Printf("  InferencePool/%s (v1alpha2):\n  %s\n", item.GetName(), string(data))
					}
				} else {
					GinkgoWriter.Printf("  Failed to list InferencePool (v1alpha2): %v\n", err2)
				}
			}

			modelList := &unstructured.UnstructuredList{}
			modelList.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Kind: "InferenceModelList",
			})
			if err := crClient.List(ctx, modelList, client.InNamespace(benchCfg.LLMDNamespace)); err == nil {
				for _, item := range modelList.Items {
					data, _ := json.MarshalIndent(item.Object, "  ", "  ")
					GinkgoWriter.Printf("  InferenceModel/%s:\n  %s\n", item.GetName(), string(data))
				}
			} else {
				GinkgoWriter.Printf("  Failed to list InferenceModel (v1alpha2): %v\n", err)
			}
		}

		GinkgoWriter.Println("--- End Diagnostics ---")
	}

	eppPatched := false
	ensureEPPConfig := func() {
		if eppPatched {
			return
		}
		By("Patching EPP ConfigMap with flowControl + scorer weights (queue=2, kv-cache=2, prefix-cache=3)")
		eppDeployName, findErr := FindEPPDeployment(ctx, k8sClient, benchCfg.LLMDNamespace)
		Expect(findErr).NotTo(HaveOccurred(), "Failed to find EPP deployment")
		patchErr := PatchEPPConfigMap(ctx, k8sClient, benchCfg.LLMDNamespace, eppDeployName)
		if patchErr != nil {
			GinkgoWriter.Printf("WARNING: EPP ConfigMap patch failed (non-fatal): %v\n", patchErr)
		} else {
			GinkgoWriter.Println("EPP ConfigMap patched successfully — flowControl enabled, weights 2/2/3")
			eppPatched = true
		}
	}

	verifyEPPConfig := func() {
		By("Discovering EPP deployment")
		eppDeployName, findErr := FindEPPDeployment(ctx, k8sClient, benchCfg.LLMDNamespace)
		Expect(findErr).NotTo(HaveOccurred(), "Failed to find EPP deployment")
		GinkgoWriter.Printf("  Found EPP deployment: %s\n", eppDeployName)

		dep, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, eppDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to get EPP deployment")

		c := dep.Spec.Template.Spec.Containers[0]
		GinkgoWriter.Printf("  EPP image: %s\n", c.Image)
		GinkgoWriter.Printf("  EPP args: %v\n", c.Args)

		flowControlEnabled := false
		for _, e := range c.Env {
			if e.Name == "ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER" && e.Value == "true" {
				flowControlEnabled = true
			}
			GinkgoWriter.Printf("  EPP env: %s=%s\n", e.Name, e.Value)
		}

		if flowControlEnabled {
			GinkgoWriter.Println("  Flow control: ENABLED (via env var)")
		} else {
			GinkgoWriter.Println("  WARNING: Flow control env var not found — EPP queue metrics may be zero")
		}

		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.ConfigMap != nil {
				cm, cmErr := k8sClient.CoreV1().ConfigMaps(benchCfg.LLMDNamespace).Get(ctx, v.ConfigMap.Name, metav1.GetOptions{})
				if cmErr == nil {
					for key, val := range cm.Data {
						GinkgoWriter.Printf("  EPP ConfigMap %s/%s:\n%s\n", v.ConfigMap.Name, key, val)
					}
				}
			}
		}
	}

	runPrefillBenchmark := func(autoscalerType string, deployMap map[string]string) {
		ensureEPPConfig()
		verifyEPPConfig()
		dumpInfrastructureDiagnostics()

		gatewayURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
			benchCfg.GatewayServiceName, benchCfg.LLMDNamespace, benchCfg.GatewayServicePort)

		By("Verifying Gateway connectivity for ALL models")
		Eventually(func(g Gomega) {
			for _, m := range modelsList {
				targetURL := gatewayURL
				if len(modelsList) > 1 {
					safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
					targetURL = gatewayURL + "/" + safePostfix
				}
				err := VerifyGatewayConnectivity(ctx, k8sClient, benchCfg.LLMDNamespace, targetURL, m)
				g.Expect(err).NotTo(HaveOccurred(), "Gateway not ready yet for %s", m)
			}
		}, 6*time.Minute, 15*time.Second).Should(Succeed(), "Gateway connectivity check failed")
		GinkgoWriter.Println("  Gateway connectivity verified for all multi-model routes")

		targetURL := gatewayURL
		GinkgoWriter.Printf("  Using Gateway URL (traffic flows through EPP): %s\n", targetURL)

		By("Checking Prometheus metric availability before load")
		for _, m := range modelsList {
			depName := deployMap[m]
			for _, q := range []string{
				fmt.Sprintf(`vllm:kv_cache_usage_perc{namespace="%s"}`, benchCfg.LLMDNamespace), // TODO labels tracking model
				fmt.Sprintf(`inference_extension_flow_control_queue_size{namespace="%s"}`, benchCfg.LLMDNamespace),
				fmt.Sprintf(`kube_deployment_status_replicas{deployment="%s",namespace="%s"}`, depName, benchCfg.LLMDNamespace),
			} {
				val, err := QueryRangeAvg(promClient.API(), q, time.Now().Add(-2*time.Minute), time.Now(), 30*time.Second)
				if err != nil {
					GinkgoWriter.Printf("  Metric check %s: %s → NOT FOUND (%v)\n", m, q, err)
				} else {
					GinkgoWriter.Printf("  Metric check %s: %s → %.4f\n", m, q, val)
				}
			}
		}

		By("Checking HPA status before load")
		hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range hpaList.Items {
				hpa := &hpaList.Items[i]
				GinkgoWriter.Printf("  HPA %s: currentReplicas=%d desiredReplicas=%d\n", hpa.Name, hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas)
				for _, cond := range hpa.Status.Conditions {
					GinkgoWriter.Printf("    condition %s: %s (%s)\n", cond.Type, cond.Status, cond.Message)
				}
			}
		}

		By("Launching GuideLLM Load Generator for All Models")
		var jobNames []string

		for _, m := range modelsList {
			safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
			targetURL := gatewayURL
			if len(modelsList) > 1 {
				targetURL = gatewayURL + "/" + safePostfix + "/v1"
			} else {
				targetURL = gatewayURL + "/v1"
			}
			// Create a distinct job name for this target
			jobName := res.ModelService + "-" + safePostfix + "-load"
			jobNames = append(jobNames, jobName)

			GinkgoWriter.Printf("  Dispatching load job %s to %s for model %s\n", jobName, targetURL, m)

			err = CreateGuideLLMJobWithArgs(
				ctx, k8sClient, benchCfg.LLMDNamespace, res.ModelService+"-"+safePostfix,
				targetURL, m,
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create GuideLLM load job for "+m)
		}

		loadStart := time.Now()

		By("Monitoring replicas and HPA status while GuideLLM runs (~10 min)")
		var timelineMap = make(map[string][]ReplicaSnap)
		var metricsTimelineMap = make(map[string][]MetricSnap)
		var maxReplicasMap = make(map[string]int32)
		for _, m := range modelsList {
			maxReplicasMap[m] = 1
		}
		done := make(chan error, 1)

		go func() {
			var firstErr error
			for _, jn := range jobNames {
				if err := WaitForJobCompletion(ctx, k8sClient, benchCfg.LLMDNamespace, jn, 25*time.Minute); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			done <- firstErr
		}()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

	monitorLoop:
		for {
			select {
			case jobErr := <-done:
				if jobErr != nil {
					for _, jn := range jobNames {
						logs, logErr := GetJobPodLogs(ctx, k8sClient, benchCfg.LLMDNamespace, jn)
						if logErr == nil && logs != "" {
							GinkgoWriter.Printf("\n--- GuideLLM Job %s Failed. Pod Logs ---\n%s\n---------------------------\n", jn, logs)
						}
					}
				}
				Expect(jobErr).NotTo(HaveOccurred(), "GuideLLM job failed or timed out")
				break monitorLoop
			case <-ticker.C:
				elapsed := time.Since(loadStart).Seconds()

				// Monitor all deployments explicitly
				for _, m := range modelsList {
					depName := deployMap[m]
					safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
					eppDepName := "gaie-" + safePostfix + "-epp"

					deployment, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, depName, metav1.GetOptions{})
					if depErr == nil {
						spec := *deployment.Spec.Replicas
						ready := deployment.Status.ReadyReplicas
						if spec > maxReplicasMap[m] {
							maxReplicasMap[m] = spec
						}
						timelineMap[m] = append(timelineMap[m], ReplicaSnap{ElapsedSec: elapsed, SpecReplicas: spec, ReadyReplicas: ready})
						GinkgoWriter.Printf("  [%.0fs] [%s] %s replicas: spec=%d ready=%d\n", elapsed, m, depName, spec, ready)
					}

					qdQuery := fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s", pod=~"%s-.*"})`, benchCfg.LLMDNamespace, depName)
					kvQuery := fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s", pod=~"%s-.*"})`, benchCfg.LLMDNamespace, depName)
					eppQDQuery := fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s", pod=~"%s-.*"})`, benchCfg.LLMDNamespace, eppDepName)

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
					metricsTimelineMap[m] = append(metricsTimelineMap[m], snap)
					GinkgoWriter.Printf("  [%.0fs] [%s] queue_depth=%.1f epp_queue=%.1f kv_cache=%.3f\n", elapsed, m, snap.QueueDepth, snap.EPPQueueDepth, snap.KVCache)
				}

				// Pod-level health check for crash detection
				var allPods []corev1.Pod
				for _, depName := range deployMap {
					pods, podErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
						LabelSelector: "app=" + depName,
					})
					if podErr == nil {
						allPods = append(allPods, pods.Items...)
					}
				}
				for _, p := range allPods {
					for _, cs := range p.Status.ContainerStatuses {
						if cs.RestartCount > 0 {
							reason := "running"
							if cs.State.Waiting != nil {
								reason = cs.State.Waiting.Reason
							} else if cs.State.Terminated != nil {
								reason = fmt.Sprintf("terminated(%s,exit=%d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
							}
							GinkgoWriter.Printf("  [%.0fs] Pod %s: restarts=%d state=%s\n", elapsed, p.Name, cs.RestartCount, reason)
						}
					}
				}

				hpaList, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
				if hpaErr == nil {
					for _, hpa := range hpaList.Items {
						GinkgoWriter.Printf("  [%.0fs] HPA %s: current=%d desired=%d\n", elapsed, hpa.Name, hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas)
					}
				}
			}
		}
		loadEnd := time.Now()
		loadDuration := loadEnd.Sub(loadStart).Seconds()

		By("Evaluating GuideLLM results and Prometheus aggregations per model")
		for i, m := range modelsList {
			depName := deployMap[m]
			jobName := jobNames[i]
			safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
			eppDepName := "gaie-" + safePostfix + "-epp"

			qdAvg, queryErr := QueryRangeAvg(
				promClient.API(),
				fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s", pod=~"%s-.*"})`, benchCfg.LLMDNamespace, depName),
				loadStart, loadEnd, 30*time.Second,
			)
			if queryErr != nil {
				GinkgoWriter.Printf("Warning: failed to query queue depth avg for %s: %v\n", m, queryErr)
			}

			kvAvg, queryErr := QueryRangeAvg(
				promClient.API(),
				fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s", pod=~"%s-.*"})`, benchCfg.LLMDNamespace, depName),
				loadStart, loadEnd, 30*time.Second,
			)
			if queryErr != nil {
				GinkgoWriter.Printf("Warning: failed to query KV cache avg for %s: %v\n", m, queryErr)
			}

			eppQDAvg, queryErr := QueryRangeAvg(
				promClient.API(),
				fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s", pod=~"%s-.*"})`, benchCfg.LLMDNamespace, eppDepName),
				loadStart, loadEnd, 30*time.Second,
			)
			if queryErr != nil {
				GinkgoWriter.Printf("Warning: failed to query EPP queue depth avg for %s: %v\n", m, queryErr)
			}

			var podInfos []PodInfo
			decodePods, podListErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + depName,
			})
			if podListErr == nil {
				for j := range decodePods.Items {
					p := &decodePods.Items[j]
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

			var guidellmRaw json.RawMessage
			var ttftJSON, itlJSON, throughputJSON json.RawMessage
			var requestTotals json.RawMessage
			var errorCount, incompleteCount, completedCount int
			var achievedRPS float64

			logs, err := GetJobPodLogs(ctx, k8sClient, benchCfg.LLMDNamespace, jobName)
			if err == nil {
				if idx := strings.Index(logs, "=== BENCHMARK JSON ==="); idx != -1 {
					jsonStr := strings.TrimSpace(logs[idx+len("=== BENCHMARK JSON ==="):])
					guidellmRaw = json.RawMessage(jsonStr)

					var parsed map[string]interface{}
					if jsonErr := json.Unmarshal(guidellmRaw, &parsed); jsonErr == nil {
						extractGuideLLMMetric(&parsed, "time_to_first_token_ms", &ttftJSON)
						extractGuideLLMMetric(&parsed, "inter_token_latency_ms", &itlJSON)
						extractGuideLLMMetric(&parsed, "output_tokens_per_second", &throughputJSON)
						extractGuideLLMMetric(&parsed, "request_totals", &requestTotals)

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
			}

			if achievedRPS == 0 && completedCount > 0 && loadDuration > 0 {
				achievedRPS = float64(completedCount) / loadDuration
			}

			replicaAvg, queryErr := QueryRangeAvg(
				promClient.API(),
				fmt.Sprintf(`avg(kube_deployment_status_replicas{deployment="%s", namespace="%s"})`, depName, benchCfg.LLMDNamespace),
				loadStart, loadEnd, 30*time.Second,
			)
			if queryErr != nil {
				GinkgoWriter.Printf("Warning: failed to query replica avg for %s: %v\n", m, queryErr)
			}

			result := PrefillResult{
				AutoscalerType:   autoscalerType,
				ModelID:          m,
				VAConfig:         vaConfig,
				HPAConfig:        hpaConfig,
				Pods:             podInfos,
				ReplicaTimeline:  timelineMap[m],
				MetricsTimeline:  metricsTimelineMap[m],
				AvgReplicas:      replicaAvg,
				MaxReplicas:      maxReplicasMap[m],
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
			prefillResults = append(prefillResults, result)

			GinkgoWriter.Printf("\n======================================================\n")
			GinkgoWriter.Printf("  %s PREFILL BENCHMARK RESULTS FOR %s\n", autoscalerType, m)
			GinkgoWriter.Printf("======================================================\n")
			GinkgoWriter.Printf("  Duration:        %.0fs\n", loadDuration)
			GinkgoWriter.Printf("  Max Replicas:    %d\n", maxReplicasMap[m])
			GinkgoWriter.Printf("  Avg Replicas:    %.2f\n", replicaAvg)
			GinkgoWriter.Printf("  Avg Queue Depth: %.2f\n", qdAvg)
			GinkgoWriter.Printf("  Avg EPP Queue:   %.2f\n", eppQDAvg)
			GinkgoWriter.Printf("  Avg KV Cache:    %.3f\n", kvAvg)
			if requestTotals != nil {
				GinkgoWriter.Printf("  Request Totals:  %s\n", string(requestTotals))
			}
			if ttftJSON != nil {
				GinkgoWriter.Printf("  TTFT:            %s\n", string(ttftJSON))
			}
			if itlJSON != nil {
				GinkgoWriter.Printf("  ITL:             %s\n", string(itlJSON))
			}
			if throughputJSON != nil {
				GinkgoWriter.Printf("  Throughput:      %s\n", string(throughputJSON))
			}
			GinkgoWriter.Printf("  Replica Timeline (%d snapshots):\n", len(timelineMap[m]))
			for _, s := range timelineMap[m] {
				GinkgoWriter.Printf("    t=%.0fs  spec=%d  ready=%d\n", s.ElapsedSec, s.SpecReplicas, s.ReadyReplicas)
			}
			GinkgoWriter.Printf("======================================================\n\n")
		}

		By("Saving prefill benchmark results to file")
		data, _ := json.MarshalIndent(prefillResults, "", "  ")
		_ = os.WriteFile(prefillResultsFile, data, 0644)
	}

	Context("WVA", func() {
		It("should run the prefill heavy workload against WVA for all base models", func() {
			cleanupAutoscalers()
			deployMap := findInfraDecodeDeployments()
			ensureInfraDeploymentsReady(deployMap)

			By("Creating VariantAutoscaling resources and HPAs for all models")
			for _, m := range modelsList {
				depName := deployMap[m]
				safePostfix := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "/", "-"), ".", "-")
				vaName := res.VAName + "-" + safePostfix
				hpaName := res.HPAName + "-" + safePostfix

				err := fixtures.EnsureVariantAutoscaling(
					ctx, crClient, benchCfg.LLMDNamespace, vaName, depName,
					m, benchCfg.AcceleratorType, 10.0, benchCfg.ControllerInstance,
					fixtures.WithMinReplicas(1),
					fixtures.WithMaxReplicas(10),
				)
				Expect(err).NotTo(HaveOccurred(), "Failed to create VA for "+m)

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
				err = fixtures.EnsureHPA(ctx, k8sClient, benchCfg.LLMDNamespace, hpaName, depName, vaName, 1, 10, WithBehavior(behavior))
				Expect(err).NotTo(HaveOccurred(), "Failed to create HPA for "+m)
			}

			waitForVAsAndMetrics()

			runPrefillBenchmark("WVA", deployMap)
		})
	})

	AfterAll(func() {
		GinkgoWriter.Println("Prefill benchmark complete — cleaning up autoscalers and scaling to 1 for next test suite")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()

		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "prefill-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "prefill-hpa-standard-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "prefill-hpa-hpa", metav1.DeleteOptions{})
		_ = fixtures.DeleteVariantAutoscaling(cleanupCtx, crClient, benchCfg.LLMDNamespace, "prefill-va")

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

// extractGuideLLMMetric extracts a metric from the GuideLLM JSON structure.
// The structure is: benchmarks[0].metrics.<key>.successful (for successful request stats).
// Falls back to benchmarks[0].metrics.<key> if "successful" sub-key doesn't exist.
func extractGuideLLMMetric(parsed *map[string]interface{}, key string, out *json.RawMessage) {
	benchmarks, ok := (*parsed)["benchmarks"].([]interface{})
	if !ok || len(benchmarks) == 0 {
		return
	}
	bm, ok := benchmarks[0].(map[string]interface{})
	if !ok {
		return
	}
	metrics, ok := bm["metrics"].(map[string]interface{})
	if !ok {
		return
	}
	metricVal, ok := metrics[key]
	if !ok {
		return
	}
	metricMap, ok := metricVal.(map[string]interface{})
	if ok {
		if successful, ok := metricMap["successful"]; ok {
			raw, _ := json.Marshal(successful)
			*out = raw
			return
		}
	}
	raw, _ := json.Marshal(metricVal)
	*out = raw
}

func truncateTail(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-maxLen:]
}

func truncateHead(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
