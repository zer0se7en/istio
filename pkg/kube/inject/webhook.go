// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package inject

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"gomodules.xyz/jsonpatch/v3"
	kubeApiAdmissionv1 "k8s.io/api/admission/v1"
	kubeApiAdmissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"istio.io/api/annotation"
	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	opconfig "istio.io/istio/operator/pkg/apis/istio/v1alpha1"
	"istio.io/istio/pilot/cmd/pilot-agent/status"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/util/gogoprotomarshal"
	"istio.io/pkg/log"
)

var (
	runtimeScheme     = runtime.NewScheme()
	codecs            = serializer.NewCodecFactory(runtimeScheme)
	deserializer      = codecs.UniversalDeserializer()
	jsonSerializer    = kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, runtimeScheme, runtimeScheme, kjson.SerializerOptions{})
	URLParameterToEnv = map[string]string{
		"cluster": "ISTIO_META_CLUSTER_ID",
		"net":     "ISTIO_META_NETWORK",
	}
)

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = kubeApiAdmissionv1.AddToScheme(runtimeScheme)
	_ = kubeApiAdmissionv1beta1.AddToScheme(runtimeScheme)
}

const (
	watchDebounceDelay = 100 * time.Millisecond
)

// Webhook implements a mutating webhook for automatic proxy injection.
type Webhook struct {
	mu                     sync.RWMutex
	Config                 *Config
	sidecarTemplateVersion string
	meshConfig             *meshconfig.MeshConfig
	valuesConfig           string

	healthCheckInterval time.Duration
	healthCheckFile     string

	watcher Watcher

	mon      *monitor
	env      *model.Environment
	revision string
}

//nolint directives: interfacer
func loadConfig(injectFile, valuesFile string) (*Config, string, error) {
	data, err := ioutil.ReadFile(injectFile)
	if err != nil {
		return nil, "", err
	}
	var c *Config
	if c, err = unmarshalConfig(data); err != nil {
		log.Warnf("Failed to parse injectFile %s", string(data))
		return nil, "", err
	}

	valuesConfig, err := ioutil.ReadFile(valuesFile)
	if err != nil {
		return nil, "", err
	}
	return c, string(valuesConfig), nil
}

func unmarshalConfig(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}

	log.Debugf("New inject configuration: sha256sum %x", sha256.Sum256(data))
	log.Debugf("Policy: %v", c.Policy)
	log.Debugf("AlwaysInjectSelector: %v", c.AlwaysInjectSelector)
	log.Debugf("NeverInjectSelector: %v", c.NeverInjectSelector)
	log.Debugf("Template: |\n  %v", strings.Replace(c.Template, "\n", "\n  ", -1))
	return &c, nil
}

// WebhookParameters configures parameters for the sidecar injection
// webhook.
type WebhookParameters struct {
	// Watcher watches the sidecar injection configuration.
	Watcher Watcher

	// Port is the webhook port, e.g. typically 443 for https.
	// This is mainly used for tests. Webhook runs on the port started by Istiod.
	Port int

	// MonitoringPort is the webhook port, e.g. typically 15014.
	// Set to -1 to disable monitoring
	MonitoringPort int

	// HealthCheckInterval configures how frequently the health check
	// file is updated. Value of zero disables the health check
	// update.
	HealthCheckInterval time.Duration

	// HealthCheckFile specifies the path to the health check file
	// that is periodically updated.
	HealthCheckFile string

	Env *model.Environment

	// Use an existing mux instead of creating our own.
	Mux *http.ServeMux

	// The istio.io/rev this injector is responsible for
	Revision string
}

