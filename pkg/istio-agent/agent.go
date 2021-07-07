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

package istioagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/gogo/protobuf/types"
	"github.com/golang/protobuf/jsonpb"

	mesh "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/cmd/pilot-agent/config"
	"istio.io/istio/pilot/pkg/dns"
	"istio.io/istio/pilot/pkg/model"
	nds "istio.io/istio/pilot/pkg/proto"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pkg/bootstrap"
	"istio.io/istio/pkg/bootstrap/platform"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/envoy"
	"istio.io/istio/pkg/istio-agent/grpcxds"
	"istio.io/istio/pkg/security"
	"istio.io/istio/security/pkg/nodeagent/cache"
	"istio.io/istio/security/pkg/nodeagent/caclient"
	citadel "istio.io/istio/security/pkg/nodeagent/caclient/providers/citadel"
	gca "istio.io/istio/security/pkg/nodeagent/caclient/providers/google"
	"istio.io/istio/security/pkg/nodeagent/sds"
	"istio.io/pkg/log"
)

// To debug:
// curl -X POST localhost:15000/logging?config=trace - to see SendingDiscoveryRequest

// Breakpoints in secretcache.go GenerateSecret..

// Note that istiod currently can't validate the JWT token unless it runs on k8s
// Main problem is the JWT validation check which hardcodes the k8s server address and token location.
//
// To test on a local machine, for debugging:
//
// kis exec $POD -- cat /run/secrets/istio-token/istio-token > var/run/secrets/tokens/istio-token
// kis port-forward $POD 15010:15010 &
//
// You can also copy the K8S CA and a token to be used to connect to k8s - but will need removing the hardcoded addr
// kis exec $POD -- cat /run/secrets/kubernetes.io/serviceaccount/{ca.crt,token} > var/run/secrets/kubernetes.io/serviceaccount/
//
// Or disable the jwt validation while debugging SDS problems.

const (
	// Location of K8S CA root.
	k8sCAPath = "./var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	// CitadelCACertPath is the directory for Citadel CA certificate.
	// This is mounted from config map 'istio-ca-root-cert'. Part of startup,
	// this may be replaced with ./etc/certs, if a root-cert.pem is found, to
	// handle secrets mounted from non-citadel CAs.
	CitadelCACertPath = "./var/run/secrets/istio"
)

const (
	MetadataClientCertKey   = "ISTIO_META_TLS_CLIENT_KEY"
	MetadataClientCertChain = "ISTIO_META_TLS_CLIENT_CERT_CHAIN"
	MetadataClientRootCert  = "ISTIO_META_TLS_CLIENT_ROOT_CERT"
)

// Agent contains the configuration of the agent, based on the injected
// environment:
// - SDS hostPath if node-agent was used
// - /etc/certs/key if Citadel or other mounted Secrets are used
// - root cert to use for connecting to XDS server
// - CA address, with proper defaults and detection
type Agent struct {
	proxyConfig *mesh.ProxyConfig

	cfg       *AgentOptions
	secOpts   *security.Options
	envoyOpts envoy.ProxyConfig

	envoyAgent  *envoy.Agent
	envoyWaitCh chan error

	sdsServer   *sds.Server
	secretCache *cache.SecretManagerClient

	// Used when proxying envoy xds via istio-agent is enabled.
	xdsProxy *XdsProxy

	// local DNS Server that processes DNS requests locally and forwards to upstream DNS if needed.
	localDNSServer *dns.LocalDNSServer

	// Signals true completion (e.g. with delayed graceful termination of Envoy)
	wg sync.WaitGroup
}

