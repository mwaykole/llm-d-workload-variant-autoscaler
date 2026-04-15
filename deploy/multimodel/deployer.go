package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Deployer orchestrates multi-model deployment and teardown.
type Deployer struct {
	cfg       Config
	clientset *kubernetes.Clientset
	dynClient dynamic.Interface
}

// Deploy deploys N models, creates shared Gateway + HTTPRoute, and verifies connectivity.
func (d *Deployer) Deploy(ctx context.Context) error {
	totalSteps := len(d.cfg.Models) + 3 // N models + Gateway + HTTPRoute + Verify

	logInfo(fmt.Sprintf("Models to deploy (%d): %s", len(d.cfg.Models), modelNames(d.cfg.Models)))
	logInfo(fmt.Sprintf("Resource slugs: %s", modelSlugs(d.cfg.Models)))

	// Step 1: Deploy first model with full control plane
	logStep(1, totalSteps, fmt.Sprintf("Full stack + %s", d.cfg.Models[0].Slug))
	if err := d.runInstallScript(d.cfg.Models[0], true); err != nil {
		return fmt.Errorf("deploy first model %s: %w", d.cfg.Models[0].ModelID, err)
	}

	// Create shared Gateway (replaces the per-model one from install.sh)
	logInfo("Creating shared Gateway (replacing model-specific gateway)...")
	if err := d.createSharedGateway(ctx); err != nil {
		return fmt.Errorf("create shared gateway: %w", err)
	}
	logSuccess(fmt.Sprintf("Shared Gateway '%s' created", sharedGatewayName))

	// Remove Model 1's per-model Gateway
	d.deleteModelGateway(ctx, d.cfg.Models[0].Slug)

	// Steps 2..N: Deploy remaining models concurrently
	if len(d.cfg.Models) > 1 {
		step := 2
		logStep(step, totalSteps, fmt.Sprintf("Deploying %d additional models concurrently", len(d.cfg.Models)-1))

		var wg sync.WaitGroup
		errCh := make(chan error, len(d.cfg.Models)-1)

		for _, m := range d.cfg.Models[1:] {
			wg.Add(1)
			go func(model ModelInfo) {
				defer wg.Done()
				logInfo(fmt.Sprintf("  [concurrent] Deploying %s...", model.Slug))

				// Delete shared EPP Secret to avoid Helm ownership conflict
				_ = d.clientset.CoreV1().Secrets(d.cfg.Namespace).Delete(ctx,
					"inference-scheduling-gateway-sa-metrics-reader-secret",
					metav1.DeleteOptions{})

				if err := d.runInstallScript(model, false); err != nil {
					errCh <- fmt.Errorf("deploy model %s: %w", model.ModelID, err)
					return
				}
				d.deleteModelGateway(ctx, model.Slug)
				logSuccess(fmt.Sprintf("  [concurrent] %s deployed", model.Slug))
			}(m)
		}
		wg.Wait()
		close(errCh)

		for err := range errCh {
			return err
		}
	}

	// Verify InferencePools
	poolStep := len(d.cfg.Models) + 1
	logStep(poolStep, totalSteps, "Verifying InferencePool resources")
	if err := d.verifyInferencePools(ctx); err != nil {
		logWarning(fmt.Sprintf("InferencePool verification: %v", err))
	}

	// Deploy HTTPRoute with URLRewrite rules
	routeStep := len(d.cfg.Models) + 2
	logStep(routeStep, totalSteps, fmt.Sprintf("Deploying HTTPRoute (gateway=%s)", sharedGatewayName))
	if err := d.createHTTPRoute(ctx); err != nil {
		return fmt.Errorf("create HTTPRoute: %w", err)
	}
	logSuccess(fmt.Sprintf("HTTPRoute deployed with %d URLRewrite rules", len(d.cfg.Models)))

	// Verify connectivity
	verifyStep := len(d.cfg.Models) + 3
	logStep(verifyStep, totalSteps, "Verifying Gateway connectivity")
	if err := d.verifyConnectivity(ctx); err != nil {
		logWarning(fmt.Sprintf("Connectivity verification: %v", err))
	}

	logSuccess("Multi-model Infrastructure Deployment Completed!")
	return nil
}

