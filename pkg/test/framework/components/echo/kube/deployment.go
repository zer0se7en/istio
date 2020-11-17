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

package kube

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/image"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/tmpl"
)

const (
	serviceYAML = `
{{- if .ServiceAccount }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ .Service }}
---
{{- end }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Service }}
  labels:
    app: {{ .Service }}
{{- if .ServiceAnnotations }}
  annotations:
{{- range $name, $value := .ServiceAnnotations }}
    {{ $name.Name }}: {{ printf "%q" $value.Value }}
{{- end }}
{{- end }}
spec:
{{- if .Headless }}
  clusterIP: None
{{- end }}
  ports:
{{- range $i, $p := .Ports }}
  - name: {{ $p.Name }}
    port: {{ $p.ServicePort }}
    targetPort: {{ $p.InstancePort }}
{{- end }}
  selector:
    app: {{ .Service }}
`

	deploymentYAML = `
{{- $subsets := .Subsets }}
{{- $cluster := .Cluster }}
{{- range $i, $subset := $subsets }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $.Service }}-{{ $subset.Version }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ $.Service }}
      version: {{ $subset.Version }}
{{- if ne $.Locality "" }}
      istio-locality: {{ $.Locality }}
{{- end }}
  template:
    metadata:
      labels:
        app: {{ $.Service }}
        version: {{ $subset.Version }}
{{- if ne $.Locality "" }}
        istio-locality: {{ $.Locality }}
{{- end }}
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "15014"
{{- range $name, $value := $subset.Annotations }}
        {{ $name.Name }}: {{ printf "%q" $value.Value }}
{{- end }}
    spec:
{{- if $.ServiceAccount }}
      serviceAccountName: {{ $.Service }}
{{- end }}
      containers:
      - name: app
        image: {{ $.Hub }}/app:{{ $.Tag }}
        imagePullPolicy: {{ $.PullPolicy }}
        args:
          - --metrics=15014
          - --cluster
          - "{{ $cluster }}"
{{- range $i, $p := $.ContainerPorts }}
{{- if eq .Protocol "GRPC" }}
          - --grpc
{{- else if eq .Protocol "TCP" }}
          - --tcp
{{- else }}
          - --port
{{- end }}
          - "{{ $p.Port }}"
{{- if $p.TLS }}
          - --tls={{ $p.Port }}
{{- end }}
{{- if $p.ServerFirst }}
          - --server-first={{ $p.Port }}
{{- end }}
{{- if $p.InstanceIP }}
          - --bind-ip={{ $p.Port }}
{{- end }}
{{- end }}
{{- range $i, $p := $.WorkloadOnlyPorts }}
{{- if eq .Protocol "TCP" }}
          - --tcp
{{- else }}
          - --port
{{- end }}
          - "{{ $p.Port }}"
{{- if $p.TLS }}
          - --tls={{ $p.Port }}
{{- end }}
{{- if $p.ServerFirst }}
          - --server-first={{ $p.Port }}
{{- end }}
{{- end }}
          - --version
          - "{{ $subset.Version }}"
{{- if $.TLSSettings }}
          - --crt=/etc/certs/custom/cert-chain.pem
          - --key=/etc/certs/custom/key.pem
{{- end }}
        ports:
{{- range $i, $p := $.ContainerPorts }}
        - containerPort: {{ $p.Port }} 
{{- if eq .Port 3333 }}
          name: tcp-health-port
{{- end }}
{{- end }}
        env:
        - name: INSTANCE_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        readinessProbe:
          httpGet:
            path: /
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
          failureThreshold: 10
        livenessProbe:
          tcpSocket:
            port: tcp-health-port
          initialDelaySeconds: 10
          periodSeconds: 10
          failureThreshold: 10
{{- if $.StartupProbe }}
        startupProbe:
          tcpSocket:
            port: tcp-health-port
          periodSeconds: 10
          failureThreshold: 10
{{- end }}
{{- if $.TLSSettings }}
        volumeMounts:
        - mountPath: /etc/certs/custom
          name: custom-certs
      volumes:
      - configMap:
          name: {{ $.Service }}-certs
        name: custom-certs
{{- end}}
---
{{- end}}
{{- if .TLSSettings }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ $.Service }}-certs
data:
  root-cert.pem: |
{{ .TLSSettings.RootCert | indent 4 }}
  cert-chain.pem: |
{{ .TLSSettings.ClientCert | indent 4 }}
  key.pem: |
{{.TLSSettings.Key | indent 4}}
---
{{- end}}
`

	// vmDeploymentYaml aims to simulate a VM, but instead of managing the complex test setup of spinning up a VM,
	// connecting, etc we run it inside a pod. The pod has pretty much all Kubernetes features disabled (DNS and SA token mount)
	// such that we can adequately simulate a VM and DIY the bootstrapping.
	vmDeploymentYaml = `
{{- $subsets := .Subsets }}
{{- $cluster := .Cluster }}
{{- range $i, $subset := $subsets }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $.Service }}-{{ $subset.Version }}
spec:
  replicas: 1
  selector:
    matchLabels:
      istio.io/test-vm: {{ $.Service }}
      istio.io/test-vm-version: {{ $subset.Version }}
  template:
    metadata:
      annotations:
        # Sidecar is inside the pod to simulate VMs - do not inject
        sidecar.istio.io/inject: "false"
      labels:
        # Label should not be selected. We will create a workload entry instead
        istio.io/test-vm: {{ $.Service }}
        istio.io/test-vm-version: {{ $subset.Version }}
    spec:
      # Disable kube-dns, to mirror VM
      # we set policy to none and explicitly provide a set of invalid values
      # for nameservers, search namespaces, etc. ndots is set to 1 so that
      # the application will first try to resolve the hostname (a, a.ns, etc.) as is
      # before attempting to add the search namespaces.
      dnsPolicy: None
      dnsConfig:
        nameservers:
        - "8.8.8.8"
        searches:
        - "com"
        options:
        - name: "ndots"
          value: "1"
      # Disable service account mount, to mirror VM
      automountServiceAccountToken: false
      containers:
      - name: istio-proxy
        image: {{ $.Hub }}/{{ $.VM.Image }}:{{ $.Tag }}
        imagePullPolicy: {{ $.PullPolicy }}
        securityContext:
          capabilities:
            add:
            - NET_ADMIN
          runAsUser: 1338
          runAsGroup: 1338
        command:
        - bash
        - -c
        - |-
          # Capture all inbound and outbound traffic
          sudo sh -c 'echo ISTIO_SERVICE_CIDR=* >> /var/lib/istio/envoy/cluster.env'
          sudo sh -c 'echo ISTIO_INBOUND_PORTS=* >> /var/lib/istio/envoy/cluster.env'
          
          # Read root cert from and place signed certs here
          sudo mkdir -p /var/run/secrets/istio
 
          # hack: remove certs that are bundled in the image
          sudo rm /var/run/secrets/istio/cert-chain.pem  
          sudo rm /var/run/secrets/istio/key.pem  
          sudo chown -R istio-proxy /var/run/secrets

          # replace root-cert with what was mounted
          sudo cp /var/run/secrets/istio/rootmount/* /var/run/secrets/istio
          sudo sh -c 'echo PROV_CERT=/var/run/secrets/istio >> /var/lib/istio/envoy/cluster.env'
          sudo sh -c 'echo OUTPUT_CERTS=/var/run/secrets/istio >> /var/lib/istio/envoy/cluster.env'
          # Block standard inbound ports
          sudo sh -c 'echo ISTIO_LOCAL_EXCLUDE_PORTS="15090,15021,15020" >> /var/lib/istio/envoy/cluster.env'
          # Proxy XDS via agent first
          sudo sh -c 'echo PROXY_XDS_VIA_AGENT=true >> /var/lib/istio/envoy/cluster.env'
          {{- if $.VM.AutoRegister }}
          sudo sh -c 'echo ISTIO_META_AUTO_REGISTER_GROUP={{$.Service}} >> /var/lib/istio/envoy/cluster.env'
          {{- end }}
          # Capture all DNS traffic in the VM and forward to Envoy
          sudo sh -c 'echo ISTIO_META_DNS_CAPTURE=true >> /var/lib/istio/envoy/cluster.env'
          sudo sh -c 'echo ISTIO_PILOT_PORT={{$.VM.IstiodPort}} >> /var/lib/istio/envoy/cluster.env'

          # Setup the namespace
          sudo sh -c 'echo ISTIO_NAMESPACE={{ $.Namespace }} >> /var/lib/istio/envoy/sidecar.env'

          sudo sh -c 'echo "{{$.VM.IstiodIP}} istiod.istio-system.svc" >> /etc/hosts'

          # Provide a proxyconfig override
          
          {{- range $name, $value := $subset.Annotations }}
          {{- if eq $name.Name "proxy.istio.io/config" }}
          sudo sh -c 'chmod a+w /etc/istio/config/mesh'
          sudo sh -c 'echo "defaultConfig:" >> /etc/istio/config/mesh'
          {{- range $idx, $line := (Lines $value.Value) }}
          sudo sh -c 'echo "  {{ $line }}" >> /etc/istio/config/mesh'
          {{- end }}
          {{- end }}
          {{- end }}

          # TODO: run with systemctl?
          export ISTIO_AGENT_FLAGS="--concurrency 2"
          sudo -E /usr/local/bin/istio-start.sh&
          /usr/local/bin/server --cluster "{{ $cluster }}" --version "{{ $subset.Version }}" \
{{- range $i, $p := $.ContainerPorts }}
{{- if eq .Protocol "GRPC" }}
             --grpc \
{{- else if eq .Protocol "TCP" }}
             --tcp \
{{- else }}
             --port \
{{- end }}
             "{{ $p.Port }}" \
{{- if $p.ServerFirst }}
             --server-first={{ $p.Port }} \
{{- end }}
{{- if $p.InstanceIP }}
             --bind-ip={{ $p.Port }} \
{{- end }}
{{- end }}
        env:
        - name: INSTANCE_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        {{- range $name, $value := $.Environment }}
        - name: {{ $name }}
          value: "{{ $value }}"
        {{- end }}
        readinessProbe:
          httpGet:
            path: /healthz/ready
            port: 15021
          initialDelaySeconds: 1
          periodSeconds: 2
          failureThreshold: 10
        volumeMounts:
        - mountPath: /var/run/secrets/tokens
          name: {{ $.Service }}-istio-token
        - mountPath: /var/run/secrets/istio/rootmount
          name: istio-ca-root-cert
        {{- range $name, $value := $subset.Annotations }}
        {{- if eq $name.Name "sidecar.istio.io/bootstrapOverride" }}
        - mountPath: /etc/istio/custom-bootstrap
          name: custom-bootstrap-volume
        {{- end }}
        {{- end }}
      volumes:
      - secret:
          secretName: {{ $.Service }}-istio-token
        name: {{ $.Service }}-istio-token
      - configMap:
          name: istio-ca-root-cert
        name: istio-ca-root-cert
      {{- range $name, $value := $subset.Annotations }}
      {{- if eq $name.Name "sidecar.istio.io/bootstrapOverride" }}
      - name: custom-bootstrap-volume
        configMap:
          name: {{ $value.Value }}
      {{- end }}
      {{- end }}
{{- end}}
`
)