// AgentOptions contains additional config for the agent, not included in ProxyConfig.
// Most are from env variables ( still experimental ) or for testing only.
// Eventually most non-test settings should graduate to ProxyConfig
// Please don't add 100 parameters to the NewAgent function (or any other)!
type AgentOptions struct {
	// ProxyXDSViaAgent if true will enable a local XDS proxy that will simply
	// ferry Envoy's XDS requests to istiod and responses back to envoy
	// This flag is temporary until the feature is stabilized.
	ProxyXDSViaAgent bool
	// ProxyXDSDebugViaAgent if true will listen on 15004 and forward queries
	// to XDS istio.io/debug. (Requires ProxyXDSViaAgent).
	ProxyXDSDebugViaAgent bool
	// DNSCapture indicates if the XDS proxy has dns capture enabled or not
	// This option will not be considered if proxyXDSViaAgent is false.
	DNSCapture bool
	// ProxyType is the type of proxy we are configured to handle
	ProxyType model.NodeType
	// ProxyNamespace to use for local dns resolution
	ProxyNamespace string
	// ProxyDomain is the DNS domain associated with the proxy (assumed
	// to include the namespace as well) (for local dns resolution)
	ProxyDomain string
	// Node identifier used by Envoy
	ServiceNode string

	// XDSRootCerts is the location of the root CA for the XDS connection. Used for setting platform certs or
	// using custom roots.
	XDSRootCerts string

	// CARootCerts of the location of the root CA for the CA connection. Used for setting platform certs or
	// using custom roots.
	CARootCerts string

	// Extra headers to add to the XDS connection.
	XDSHeaders map[string]string

	// Is the proxy an IPv6 proxy
	IsIPv6 bool

	// Path to local UDS to communicate with Envoy
	XdsUdsPath string

	// Ability to retrieve ProxyConfig dynamically through XDS
	EnableDynamicProxyConfig bool

	// All of the proxy's IP Addresses
	ProxyIPAddresses []string

	// Enables dynamic generation of bootstrap.
	EnableDynamicBootstrap bool

	// Envoy status port (that circles back to the agent status port). Really belongs to the proxy config.
	// Cannot be eradicated because mistakes have been made.
	EnvoyStatusPort int

	// Envoy prometheus port that circles back to its admin port for prom endpoint. Really belongs to the
	// proxy config.
	EnvoyPrometheusPort int

	// Cloud platform
	Platform platform.Environment

	// GRPCBootstrapPath if set will generate a file compatible with GRPC_XDS_BOOTSTRAP
	GRPCBootstrapPath string

	// Disables all envoy agent features
	DisableEnvoy bool
}

// NewAgent hosts the functionality for local SDS and XDS. This consists of the local SDS server and
// associated clients to sign certificates (when not using files), and the local XDS proxy (including
// health checking for VMs and DNS proxying).
func NewAgent(proxyConfig *mesh.ProxyConfig, agentOpts *AgentOptions, sopts *security.Options,
	eopts envoy.ProxyConfig) *Agent {
	return &Agent{
		proxyConfig: proxyConfig,
		cfg:         agentOpts,
		secOpts:     sopts,
		envoyOpts:   eopts,
	}
}

// EnvoyDisabled if true inidcates calling Run will not run and wait for Envoy.
func (a *Agent) EnvoyDisabled() bool {
	return a.envoyOpts.TestOnly || a.cfg.DisableEnvoy
}

// WaitForSigterm if true indicates calling Run will block until SIGKILL is received.
func (a *Agent) WaitForSigterm() bool {
	return a.EnvoyDisabled() && !a.envoyOpts.TestOnly
}

func (a *Agent) generateNodeMetadata() (*model.Node, error) {
	provCert, err := a.FindRootCAForXDS()
	if err != nil {
		return nil, fmt.Errorf("failed to find root CA cert for XDS: %v", err)
	}

	if provCert == "" {
		// Envoy only supports load from file. If we want to use system certs, use best guess
		// To be more correct this could lookup all the "well known" paths but this is extremely \
		// unlikely to run on a non-debian based machine, and if it is it can be explicitly configured
		provCert = "/etc/ssl/certs/ca-certificates.crt"
	}
	var pilotSAN []string
	if a.proxyConfig.ControlPlaneAuthPolicy == mesh.AuthenticationPolicy_MUTUAL_TLS {
		// Obtain Pilot SAN, using DNS.
		pilotSAN = []string{config.GetPilotSan(a.proxyConfig.DiscoveryAddress)}
	}
	log.Infof("Pilot SAN: %v", pilotSAN)

	return bootstrap.GetNodeMetaData(bootstrap.MetadataOptions{
		ID:                  a.cfg.ServiceNode,
		Envs:                os.Environ(),
		Platform:            a.cfg.Platform,
		InstanceIPs:         a.cfg.ProxyIPAddresses,
		StsPort:             a.secOpts.STSPort,
		ProxyConfig:         a.proxyConfig,
		ProxyViaAgent:       a.cfg.ProxyXDSViaAgent,
		PilotSubjectAltName: pilotSAN,
		OutlierLogPath:      a.envoyOpts.OutlierLogPath,
		ProvCert:            provCert,
		EnvoyPrometheusPort: a.cfg.EnvoyPrometheusPort,
		EnvoyStatusPort:     a.cfg.EnvoyStatusPort,
	})
}