// NewWebhook creates a new instance of a mutating webhook for automatic sidecar injection.
func NewWebhook(p WebhookParameters) (*Webhook, error) {
	if p.Mux == nil {
		return nil, errors.New("expected mux to be passed, but was not passed")
	}

	wh := &Webhook{
		watcher:             p.Watcher,
		meshConfig:          p.Env.Mesh(),
		healthCheckInterval: p.HealthCheckInterval,
		healthCheckFile:     p.HealthCheckFile,
		env:                 p.Env,
		revision:            p.Revision,
	}

	p.Watcher.SetHandler(wh.updateConfig)
	sidecarConfig, valuesConfig, err := p.Watcher.Get()
	if err != nil {
		return nil, err
	}
	wh.updateConfig(sidecarConfig, valuesConfig)

	p.Mux.HandleFunc("/inject", wh.serveInject)
	p.Mux.HandleFunc("/inject/", wh.serveInject)

	p.Env.Watcher.AddMeshHandler(func() {
		wh.mu.Lock()
		wh.meshConfig = p.Env.Mesh()
		wh.mu.Unlock()
	})

	if p.MonitoringPort >= 0 {
		mon, err := startMonitor(p.Mux, p.MonitoringPort)
		if err != nil {
			return nil, fmt.Errorf("could not start monitoring server %v", err)
		}
		wh.mon = mon
	}

	return wh, nil
}

// Run implements the webhook server
func (wh *Webhook) Run(stop <-chan struct{}) {
	go wh.watcher.Run(stop)

	if wh.mon != nil {
		defer wh.mon.monitoringServer.Close()
	}

	var healthC <-chan time.Time
	if wh.healthCheckInterval != 0 && wh.healthCheckFile != "" {
		t := time.NewTicker(wh.healthCheckInterval)
		healthC = t.C
		defer t.Stop()
	}

	for {
		select {
		case <-healthC:
			content := []byte(`ok`)
			if err := ioutil.WriteFile(wh.healthCheckFile, content, 0644); err != nil {
				log.Errorf("Health check update of %q failed: %v", wh.healthCheckFile, err)
			}
		case <-stop:
			return
		}
	}
}

func (wh *Webhook) updateConfig(sidecarConfig *Config, valuesConfig string) {
	version := sidecarTemplateVersionHash(sidecarConfig.Template)
	wh.mu.Lock()
	wh.Config = sidecarConfig
	wh.valuesConfig = valuesConfig
	wh.sidecarTemplateVersion = version
	wh.mu.Unlock()
}

func setIfUnset(m map[string]string, k, v string) {
	if _, f := m[k]; f {
		// already set
		return
	}
	m[k] = v
}

type ContainerReorder int

const (
	MoveFirst ContainerReorder = iota
	MoveLast
	Remove
)

func modifyContainers(cl []corev1.Container, name string, modifier ContainerReorder) []corev1.Container {
	containers := []corev1.Container{}
	var match *corev1.Container
	for _, c := range cl {
		c := c
		if c.Name != name {
			containers = append(containers, c)
		} else {
			match = &c
		}
	}
	if match == nil {
		return containers
	}
	switch modifier {
	case MoveFirst:
		return append([]corev1.Container{*match}, containers...)
	case MoveLast:
		return append(containers, *match)
	case Remove:
		return containers
	default:
		return cl
	}
}

func enablePrometheusMerge(mesh *meshconfig.MeshConfig, anno map[string]string) bool {
	// If annotation is present, we look there first
	if val, f := anno[annotation.PrometheusMergeMetrics.Name]; f {
		bval, err := strconv.ParseBool(val)
		if err != nil {
			// This shouldn't happen since we validate earlier in the code
			log.Warnf("invalid annotation %v=%v", annotation.PrometheusMergeMetrics.Name, bval)
		} else {
			return bval
		}
	}
	// If mesh config setting is present, use that
	if mesh.GetEnablePrometheusMerge() != nil {
		return mesh.GetEnablePrometheusMerge().Value
	}
	// Otherwise, we default to enable
	return true
}

func ExtractCanonicalServiceLabels(podLabels map[string]string, workloadName string) (string, string) {
	return extractCanonicalServiceLabel(podLabels, workloadName), extractCanonicalServiceRevision(podLabels)
}

func extractCanonicalServiceRevision(podLabels map[string]string) string {
	if rev, ok := podLabels[model.IstioCanonicalServiceRevisionLabelName]; ok {
		return rev
	}

	if rev, ok := podLabels["app.kubernetes.io/version"]; ok {
		return rev
	}

	if rev, ok := podLabels["version"]; ok {
		return rev
	}

	return "latest"
}