var (
	serviceTemplate      *template.Template
	deploymentTemplate   *template.Template
	vmDeploymentTemplate *template.Template
)

func init() {
	serviceTemplate = template.New("echo_service")
	if _, err := serviceTemplate.Funcs(sprig.TxtFuncMap()).Parse(serviceYAML); err != nil {
		panic(fmt.Sprintf("unable to parse echo service template: %v", err))
	}

	deploymentTemplate = template.New("echo_deployment")
	if _, err := deploymentTemplate.Funcs(sprig.TxtFuncMap()).Parse(deploymentYAML); err != nil {
		panic(fmt.Sprintf("unable to parse echo deployment template: %v", err))
	}

	vmDeploymentTemplate = template.New("echo_vm_deployment")
	if _, err := vmDeploymentTemplate.Funcs(sprig.TxtFuncMap()).Funcs(template.FuncMap{"Lines": lines}).Parse(vmDeploymentYaml); err != nil {
		panic(fmt.Sprintf("unable to parse echo vm deployment template: %v", err))
	}
}

func generateYAML(ctx resource.Context, cfg echo.Config, cluster resource.Cluster) (serviceYAML string, deploymentYAML string, err error) {
	// Create the parameters for the YAML template.
	settings, err := image.SettingsFromCommandLine()
	if err != nil {
		return "", "", err
	}
	return generateYAMLWithSettings(ctx, cfg, settings, cluster)
}