// Undeploy tears down all multi-model resources.
func (d *Deployer) Undeploy(ctx context.Context) error {
	logInfo("Starting Multi-Model Undeployment")
	logInfo(fmt.Sprintf("Models: %s", modelNames(d.cfg.Models)))

	// Delete HTTPRoute
	logInfo("Deleting multi-model HTTPRoute...")
	_ = d.dynClient.Resource(httpRouteGVR()).Namespace(d.cfg.Namespace).Delete(ctx, httpRouteName, metav1.DeleteOptions{})
	logSuccess("HTTPRoute deleted")

	// Delete shared Gateway
	logInfo("Deleting shared Gateway...")
	_ = d.dynClient.Resource(gatewayGVR()).Namespace(d.cfg.Namespace).Delete(ctx, sharedGatewayName, metav1.DeleteOptions{})
	logSuccess("Shared Gateway deleted")

	// Delete InferencePools
	logInfo("Deleting InferencePool resources...")
	poolAPIGroup := detectInferencePoolAPIGroup(ctx, d.clientset)
	for _, m := range d.cfg.Models {
		poolName := "gaie-" + m.Slug
		_ = d.dynClient.Resource(inferencePoolGVR(poolAPIGroup)).Namespace(d.cfg.Namespace).Delete(ctx, poolName, metav1.DeleteOptions{})
		logSuccess(fmt.Sprintf("InferencePool %s deleted", poolName))
	}

	// Uninstall model-specific Helm releases
	logInfo("Removing model-specific Helm releases...")
	for _, m := range d.cfg.Models {
		for _, prefix := range []string{"ms-", "gaie-", "infra-"} {
			releaseName := prefix + m.Slug
			cmd := exec.CommandContext(ctx, "helm", "uninstall", releaseName, "-n", d.cfg.Namespace)
			if out, err := cmd.CombinedOutput(); err != nil {
				logWarning(fmt.Sprintf("  %s not found: %s", releaseName, strings.TrimSpace(string(out))))
			} else {
				logSuccess(fmt.Sprintf("  %s uninstalled", releaseName))
			}
		}
	}

	// Call install.sh --undeploy for the control plane
	logInfo("Undeploying control plane (WVA, monitoring, scaler backend)...")
	env := d.buildBaseEnv()
	env = append(env,
		"RELEASE_NAME_POSTFIX="+d.cfg.Models[0].Slug,
		"INSTALL_GATEWAY_CTRLPLANE=true",
		"DEPLOY_WVA=true",
		"DEPLOY_PROMETHEUS=true",
		"DEPLOY_LLM_D=false",
		"DELETE_NAMESPACES="+d.cfg.DeleteNamespaces,
	)
	cmd := exec.CommandContext(ctx, d.cfg.DeployScript, "--undeploy")
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("undeploy control plane: %w", err)
	}

	logSuccess("Multi-model Undeployment Completed!")
	return nil
}

// runInstallScript calls deploy/install.sh with the appropriate env for a single model.
func (d *Deployer) runInstallScript(model ModelInfo, fullStack bool) error {
	env := d.buildBaseEnv()
	env = append(env,
		"MODEL_ID="+model.ModelID,
		"RELEASE_NAME_POSTFIX="+model.Slug,
		"INFRA_ONLY=false",
		"DEPLOY_LLM_D=true",
		"DECODE_REPLICAS="+d.cfg.DecodeReplicas,
	)

	if fullStack {
		env = append(env,
			"INSTALL_GATEWAY_CTRLPLANE=true",
			"DEPLOY_WVA=true",
			"DEPLOY_PROMETHEUS=true",
			"E2E_TESTS_ENABLED=false",
		)
	} else {
		env = append(env,
			"INSTALL_GATEWAY_CTRLPLANE=false",
			"DEPLOY_WVA=false",
			"DEPLOY_PROMETHEUS=false",
			"DEPLOY_PROMETHEUS_ADAPTER=false",
			"SCALER_BACKEND=none",
			"E2E_TESTS_ENABLED=true",
		)
	}

	cmd := exec.Command(d.cfg.DeployScript) //nolint:gosec // deploy script path from config
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildBaseEnv creates the base environment variable list for install.sh calls,
// inheriting relevant vars from the current process.
func (d *Deployer) buildBaseEnv() []string {
	env := os.Environ()
	env = append(env,
		"ENVIRONMENT="+d.cfg.Environment,
		"LLMD_NS="+d.cfg.Namespace,
	)
	if d.cfg.WVANamespace != "" {
		env = append(env, "WVA_NS="+d.cfg.WVANamespace)
	}
	if d.cfg.NamespaceScoped != "" {
		env = append(env, "NAMESPACE_SCOPED="+d.cfg.NamespaceScoped)
	}
	if d.cfg.LLMDRelease != "" {
		env = append(env, "LLM_D_RELEASE="+d.cfg.LLMDRelease)
	}
	if d.cfg.WVAImageRepo != "" {
		env = append(env, "WVA_IMAGE_REPO="+d.cfg.WVAImageRepo)
	}
	if d.cfg.WVAImageTag != "" {
		env = append(env, "WVA_IMAGE_TAG="+d.cfg.WVAImageTag)
	}
	if d.cfg.WVAImagePullPolicy != "" {
		env = append(env, "WVA_IMAGE_PULL_POLICY="+d.cfg.WVAImagePullPolicy)
	}
	return env
}

// createSharedGateway creates the shared multi-model Gateway resource.
func (d *Deployer) createSharedGateway(ctx context.Context) error {
	gw := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      sharedGatewayName,
				"namespace": d.cfg.Namespace,
				"labels": map[string]interface{}{
					"istio.io/enable-inference-extproc": "true",
				},
			},
			"spec": map[string]interface{}{
				"gatewayClassName": "istio",
				"listeners": []interface{}{
					map[string]interface{}{
						"port":     int64(80),
						"protocol": "HTTP",
						"name":     "default",
						"allowedRoutes": map[string]interface{}{
							"namespaces": map[string]interface{}{
								"from": "All",
							},
						},
					},
				},
			},
		},
	}

	existing, err := d.dynClient.Resource(gatewayGVR()).Namespace(d.cfg.Namespace).Get(ctx, sharedGatewayName, metav1.GetOptions{})
	if err == nil {
		gw.SetResourceVersion(existing.GetResourceVersion())
		_, err = d.dynClient.Resource(gatewayGVR()).Namespace(d.cfg.Namespace).Update(ctx, gw, metav1.UpdateOptions{})
		return err
	}
	_, err = d.dynClient.Resource(gatewayGVR()).Namespace(d.cfg.Namespace).Create(ctx, gw, metav1.CreateOptions{})
	return err
}