func extractCanonicalServiceLabel(podLabels map[string]string, workloadName string) string {
	if svc, ok := podLabels[model.IstioCanonicalServiceLabelName]; ok {
		return svc
	}

	if svc, ok := podLabels["app.kubernetes.io/name"]; ok {
		return svc
	}

	if svc, ok := podLabels["app"]; ok {
		return svc
	}

	return workloadName
}

func toAdmissionResponse(err error) *kube.AdmissionResponse {
	return &kube.AdmissionResponse{Result: &metav1.Status{Message: err.Error()}}
}

type InjectionParameters struct {
	pod                 *corev1.Pod
	deployMeta          *metav1.ObjectMeta
	typeMeta            *metav1.TypeMeta
	template            string
	version             string
	meshConfig          *meshconfig.MeshConfig
	valuesConfig        string
	revision            string
	proxyEnvs           map[string]string
	injectedAnnotations map[string]string
}

func checkPreconditions(params InjectionParameters) {
	spec := params.pod.Spec
	metadata := params.pod.ObjectMeta
	// If DNSPolicy is not ClusterFirst, the Envoy sidecar may not able to connect to Istio Pilot.
	if spec.DNSPolicy != "" && spec.DNSPolicy != corev1.DNSClusterFirst {
		podName := potentialPodName(metadata)
		log.Warnf("%q's DNSPolicy is not %q. The Envoy sidecar may not able to connect to Istio Pilot",
			metadata.Namespace+"/"+podName, corev1.DNSClusterFirst)
	}
}

func getInjectionStatus(podSpec corev1.PodSpec, version string) string {
	stat := &SidecarInjectionStatus{Version: version}
	for _, c := range podSpec.InitContainers {
		stat.InitContainers = append(stat.InitContainers, c.Name)
	}
	for _, c := range podSpec.Containers {
		stat.Containers = append(stat.Containers, c.Name)
	}
	for _, c := range podSpec.Volumes {
		stat.Volumes = append(stat.Volumes, c.Name)
	}
	for _, c := range podSpec.ImagePullSecrets {
		stat.ImagePullSecrets = append(stat.ImagePullSecrets, c.Name)
	}
	statusAnnotationValue, err := json.Marshal(stat)
	if err != nil {
		return "{}"
	}
	return string(statusAnnotationValue)
}

func injectPod(req InjectionParameters) ([]byte, error) {
	checkPreconditions(req)
	originalPodSpec, err := json.Marshal(req.pod)
	if err != nil {
		return nil, err
	}

	// Run the injection template, giving us a partial pod spec
	spec, injectedSpec, err := RunTemplate(req)
	if err != nil {
		return nil, fmt.Errorf("failed to run injection template: %v", err)
	}

	// Merge the original pod spec with the injected overlay
	mergedPodSpec, err := mergeInjectedConfig(req, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to merge pod spec: %v", err)
	}
	pod := req.pod.DeepCopy()
	pod.Spec = mergedPodSpec

	// Apply some additional transformations to the pod
	if err := postProcessPod(pod, *injectedSpec, req); err != nil {
		return nil, fmt.Errorf("failed to process pod: %v", err)
	}

	patch, err := createPatch(pod, originalPodSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch: %v", err)
	}

	log.Debugf("AdmissionResponse: patch=%v\n", string(patch))
	return patch, nil
}

func createPatch(pod *corev1.Pod, original []byte) ([]byte, error) {
	reinjected, err := json.Marshal(pod)
	if err != nil {
		return nil, err
	}
	p, err := jsonpatch.CreatePatch(original, reinjected)
	if err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// postProcessPod applies additionally transformations to the pod after merging with the injected template
// This is generally things that cannot reasonably be added to the template
func postProcessPod(pod *corev1.Pod, injectedPodSpec corev1.PodSpec, req InjectionParameters) error {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}

	applyConcurrency(pod.Spec.Containers)

	overwriteClusterInfo(pod.Spec.Containers, req)

	if err := applyPrometheusMerge(pod, req.meshConfig); err != nil {
		return err
	}

	applyFSGroup(pod)

	if err := applyRewrite(pod, req); err != nil {
		return err
	}

	applyMetadata(pod, injectedPodSpec, req)

	if err := reorderPod(pod, req); err != nil {
		return err
	}

	return nil
}