func (a *Agent) initializeEnvoyAgent(ctx context.Context) error {
	node, err := a.generateNodeMetadata()
	if err != nil {
		return fmt.Errorf("failed to generate bootstrap metadata: %v", err)
	}

	// Note: the cert checking still works, the generated file is updated if certs are changed.
	// We just don't save the generated file, but use a custom one instead. Pilot will keep
	// monitoring the certs and restart if the content of the certs changes.
	if len(a.proxyConfig.CustomConfigFile) > 0 {
		// there is a custom configuration. Don't write our own config - but keep watching the certs.
		a.envoyOpts.ConfigPath = a.proxyConfig.CustomConfigFile
		a.envoyOpts.ConfigCleanup = false
	} else {
		out, err := bootstrap.New(bootstrap.Config{
			Node: node,
		}).CreateFileForEpoch(0)
		if err != nil {
			return fmt.Errorf("failed to generate bootstrap config: %v", err)
		}
		a.envoyOpts.ConfigPath = out
		a.envoyOpts.ConfigCleanup = true
	}

	// Back-fill envoy options from proxy config options
	a.envoyOpts.BinaryPath = a.proxyConfig.BinaryPath
	a.envoyOpts.AdminPort = a.proxyConfig.ProxyAdminPort
	a.envoyOpts.DrainDuration = a.proxyConfig.DrainDuration
	a.envoyOpts.ParentShutdownDuration = a.proxyConfig.ParentShutdownDuration
	a.envoyOpts.Concurrency = a.proxyConfig.Concurrency.GetValue()

	envoyProxy := envoy.NewProxy(a.envoyOpts)

	drainDuration, _ := types.DurationFromProto(a.proxyConfig.TerminationDrainDuration)
	a.envoyAgent = envoy.NewAgent(envoyProxy, drainDuration)
	a.envoyWaitCh = make(chan error, 1)
	if a.cfg.EnableDynamicBootstrap {
		// Simulate an xDS request for a bootstrap
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()

			// wait indefinitely and keep retrying with jittered exponential backoff
			backoff := 500
			max := 30000
		retries:
			for {
				// handleStream hands on to request after exit, so create a fresh one instead.
				request := &bootstrapDiscoveryRequest{
					node:        node,
					envoyWaitCh: a.envoyWaitCh,
					envoyUpdate: envoyProxy.UpdateConfig,
				}
				_ = a.xdsProxy.handleStream(request)
				select {
				case <-a.envoyWaitCh:
					break retries
				default:
				}
				delay := time.Duration(rand.Int()%backoff) * time.Millisecond
				log.Infof("retrying bootstrap discovery request with backoff: %v", delay)
				select {
				case <-ctx.Done():
					break
				case <-time.After(delay):
				}
				if backoff < max/2 {
					backoff *= 2
				} else {
					backoff = max
				}
			}
		}()
	} else {
		close(a.envoyWaitCh)
	}
	return nil
}

type bootstrapDiscoveryRequest struct {
	node        *model.Node
	envoyWaitCh chan error
	envoyUpdate func(data []byte) error
	sent        bool
	received    bool
}

// Send refers to a request from the xDS proxy.
func (b *bootstrapDiscoveryRequest) Send(resp *discovery.DiscoveryResponse) error {
	if resp.TypeUrl == v3.BootstrapType && !b.received {
		b.received = true
		if len(resp.Resources) != 1 {
			b.envoyWaitCh <- fmt.Errorf("unexpected number of bootstraps: %d", len(resp.Resources))
			return nil
		}
		var bs bootstrapv3.Bootstrap
		if err := resp.Resources[0].UnmarshalTo(&bs); err != nil {
			b.envoyWaitCh <- fmt.Errorf("failed to unmarshal bootstrap: %v", err)
			return nil
		}
		js := jsonpb.Marshaler{OrigName: true, Indent: "  "}
		var buf bytes.Buffer
		if err := js.Marshal(&buf, &bs); err != nil {
			b.envoyWaitCh <- fmt.Errorf("failed to marshal bootstrap as JSON: %v", err)
			return nil
		}
		if err := b.envoyUpdate(buf.Bytes()); err != nil {
			b.envoyWaitCh <- fmt.Errorf("failed to update bootstrap from discovery: %v", err)
			return nil
		}
		close(b.envoyWaitCh)
	}
	return nil
}

