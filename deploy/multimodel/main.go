// Package main provides a Go replacement for deploy/install-multi-model.sh.
//
// It orchestrates multi-model deployments by calling deploy/install.sh per model,
// then uses the Kubernetes API directly to create the shared Gateway, HTTPRoute with
// URLRewrite rules, and verify connectivity — eliminating the need for an in-cluster
// curl Job and enabling concurrent model deployments.
//
// Usage:
//
//	MODELS="Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B" go run ./deploy/multimodel
//	MODELS="Qwen/Qwen3-0.6B" go run ./deploy/multimodel --undeploy
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	cfg := loadConfig()

	k8sCfg, err := buildK8sConfig()
	if err != nil {
		log.Fatalf("Failed to build Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes clientset: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	deployer := &Deployer{
		cfg:       cfg,
		clientset: clientset,
		dynClient: dynClient,
	}

	if cfg.Undeploy {
		if err := deployer.Undeploy(context.Background()); err != nil {
			log.Fatalf("Undeploy failed: %v", err)
		}
	} else {
		if err := deployer.Deploy(context.Background()); err != nil {
			log.Fatalf("Deploy failed: %v", err)
		}
	}
}

func buildK8sConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	if _, err := os.Stat(kubeconfig); err == nil {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// Config holds multi-model deployment parameters, loaded from environment variables.
type Config struct {
	Models         []ModelInfo
	Namespace      string
	Environment    string
	DecodeReplicas string
	ProjectDir     string
	DeployScript   string
	Undeploy       bool
	// Passthrough env vars for install.sh
	WVAImageRepo      string
	WVAImageTag       string
	WVAImagePullPolicy string
	NamespaceScoped   string
	LLMDRelease       string
	DeleteNamespaces  string
	WVANamespace      string
}

// ModelInfo groups a HuggingFace model ID with its DNS-safe slug.
type ModelInfo struct {
	ModelID string
	Slug    string
}

// ModelToSlug converts a HuggingFace model ID into a DNS-safe resource slug.
func ModelToSlug(modelID string) string {
	s := strings.ToLower(modelID)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

func loadConfig() Config {
	modelsStr := os.Getenv("MODELS")
	if modelsStr == "" {
		modelsStr = "Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B"
	}

	var models []ModelInfo
	for _, m := range strings.Split(modelsStr, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		models = append(models, ModelInfo{ModelID: m, Slug: ModelToSlug(m)})
	}
	if len(models) == 0 {
		log.Fatal("MODELS must contain at least 1 comma-separated model ID")
	}

	projectDir := os.Getenv("WVA_PROJECT")
	if projectDir == "" {
		var err error
		projectDir, err = os.Getwd()
		if err != nil {
			log.Fatalf("Failed to get working directory: %v", err)
		}
	}

	undeploy := false
	for _, arg := range os.Args[1:] {
		if arg == "--undeploy" || arg == "-u" {
			undeploy = true
		}
	}

	return Config{
		Models:             models,
		Namespace:          envDefault("LLMD_NS", "llm-d-inference-scheduler"),
		Environment:        envDefault("ENVIRONMENT", "kind-emulator"),
		DecodeReplicas:     envDefault("DECODE_REPLICAS", "1"),
		ProjectDir:         projectDir,
		DeployScript:       projectDir + "/deploy/install.sh",
		Undeploy:           undeploy,
		WVAImageRepo:       os.Getenv("WVA_IMAGE_REPO"),
		WVAImageTag:        os.Getenv("WVA_IMAGE_TAG"),
		WVAImagePullPolicy: envDefault("WVA_IMAGE_PULL_POLICY", "IfNotPresent"),
		NamespaceScoped:    envDefault("NAMESPACE_SCOPED", "false"),
		LLMDRelease:        os.Getenv("LLM_D_RELEASE"),
		DeleteNamespaces:   envDefault("DELETE_NAMESPACES", "false"),
		WVANamespace:       os.Getenv("WVA_NS"),
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// gatewayGVR returns the GVR for Gateway API Gateway resources.
func gatewayGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
}

// httpRouteGVR returns the GVR for Gateway API HTTPRoute resources.
func httpRouteGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}
}

// detectInferencePoolAPIGroup checks which InferencePool CRD is installed,
// preferring the GA version.
func detectInferencePoolAPIGroup(ctx context.Context, clientset *kubernetes.Clientset) string {
	_, err := clientset.Discovery().ServerResourcesForGroupVersion("inference.networking.k8s.io/v1")
	if err == nil {
		return "inference.networking.k8s.io"
	}
	return "inference.networking.x-k8s.io"
}

// inferencePoolGVR returns the GVR for InferencePool, auto-detecting the API group.
func inferencePoolGVR(apiGroup string) schema.GroupVersionResource {
	version := "v1"
	if apiGroup == "inference.networking.x-k8s.io" {
		version = "v1alpha2"
	}
	return schema.GroupVersionResource{
		Group:    apiGroup,
		Version:  version,
		Resource: "inferencepools",
	}
}

const (
	sharedGatewayName = "multi-model-inference-gateway"
	httpRouteName     = "multi-model-route"
)

func logStep(step, total int, msg string) {
	fmt.Printf("\n═══════════════════════════════════════════════════════════\n")
	fmt.Printf("  Step %d/%d: %s\n", step, total, msg)
	fmt.Printf("═══════════════════════════════════════════════════════════\n\n")
}

func logInfo(msg string)    { fmt.Printf("[INFO]    %s\n", msg) }
func logSuccess(msg string) { fmt.Printf("[SUCCESS] %s\n", msg) }
func logWarning(msg string) { fmt.Printf("[WARNING] %s\n", msg) }

// verifyConnectivity checks that each model is reachable through the Gateway
// by creating a temporary port-forward and issuing HTTP requests from the
// Go process — no in-cluster Job or Docker Hub image needed.
func (d *Deployer) verifyConnectivity(ctx context.Context) error {
	gwSvcName := sharedGatewayName + "-istio"

	svc, err := d.clientset.CoreV1().Services(d.cfg.Namespace).Get(ctx, gwSvcName, metav1.GetOptions{})
	if err != nil {
		logWarning(fmt.Sprintf("Gateway service %s not found, skipping connectivity check: %v", gwSvcName, err))
		return nil
	}

	var svcPort int32 = 80
	for _, p := range svc.Spec.Ports {
		if p.Name == "http" || p.Name == "default" || p.Port == 80 {
			svcPort = p.Port
			break
		}
	}

	logInfo(fmt.Sprintf("Verifying connectivity via %s:%d", gwSvcName, svcPort))
	logInfo("Creating port-forward to Gateway service...")

	pf, err := newPortForwarder(d.clientset, d.cfg.Namespace, gwSvcName, int(svcPort))
	if err != nil {
		logWarning(fmt.Sprintf("Port-forward setup failed: %v — skipping connectivity check", err))
		return nil
	}
	defer pf.close()

	if err := pf.waitReady(30 * time.Second); err != nil {
		logWarning(fmt.Sprintf("Port-forward not ready: %v — skipping connectivity check", err))
		return nil
	}

	timeout := 3 * time.Minute
	interval := 10 * time.Second
	deadline := time.After(timeout)

	for {
		allOK := true
		for _, m := range d.cfg.Models {
			url := fmt.Sprintf("http://localhost:%d/%s/v1/models", pf.localPort, m.Slug)
			code := httpGetStatus(url)
			if code != 200 {
				allOK = false
			}
		}
		if allOK {
			for _, m := range d.cfg.Models {
				logSuccess(fmt.Sprintf("✓ %s reachable via Gateway", m.Slug))
			}
			logSuccess(fmt.Sprintf("All %d models reachable through the Gateway!", len(d.cfg.Models)))
			return nil
		}
		select {
		case <-deadline:
			for _, m := range d.cfg.Models {
				url := fmt.Sprintf("http://localhost:%d/%s/v1/models", pf.localPort, m.Slug)
				code := httpGetStatus(url)
				if code == 200 {
					logSuccess(fmt.Sprintf("✓ %s reachable (HTTP 200)", m.Slug))
				} else {
					logWarning(fmt.Sprintf("✗ %s not reachable (HTTP %d)", m.Slug, code))
				}
			}
			logWarning(fmt.Sprintf("Some models not reachable after %s — they may still be loading.", timeout))
			return nil
		case <-time.After(interval):
		}
	}
}