func applyMetadata(pod *corev1.Pod, injectedPodSpec corev1.PodSpec, req InjectionParameters) {
	canonicalSvc, canonicalRev := ExtractCanonicalServiceLabels(pod.Labels, req.deployMeta.Name)
	setIfUnset(pod.Labels, label.TLSMode, model.IstioMutualTLSModeLabel)
	setIfUnset(pod.Labels, model.IstioCanonicalServiceLabelName, canonicalSvc)
	setIfUnset(pod.Labels, label.IstioRev, req.revision)
	setIfUnset(pod.Labels, model.IstioCanonicalServiceRevisionLabelName, canonicalRev)

	// Add all additional injected annotations. These are overridden if needed
	pod.Annotations[annotation.SidecarStatus.Name] = getInjectionStatus(injectedPodSpec, req.version)
	for k, v := range req.injectedAnnotations {
		pod.Annotations[k] = v
	}

}

// reorderPod ensures containers are properly ordered after merging
func reorderPod(pod *corev1.Pod, req InjectionParameters) error {
	var (
		merr error
	)
	mc := &meshconfig.MeshConfig{
		DefaultConfig: &meshconfig.ProxyConfig{},
	}
	// Get copy of pod proxyconfig, to determine container ordering
	if pca, f := req.pod.ObjectMeta.GetAnnotations()[annotation.ProxyConfig.Name]; f {
		mc, merr = mesh.ApplyProxyConfig(pca, *req.meshConfig)
		if merr != nil {
			return merr
		}
	}

	valuesStruct := &opconfig.Values{}
	if err := gogoprotomarshal.ApplyYAML(req.valuesConfig, valuesStruct); err != nil {
		return fmt.Errorf("could not parse configuration values: %v", err)
	}
	// nolint: staticcheck
	holdPod := mc.DefaultConfig.HoldApplicationUntilProxyStarts.GetValue() ||
		valuesStruct.GetGlobal().GetProxy().GetHoldApplicationUntilProxyStarts().GetValue()

	proxyLocation := MoveLast
	// If HoldApplicationUntilProxyStarts is set, reorder the proxy location
	if holdPod {
		proxyLocation = MoveFirst
	}

	// Proxy container should be last, unless HoldApplicationUntilProxyStarts is set
	// This is to ensure `kubectl exec` and similar commands continue to default to the user's container
	pod.Spec.Containers = modifyContainers(pod.Spec.Containers, ProxyContainerName, proxyLocation)
	// Validation container must be first to block any user containers
	pod.Spec.InitContainers = modifyContainers(pod.Spec.InitContainers, ValidationContainerName, MoveFirst)
	// Init container must be last to allow any traffic to pass before iptables is setup
	pod.Spec.InitContainers = modifyContainers(pod.Spec.InitContainers, InitContainerName, MoveLast)
	pod.Spec.InitContainers = modifyContainers(pod.Spec.InitContainers, EnableCoreDumpName, MoveLast)

	return nil
}

func applyRewrite(pod *corev1.Pod, req InjectionParameters) error {
	valuesStruct := &opconfig.Values{}
	if err := gogoprotomarshal.ApplyYAML(req.valuesConfig, valuesStruct); err != nil {
		log.Infof("Failed to parse values config: %v [%v]\n", err, req.valuesConfig)
		return fmt.Errorf("could not parse configuration values: %v", err)
	}

	rewrite := ShouldRewriteAppHTTPProbers(pod.Annotations, valuesStruct.GetSidecarInjectorWebhook().GetRewriteAppHTTPProbe())
	sidecar := FindSidecar(pod.Spec.Containers)

	// We don't have to escape json encoding here when using golang libraries.
	if rewrite && sidecar != nil {
		if prober := DumpAppProbers(&pod.Spec, req.meshConfig.GetDefaultConfig().GetStatusPort()); prober != "" {
			sidecar.Env = append(sidecar.Env, corev1.EnvVar{Name: status.KubeAppProberEnvName, Value: prober})
		}
		patchRewriteProbe(pod.Annotations, pod, req.meshConfig.GetDefaultConfig().GetStatusPort())
	}
	return nil
}