const DefaultVMImage = "app_sidecar_ubuntu_bionic"

func generateYAMLWithSettings(
	ctx resource.Context, cfg echo.Config,
	settings *image.Settings, cluster resource.Cluster) (serviceYAML string, deploymentYAML string, err error) {
	ver, err := cluster.GetKubernetesVersion()
	if err != nil {
		return "", "", err
	}
	supportStartupProbe := true
	if ver.Minor < "16" {
		// Added in Kubernetes 1.16
		supportStartupProbe = false
	}
	// Convert legacy config to workload oritended.
	if cfg.Subsets == nil {
		cfg.Subsets = []echo.SubsetConfig{
			{
				Version: cfg.Version,
			},
		}
	}

	for i := range cfg.Subsets {
		if cfg.Subsets[i].Version == "" {
			cfg.Subsets[i].Version = "v1"
		}
	}

	var vmImage, istiodIP, istiodPort string
	if cfg.DeployAsVM {
		// TODO if possible, use istioctl x workload ... to configure the VM
		ist, err := istio.Get(ctx)
		if err != nil {
			return "", "", err
		}
		addr, err := ist.RemoteDiscoveryAddressFor(cluster)
		if err != nil {
			return "", "", err
		}

		istiodIP = addr.IP.String()
		istiodPort = strconv.Itoa(addr.Port)

		// if image is not provided, default to app_sidecar
		if cfg.VMImage == "" {
			vmImage = DefaultVMImage
		} else {
			vmImage = cfg.VMImage
		}
	}
	namespace := ""
	if cfg.Namespace != nil {
		namespace = cfg.Namespace.Name()
	}
	params := map[string]interface{}{
		"Hub":                settings.Hub,
		"Tag":                strings.TrimSuffix(settings.Tag, "-distroless"),
		"PullPolicy":         settings.PullPolicy,
		"Service":            cfg.Service,
		"Version":            cfg.Version,
		"Headless":           cfg.Headless,
		"Locality":           cfg.Locality,
		"ServiceAccount":     cfg.ServiceAccount,
		"Ports":              cfg.Ports,
		"WorkloadOnlyPorts":  cfg.WorkloadOnlyPorts,
		"ContainerPorts":     getContainerPorts(cfg.Ports),
		"ServiceAnnotations": cfg.ServiceAnnotations,
		"Subsets":            cfg.Subsets,
		"TLSSettings":        cfg.TLSSettings,
		"Cluster":            cfg.Cluster.Name(),
		"Namespace":          namespace,
		"VM": map[string]interface{}{
			"Image":        vmImage,
			"IstiodIP":     istiodIP,
			"IstiodPort":   istiodPort,
			"AutoRegister": cfg.AutoRegisterVM,
		},
		"Environment":  cfg.VMEnvironment,
		"StartupProbe": supportStartupProbe,
	}

	serviceYAML, err = tmpl.Execute(serviceTemplate, params)
	if err != nil {
		return
	}

	deploy := deploymentTemplate
	if cfg.DeployAsVM {
		deploy = vmDeploymentTemplate
	}

	// Generate the YAML content.
	deploymentYAML, err = tmpl.Execute(deploy, params)
	return
}

func lines(input string) []string {
	out := []string{}
	scanner := bufio.NewScanner(strings.NewReader(input))
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}
	return out
}
