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

package grpcxds

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/file"
)

// Bootstrap contains the general structure of what's expected by GRPC's XDS implementation.
// See https://github.com/grpc/grpc-go/blob/master/xds/internal/xdsclient/bootstrap/bootstrap.go
// TODO use structs from gRPC lib if created/exported
type Bootstrap struct {
	XDSServers    []XdsServer                    `json:"xds_servers,omitempty"`
	Node          *corev3.Node                   `json:"node,omitempty"`
	CertProviders map[string]CertificateProvider `json:"certificate_providers,omitempty"`
}

type ChannelCreds struct {
	Type   string      `json:"type,omitempty"`
	Config interface{} `json:"config,omitempty"`
}

type XdsServer struct {
	ServerURI      string         `json:"server_uri,omitempty"`
	ChannelCreds   []ChannelCreds `json:"channel_creds,omitempty"`
	ServerFeatures []string       `json:"server_features,omitempty"`
}

type CertificateProvider struct {
	Name   string      `json:"name,omitempty"`
	Config interface{} `json:"config,omitempty"`
}

const FileWatcherCertProviderName = "file_watcher"

type FileWatcherCertProviderConfig struct {
	CertificateFile   string               `json:"certificate_file,omitempty"`
	PrivateKeyFile    string               `json:"private_key_file,omitempty"`
	CACertificateFile string               `json:"ca_certificate_file,omitempty"`
	RefreshDuration   *durationpb.Duration `json:"refresh_interval,omitempty"`
}

func (c *FileWatcherCertProviderConfig) FilePaths() []string {
	return []string{c.CertificateFile, c.PrivateKeyFile, c.CACertificateFile}
}

// FileWatcherProvider returns the FileWatcherCertProviderConfig if one exists in CertProviders
func (b *Bootstrap) FileWatcherProvider() *FileWatcherCertProviderConfig {
	if b == nil || b.CertProviders == nil {
		return nil
	}
	for _, provider := range b.CertProviders {
		if provider.Name == FileWatcherCertProviderName {
			cfg, ok := provider.Config.(FileWatcherCertProviderConfig)
			if !ok {
				return nil
			}
			return &cfg
		}
	}
	return nil
}

// LoadBootstrap loads a Bootstrap from the given file path.
func LoadBootstrap(file string) (*Bootstrap, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	b := &Bootstrap{}
	if err := json.Unmarshal(data, b); err != nil {
		return nil, err
	}
	return b, err
}

type GenerateBootstrapOptions struct {
	Node             *model.Node
	ProxyXDSViaAgent bool
	XdsUdsPath       string
	DiscoveryAddress string
	CertDir          string
}

// GenerateBootstrap generates the bootstrap structure for gRPC XDS integration.
func GenerateBootstrap(opts GenerateBootstrapOptions) (*Bootstrap, error) {
	xdsMeta, err := structpb.NewStruct(opts.Node.RawMetadata)
	if err != nil {
		return nil, fmt.Errorf("failed converting to xds metadata: %v", err)
	}

	// TODO direct to CP should use secure channel (most likely JWT + TLS, but possibly allow mTLS)
	serverURI := opts.DiscoveryAddress
	if opts.ProxyXDSViaAgent && opts.XdsUdsPath != "" {
		serverURI = fmt.Sprintf("unix:///%s", opts.XdsUdsPath)
	}

	bootstrap := Bootstrap{
		XDSServers: []XdsServer{{
			ServerURI: serverURI,
			// connect locally via agent
			ChannelCreds:   []ChannelCreds{{Type: "insecure"}},
			ServerFeatures: []string{"xds_v3"},
		}},
		Node: &corev3.Node{
			Id:       opts.Node.ID,
			Locality: opts.Node.Locality,
			Metadata: xdsMeta,
		},
	}

	if opts.CertDir != "" {
		bootstrap.CertProviders = map[string]CertificateProvider{
			"default": {
				Name: "file_watcher",
				Config: FileWatcherCertProviderConfig{
					PrivateKeyFile:    path.Join(opts.CertDir, "key.pem"),
					CertificateFile:   path.Join(opts.CertDir, "cert-chain.pem"),
					CACertificateFile: path.Join(opts.CertDir, "root-cert.pem"),
					// TODO use a more appropriate interval
					RefreshDuration: durationpb.New(15 * time.Minute),
				},
			},
		}
	}

	return &bootstrap, err
}

// GenerateBootstrapFile generates and writes atomically as JSON to the given file path.
func GenerateBootstrapFile(opts GenerateBootstrapOptions, path string) (*Bootstrap, error) {
	bootstrap, err := GenerateBootstrap(opts)
	if err != nil {
		return nil, err
	}
	jsonData, err := json.MarshalIndent(bootstrap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := file.AtomicWrite(path, jsonData, os.FileMode(0o644)); err != nil {
		return nil, fmt.Errorf("failed writing to %s: %v", path, err)
	}
	return bootstrap, nil
}