func applyFSGroup(pod *corev1.Pod) {
	if features.EnableLegacyFSGroupInjection {
		// due to bug https://github.com/kubernetes/kubernetes/issues/57923,
		// k8s sa jwt token volume mount file is only accessible to root user, not istio-proxy(the user that istio proxy runs as).
		// workaround by https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#set-the-security-context-for-a-pod
		var grp = int64(1337)
		if pod.Spec.SecurityContext == nil {
			pod.Spec.SecurityContext = &corev1.PodSecurityContext{
				FSGroup: &grp,
			}
		} else {
			pod.Spec.SecurityContext.FSGroup = &grp
		}
	}
}

// applyPrometheusMerge configures prometheus scraping annotations for the "metrics merge" feature.
// This moves the current prometheus.io annotations into an environment variable and replaces them
// pointing to the agent.
func applyPrometheusMerge(pod *corev1.Pod, mesh *meshconfig.MeshConfig) error {
	sidecar := FindSidecar(pod.Spec.Containers)
	if enablePrometheusMerge(mesh, pod.ObjectMeta.Annotations) {
		targetPort := strconv.Itoa(int(mesh.GetDefaultConfig().GetStatusPort()))
		if cur, f := pod.Annotations["prometheus.io/port"]; f {
			// We have already set the port, assume user is controlling this or, more likely, re-injected
			// the pod.
			if cur == targetPort {
				return nil
			}
		}
		scrape := status.PrometheusScrapeConfiguration{
			Scrape: pod.Annotations["prometheus.io/scrape"],
			Path:   pod.Annotations["prometheus.io/path"],
			Port:   pod.Annotations["prometheus.io/port"],
		}
		empty := status.PrometheusScrapeConfiguration{}
		if sidecar != nil && scrape != empty {
			by, err := json.Marshal(scrape)
			if err != nil {
				return err
			}
			sidecar.Env = append(sidecar.Env, corev1.EnvVar{Name: status.PrometheusScrapingConfig.Name, Value: string(by)})
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations["prometheus.io/port"] = targetPort
		pod.Annotations["prometheus.io/path"] = "/stats/prometheus"
		pod.Annotations["prometheus.io/scrape"] = "true"
	}
	return nil
}

func mergeInjectedConfig(req InjectionParameters, injected []byte) (corev1.PodSpec, error) {
	current, err := json.Marshal(req.pod.Spec)
	if err != nil {
		return corev1.PodSpec{}, err
	}

	// The template is yaml, StrategicMergePatch expects JSON
	injectedJSON, err := yaml.YAMLToJSON(injected)
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("yaml to json: %v", err)
	}

	pod := corev1.PodSpec{}
	// Overlay the injected template onto the original pod spec
	patched, err := strategicpatch.StrategicMergePatch(current, injectedJSON, pod)
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("strategic merge: %v", err)
	}
	if err := json.Unmarshal(patched, &pod); err != nil {
		return corev1.PodSpec{}, fmt.Errorf("unmarshal patched pod: %v", err)
	}
	return pod, nil
}