// deleteModelGateway removes the per-model Gateway resource created by install.sh.
func (d *Deployer) deleteModelGateway(ctx context.Context, slug string) {
	gwName := fmt.Sprintf("infra-%s-inference-gateway", slug)
	logInfo(fmt.Sprintf("Removing model-specific Gateway %s...", gwName))
	err := d.dynClient.Resource(gatewayGVR()).Namespace(d.cfg.Namespace).Delete(ctx, gwName, metav1.DeleteOptions{})
	if err != nil {
		logWarning(fmt.Sprintf("  Gateway %s not found (may not exist): %v", gwName, err))
	}
}

// verifyInferencePools checks that each model's InferencePool exists.
func (d *Deployer) verifyInferencePools(ctx context.Context) error {
	poolAPIGroup := detectInferencePoolAPIGroup(ctx, d.clientset)
	logInfo(fmt.Sprintf("Detected InferencePool API group: %s", poolAPIGroup))

	for _, m := range d.cfg.Models {
		poolName := "gaie-" + m.Slug
		_, err := d.dynClient.Resource(inferencePoolGVR(poolAPIGroup)).Namespace(d.cfg.Namespace).Get(ctx, poolName, metav1.GetOptions{})
		if err != nil {
			logWarning(fmt.Sprintf("InferencePool %s not found — it should have been created by the gaie-%s Helm release", poolName, m.Slug))
		} else {
			logSuccess(fmt.Sprintf("InferencePool %s exists", poolName))
		}
	}
	return nil
}

// createHTTPRoute creates the shared HTTPRoute with one URLRewrite rule per model.
func (d *Deployer) createHTTPRoute(ctx context.Context) error {
	poolAPIGroup := detectInferencePoolAPIGroup(ctx, d.clientset)

	var rules []interface{}
	for _, m := range d.cfg.Models {
		rule := map[string]interface{}{
			"matches": []interface{}{
				map[string]interface{}{
					"path": map[string]interface{}{
						"type":  "PathPrefix",
						"value": "/" + m.Slug + "/v1",
					},
				},
			},
			"filters": []interface{}{
				map[string]interface{}{
					"type": "URLRewrite",
					"urlRewrite": map[string]interface{}{
						"path": map[string]interface{}{
							"type":               "ReplacePrefixMatch",
							"replacePrefixMatch": "/v1",
						},
					},
				},
			},
			"backendRefs": []interface{}{
				map[string]interface{}{
					"group": poolAPIGroup,
					"kind":  "InferencePool",
					"name":  "gaie-" + m.Slug,
					"port":  int64(8000),
				},
			},
			"timeouts": map[string]interface{}{
				"request": "300s",
			},
		}
		rules = append(rules, rule)
	}

	route := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":      httpRouteName,
				"namespace": d.cfg.Namespace,
			},
			"spec": map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{
						"group": "gateway.networking.k8s.io",
						"kind":  "Gateway",
						"name":  sharedGatewayName,
					},
				},
				"rules": rules,
			},
		},
	}

	existing, err := d.dynClient.Resource(httpRouteGVR()).Namespace(d.cfg.Namespace).Get(ctx, httpRouteName, metav1.GetOptions{})
	if err == nil {
		route.SetResourceVersion(existing.GetResourceVersion())
		_, err = d.dynClient.Resource(httpRouteGVR()).Namespace(d.cfg.Namespace).Update(ctx, route, metav1.UpdateOptions{})
		return err
	}
	_, err = d.dynClient.Resource(httpRouteGVR()).Namespace(d.cfg.Namespace).Create(ctx, route, metav1.CreateOptions{})
	return err
}

func modelNames(models []ModelInfo) string {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.ModelID
	}
	return strings.Join(names, ", ")
}

func modelSlugs(models []ModelInfo) string {
	slugs := make([]string, len(models))
	for i, m := range models {
		slugs[i] = m.Slug
	}
	return strings.Join(slugs, ", ")
}