// Receive refers to a request to the xDS proxy.
func (b *bootstrapDiscoveryRequest) Recv() (*discovery.DiscoveryRequest, error) {
	if b.sent {
		<-b.envoyWaitCh
		return nil, io.EOF
	}
	b.sent = true
	return &discovery.DiscoveryRequest{
		TypeUrl: v3.BootstrapType,
		Node:    bootstrap.ConvertNodeToXDSNode(b.node),
	}, nil
}

func (b *bootstrapDiscoveryRequest) Context() context.Context { return context.Background() }

// Simplified SDS setup.
//
// 1. External CA: requires authenticating the trusted JWT AND validating the SAN against the JWT.
//    For example Google CA
//
// 2. Indirect, using istiod: using K8S cert.
//
// This is a non-blocking call which returns either an error or a function to await for completion.
func (a *Agent) Run(ctx context.Context) (func(), error) {
	var err error
	a.secretCache, err = a.newSecretManager()
	if err != nil {
		return nil, fmt.Errorf("failed to start workload secret manager %v", err)
	}

	a.sdsServer = sds.NewServer(a.secOpts, a.secretCache)
	a.secretCache.SetUpdateCallback(a.sdsServer.UpdateCallback)

	if err = a.initLocalDNSServer(); err != nil {
		return nil, fmt.Errorf("failed to start local DNS server: %v", err)
	}

	if a.cfg.ProxyXDSViaAgent {
		a.xdsProxy, err = initXdsProxy(a)
		if err != nil {
			return nil, fmt.Errorf("failed to start xds proxy: %v", err)
		}
		if a.cfg.ProxyXDSDebugViaAgent {
			err = a.xdsProxy.initDebugInterface()
			if err != nil {
				return nil, fmt.Errorf("failed to start istio tap server: %v", err)
			}
		}
	}

	if a.cfg.GRPCBootstrapPath != "" {
		if err := a.generateGRPCBootstrap(); err != nil {
			return nil, fmt.Errorf("failed generating gRPC XDS bootstrap: %v", err)
		}
	}

	if !a.EnvoyDisabled() {
		err = a.initializeEnvoyAgent(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to start envoy agent: %v", err)
		}

		a.wg.Add(1)
		go func() {
			defer a.wg.Done()

			if a.cfg.EnableDynamicBootstrap {
				start := time.Now()
				var err error
				select {
				case err = <-a.envoyWaitCh:
				case <-ctx.Done():
					// Early cancellation before envoy started.
					return
				}
				if err != nil {
					log.Errorf("failed to write updated envoy bootstrap: %v", err)
					return
				}
				log.Infof("received server-side bootstrap in %v", time.Since(start))
			}

			// This is a blocking call for graceful termination.
			a.envoyAgent.Run(ctx)
		}()
	} else if a.WaitForSigterm() {
		// wait for SIGTERM and perform graceful shutdown
		stop := make(chan os.Signal)
		signal.Notify(stop, syscall.SIGTERM)
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			<-stop
		}()
	}

	return a.wg.Wait, nil
}

func (a *Agent) initLocalDNSServer() (err error) {
	// we dont need dns server on gateways
	if a.cfg.DNSCapture && a.cfg.ProxyXDSViaAgent && a.cfg.ProxyType == model.SidecarProxy {
		if a.localDNSServer, err = dns.NewLocalDNSServer(a.cfg.ProxyNamespace, a.cfg.ProxyDomain); err != nil {
			return err
		}
		a.localDNSServer.StartDNS()
	}
	return nil
}