func (wh *Webhook) inject(ar *kube.AdmissionReview, path string) *kube.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		handleError(fmt.Sprintf("Could not unmarshal raw object: %v %s", err,
			string(req.Object.Raw)))
		return toAdmissionResponse(err)
	}

	// Deal with potential empty fields, e.g., when the pod is created by a deployment
	podName := potentialPodName(pod.ObjectMeta)
	if pod.ObjectMeta.Namespace == "" {
		pod.ObjectMeta.Namespace = req.Namespace
	}
	log.Infof("Sidecar injection request for %v/%v", req.Namespace, podName)
	log.Debugf("Object: %v", string(req.Object.Raw))
	log.Debugf("OldObject: %v", string(req.OldObject.Raw))

	if !injectRequired(ignoredNamespaces, wh.Config, &pod.Spec, pod.ObjectMeta) {
		log.Infof("Skipping %s/%s due to policy check", pod.ObjectMeta.Namespace, podName)
		totalSkippedInjections.Increment()
		return &kube.AdmissionResponse{
			Allowed: true,
		}
	}

	deploy, typeMeta := kube.GetDeployMetaFromPod(&pod)
	params := InjectionParameters{
		pod:                 &pod,
		deployMeta:          deploy,
		typeMeta:            typeMeta,
		template:            wh.Config.Template,
		version:             wh.sidecarTemplateVersion,
		meshConfig:          wh.meshConfig,
		valuesConfig:        wh.valuesConfig,
		revision:            wh.revision,
		injectedAnnotations: wh.Config.InjectedAnnotations,
		proxyEnvs:           parseInjectEnvs(path),
	}

	patchBytes, err := injectPod(params)
	if err != nil {
		handleError(fmt.Sprintf("Pod injection failed: %v", err))
		return toAdmissionResponse(err)
	}

	reviewResponse := kube.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *string {
			pt := "JSONPatch"
			return &pt
		}(),
	}
	totalSuccessfulInjections.Increment()
	return &reviewResponse
}

func (wh *Webhook) serveInject(w http.ResponseWriter, r *http.Request) {
	totalInjections.Increment()
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		handleError("no body found")
		http.Error(w, "no body found", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		handleError(fmt.Sprintf("contentType=%s, expect application/json", contentType))
		http.Error(w, "invalid Content-Type, want `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}

	var reviewResponse *kube.AdmissionResponse
	var obj runtime.Object
	var ar *kube.AdmissionReview
	if out, _, err := deserializer.Decode(body, nil, obj); err != nil {
		handleError(fmt.Sprintf("Could not decode body: %v", err))
		reviewResponse = toAdmissionResponse(err)
	} else {
		log.Debugf("AdmissionRequest for path=%s\n", path)
		ar, err = kube.AdmissionReviewKubeToAdapter(out)
		if err != nil {
			handleError(fmt.Sprintf("Could not decode object: %v", err))
		}
		reviewResponse = wh.inject(ar, path)
	}

	response := kube.AdmissionReview{}
	response.Response = reviewResponse
	var responseKube runtime.Object
	var apiVersion string
	if ar != nil {
		apiVersion = ar.APIVersion
		response.TypeMeta = ar.TypeMeta
		if response.Response != nil {
			if ar.Request != nil {
				response.Response.UID = ar.Request.UID
			}
		}
	}
	responseKube = kube.AdmissionReviewAdapterToKube(&response, apiVersion)
	resp, err := json.Marshal(responseKube)
	if err != nil {
		log.Errorf("Could not encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	if _, err := w.Write(resp); err != nil {
		log.Errorf("Could not write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}

// parseInjectEnvs parse new envs from inject url path
// follow format: /inject/k1/v1/k2/v2, any kv order works
// eg. "/inject/cluster/cluster1", "/inject/net/network1/cluster/cluster1"
func parseInjectEnvs(path string) map[string]string {
	path = strings.TrimSuffix(path, "/")
	res := strings.Split(path, "/")
	newEnvs := make(map[string]string)

	for i := 2; i < len(res); i += 2 { // skip '/inject'
		k := res[i]
		if i == len(res)-1 { // ignore the last key without value
			log.Warnf("Odd number of inject env entries, ignore the last key %s\n", k)
			break
		}

		env, found := URLParameterToEnv[k]
		if !found {
			env = strings.ToUpper(k) // if not found, use the custom env directly
		}
		if env != "" {
			newEnvs[env] = res[i+1]
		}
	}

	return newEnvs
}

func handleError(message string) {
	log.Errorf(message)
	totalFailedInjections.Increment()
}
