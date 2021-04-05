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

package bootstrap

import (
	"encoding/json"
	"os"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/kubemesh"
	"istio.io/istio/pkg/util/gogoprotomarshal"
	"istio.io/pkg/filewatcher"
	"istio.io/pkg/log"
	"istio.io/pkg/version"
)

const (
	// defaultMeshConfigMapName is the default name of the ConfigMap with the mesh config
	// The actual name can be different - use getMeshConfigMapName
	defaultMeshConfigMapName = "istio"
	// configMapKey should match the expected MeshConfig file name
	configMapKey = "mesh"
)

// initMeshConfiguration creates the mesh in the pilotConfig from the input arguments.
// Original/default behavior:
// - use the mounted file, if it exists.
// - use istio-REVISION if k8s is enabled
// - fallback to default
//
// If the 'SHARED_MESH_CONFIG' env is set (experimental feature in 1.10):
// - if a file exist, load it - will be merged
// - if istio-REVISION exists, will be used, even if the file is present.
// - the SHARED_MESH_CONFIG config map will also be loaded and merged.
func (s *Server) initMeshConfiguration(args *PilotArgs, fileWatcher filewatcher.FileWatcher) {
	log.Info("initializing mesh configuration ", args.MeshConfigFile)
	defer func() {
		if s.environment.Watcher != nil {
			meshdump, _ := gogoprotomarshal.ToJSONWithIndent(s.environment.Mesh(), "    ")
			log.Infof("mesh configuration: %s", meshdump)
			log.Infof("version: %s", version.Info.String())
			argsdump, _ := json.MarshalIndent(args, "", "   ")
			log.Infof("flags: %s", argsdump)
		}
	}()

	var err error
	if _, err = os.Stat(args.MeshConfigFile); !os.IsNotExist(err) {
		s.environment.Watcher, err = mesh.NewFileWatcher(fileWatcher, args.MeshConfigFile)
		if err == nil {
			// Normal install no longer uses this mode - testing and special installs still use this.
			log.Warnf("Using local mesh config file, in cluster configs ignored %s", args.MeshConfigFile)
			return
		}
	}

	// Config file either didn't exist or failed to load.
	if s.kubeClient == nil {
		// Use a default mesh.
		meshConfig := mesh.DefaultMeshConfig()
		s.environment.Watcher = mesh.NewFixedWatcher(&meshConfig)
		log.Warnf("Using default mesh - missing file %s and no k8s client", args.MeshConfigFile)
		return
	}

	// Watch the istio ConfigMap for mesh config changes.
	// This may be necessary for external Istiod.
	configMapName := getMeshConfigMapName(args.Revision)
	s.environment.Watcher = kubemesh.NewConfigMapWatcher(
		s.kubeClient, args.Namespace, configMapName, configMapKey, features.SharedMeshConfig)
}

// initMeshNetworks loads the mesh networks configuration from the file provided
// in the args and add a watcher for changes in this file.
func (s *Server) initMeshNetworks(args *PilotArgs, fileWatcher filewatcher.FileWatcher) {
	log.Info("initializing mesh networks")
	if args.NetworksConfigFile != "" {
		var err error
		s.environment.NetworksWatcher, err = mesh.NewNetworksWatcher(fileWatcher, args.NetworksConfigFile)
		if err != nil {
			log.Info(err)
		}
	}

	if s.environment.NetworksWatcher == nil {
		log.Info("mesh networks configuration not provided")
		s.environment.NetworksWatcher = mesh.NewFixedNetworksWatcher(nil)
	}
}

func getMeshConfigMapName(revision string) string {
	name := defaultMeshConfigMapName
	if revision == "" || revision == "default" {
		return name
	}
	return name + "-" + revision
}