func (a *Agent) generateGRPCBootstrap() error {
	// generate metadata
	node, err := a.generateNodeMetadata()
	if err != nil {
		return fmt.Errorf("failed generating node metadata: %v", err)
	}

	_, err = grpcxds.GenerateBootstrapFile(grpcxds.GenerateBootstrapOptions{
		Node:             node,
		ProxyXDSViaAgent: a.cfg.ProxyXDSViaAgent,
		XdsUdsPath:       a.cfg.XdsUdsPath,
		DiscoveryAddress: a.proxyConfig.DiscoveryAddress,
		CertDir:          a.secOpts.OutputKeyCertToDir,
	}, a.cfg.GRPCBootstrapPath)
	if err != nil {
		return err
	}
	return nil
}

func (a *Agent) Check() (err error) {
	// we dont need dns server on gateways
	if a.cfg.DNSCapture && a.cfg.ProxyXDSViaAgent && a.cfg.ProxyType == model.SidecarProxy {
		if !a.localDNSServer.IsReady() {
			return errors.New("istio DNS capture is turned ON and DNS lookup table is not ready yet")
		}
	}
	return nil
}

func (a *Agent) GetDNSTable() *nds.NameTable {
	if a.localDNSServer != nil {
		return a.localDNSServer.NameTable()
	}
	return nil
}

func (a *Agent) Close() {
	if a.xdsProxy != nil {
		a.xdsProxy.close()
	}
	if a.localDNSServer != nil {
		a.localDNSServer.Close()
	}
	if a.sdsServer != nil {
		a.sdsServer.Stop()
	}
	if a.secretCache != nil {
		a.secretCache.Close()
	}
}

// FindRootCAForXDS determines the root CA to be configured in bootstrap file.
// It may be different from the CA for the cert server - which is based on CA_ADDR
// In addition it deals with the case the XDS server is on port 443, expected with a proper cert.
// /etc/ssl/certs/ca-certificates.crt
func (a *Agent) FindRootCAForXDS() (string, error) {
	var rootCAPath string

	if a.cfg.XDSRootCerts == security.SystemRootCerts {
		// Special case input for root cert configuration to use system root certificates
		return "", nil
	} else if a.cfg.XDSRootCerts != "" {
		// Using specific platform certs or custom roots
		rootCAPath = a.cfg.XDSRootCerts
	} else if fileExists(security.DefaultRootCertFilePath) {
		// Old style - mounted cert. This is used for XDS auth only,
		// not connecting to CA_ADDR because this mode uses external
		// agent (Secret refresh, etc)
		return security.DefaultRootCertFilePath, nil
	} else if a.secOpts.PilotCertProvider == constants.CertProviderKubernetes {
		// Using K8S - this is likely incorrect, may work by accident (https://github.com/istio/istio/issues/22161)
		rootCAPath = k8sCAPath
	} else if a.secOpts.ProvCert != "" {
		// This was never completely correct - PROV_CERT are only intended for auth with CA_ADDR,
		// and should not be involved in determining the root CA.
		// For VMs, the root cert file used to auth may be populated afterwards.
		// Thus, return directly here and skip checking for existence.
		return a.secOpts.ProvCert + "/root-cert.pem", nil
	} else if a.secOpts.FileMountedCerts {
		// FileMountedCerts - Load it from Proxy Metadata.
		rootCAPath = a.proxyConfig.ProxyMetadata[MetadataClientRootCert]
	} else if a.secOpts.PilotCertProvider == constants.CertProviderNone {
		return "", fmt.Errorf("root CA file for XDS required but configured provider as none")
	} else {
		// PILOT_CERT_PROVIDER - default is istiod
		// This is the default - a mounted config map on K8S
		rootCAPath = path.Join(CitadelCACertPath, constants.CACertNamespaceConfigMapDataName)
	}

	// Additional checks for root CA cert existence. Fail early, instead of obscure envoy errors
	if fileExists(rootCAPath) {
		return rootCAPath, nil
	}

	return "", fmt.Errorf("root CA file for XDS does not exist %s", rootCAPath)
}

func fileExists(path string) bool {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		return true
	}
	return false
}

// Find the root CA to use when connecting to the CA (Istiod or external).
func (a *Agent) FindRootCAForCA() (string, error) {
	var rootCAPath string

	if a.cfg.CARootCerts == security.SystemRootCerts {
		return "", nil
	} else if a.cfg.CARootCerts != "" {
		rootCAPath = a.cfg.CARootCerts
	} else if a.secOpts.PilotCertProvider == constants.CertProviderKubernetes {
		// Using K8S - this is likely incorrect, may work by accident.
		// API is GA.
		rootCAPath = k8sCAPath // ./var/run/secrets/kubernetes.io/serviceaccount/ca.crt
	} else if a.secOpts.PilotCertProvider == constants.CertProviderCustom {
		rootCAPath = security.DefaultRootCertFilePath // ./etc/certs/root-cert.pem
	} else if a.secOpts.ProvCert != "" {
		// This was never completely correct - PROV_CERT are only intended for auth with CA_ADDR,
		// and should not be involved in determining the root CA.
		// For VMs, the root cert file used to auth may be populated afterwards.
		// Thus, return directly here and skip checking for existence.
		return a.secOpts.ProvCert + "/root-cert.pem", nil
	} else if a.secOpts.PilotCertProvider == constants.CertProviderNone {
		return "", fmt.Errorf("root CA file for CA required but configured provider as none")
	} else {
		// This is the default - a mounted config map on K8S
		rootCAPath = path.Join(CitadelCACertPath, constants.CACertNamespaceConfigMapDataName)
		// or: "./var/run/secrets/istio/root-cert.pem"
	}

	// Additional checks for root CA cert existence.
	if fileExists(rootCAPath) {
		return rootCAPath, nil
	}

	return "", fmt.Errorf("root CA file for CA does not exist %s", rootCAPath)
}

// newSecretManager creates the SecretManager for workload secrets
func (a *Agent) newSecretManager() (*cache.SecretManagerClient, error) {
	// If proxy is using file mounted certs, we do not have to connect to CA.
	if a.secOpts.FileMountedCerts {
		log.Info("Workload is using file mounted certificates. Skipping connecting to CA")
		return cache.NewSecretManagerClient(nil, a.secOpts)
	}

	log.Infof("CA Endpoint %s, provider %s", a.secOpts.CAEndpoint, a.secOpts.CAProviderName)

	// TODO: this should all be packaged in a plugin, possibly with optional compilation.
	if a.secOpts.CAProviderName == security.GoogleCAProvider {
		// Use a plugin to an external CA - this has direct support for the K8S JWT token
		// This is only used if the proper env variables are injected - otherwise the existing Citadel or Istiod will be
		// used.
		caClient, err := gca.NewGoogleCAClient(a.secOpts.CAEndpoint, true, caclient.NewCATokenProvider(a.secOpts))
		if err != nil {
			return nil, err
		}
		return cache.NewSecretManagerClient(caClient, a.secOpts)
	}

	// Using citadel CA
	var rootCert []byte
	var err error
	// Special case: if Istiod runs on a secure network, on the default port, don't use TLS
	// TODO: may add extra cases or explicit settings - but this is a rare use cases, mostly debugging
	tls := true
	if strings.HasSuffix(a.secOpts.CAEndpoint, ":15010") {
		tls = false
		log.Warn("Debug mode or IP-secure network")
	}
	if tls {
		caCertFile, err := a.FindRootCAForCA()
		if err != nil {
			return nil, fmt.Errorf("failed to find root CA cert for CA: %v", err)
		}

		if caCertFile == "" {
			log.Infof("Using CA %s cert with system certs", a.secOpts.CAEndpoint)
		} else if rootCert, err = ioutil.ReadFile(caCertFile); err != nil {
			log.Fatalf("invalid config - %s missing a root certificate %s", a.secOpts.CAEndpoint, caCertFile)
		} else {
			log.Infof("Using CA %s cert with certs: %s", a.secOpts.CAEndpoint, caCertFile)
		}
	}

	// Will use TLS unless the reserved 15010 port is used ( istiod on an ipsec/secure VPC)
	// rootCert may be nil - in which case the system roots are used, and the CA is expected to have public key
	// Otherwise assume the injection has mounted /etc/certs/root-cert.pem
	caClient, err := citadel.NewCitadelClient(a.secOpts, tls, rootCert)
	if err != nil {
		return nil, err
	}

	return cache.NewSecretManagerClient(caClient, a.secOpts)
}

// GRPCBootstrapPath returns the most recently generated gRPC bootstrap or nil if there is none.
func (a *Agent) GRPCBootstrapPath() string {
	return a.cfg.GRPCBootstrapPath
}
