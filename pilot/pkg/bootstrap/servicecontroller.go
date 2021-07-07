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
	"fmt"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pilot/pkg/serviceregistry/aggregate"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pilot/pkg/serviceregistry/mock"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pilot/pkg/serviceregistry/serviceentry"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/kube/secretcontroller"
	"istio.io/pkg/log"
)

func (s *Server) ServiceController() *aggregate.Controller {
	return s.environment.ServiceDiscovery.(*aggregate.Controller)
}

// initServiceControllers creates and initializes the service controllers
func (s *Server) initServiceControllers(args *PilotArgs) error {
	serviceControllers := s.ServiceController()

	s.serviceEntryStore = serviceentry.NewServiceDiscovery(
		s.configController, s.environment.IstioConfigStore, s.XDSServer,
		serviceentry.WithClusterID(s.clusterID),
	)
	serviceControllers.AddRegistry(s.serviceEntryStore)

	registered := make(map[provider.ID]bool)
	for _, r := range args.RegistryOptions.Registries {
		serviceRegistry := provider.ID(r)
		if _, exists := registered[serviceRegistry]; exists {
			log.Warnf("%s registry specified multiple times.", r)
			continue
		}
		registered[serviceRegistry] = true
		log.Infof("Adding %s registry adapter", serviceRegistry)
		switch serviceRegistry {
		case provider.Kubernetes:
			if err := s.initKubeRegistry(args); err != nil {
				return err
			}
		case provider.Mock:
			s.initMockRegistry()
		default:
			return fmt.Errorf("service registry %s is not supported", r)
		}
	}

	// Defer running of the service controllers.
	s.addStartFunc(func(stop <-chan struct{}) error {
		go serviceControllers.Run(stop)
		return nil
	})

	return nil
}

// initKubeRegistry creates all the k8s service controllers under this pilot
func (s *Server) initKubeRegistry(args *PilotArgs) (err error) {
	args.RegistryOptions.KubeOptions.ClusterID = s.clusterID
	args.RegistryOptions.KubeOptions.Metrics = s.environment
	args.RegistryOptions.KubeOptions.XDSUpdater = s.XDSServer
	args.RegistryOptions.KubeOptions.NetworksWatcher = s.environment.NetworksWatcher
	args.RegistryOptions.KubeOptions.MeshWatcher = s.environment.Watcher
	args.RegistryOptions.KubeOptions.SystemNamespace = args.Namespace

	mc := kubecontroller.NewMulticluster(args.PodName,
		s.kubeClient,
		args.RegistryOptions.ClusterRegistriesNamespace,
		args.RegistryOptions.KubeOptions,
		s.ServiceController(),
		s.serviceEntryStore,
		s.istiodCertBundleWatcher,
		args.Revision,
		s.fetchCARoot,
		s.environment.ClusterLocal(),
		s.server)

	// initialize the "main" cluster registry before starting controllers for remote clusters
	s.addStartFunc(func(stop <-chan struct{}) error {
		writableStop := make(chan struct{})
		go func() {
			<-stop
			close(writableStop)
		}()
		if err := mc.AddMemberCluster(args.RegistryOptions.KubeOptions.ClusterID, &secretcontroller.Cluster{
			Client: s.kubeClient,
			Stop:   writableStop,
		}); err != nil {
			return fmt.Errorf("failed initializing registry for %s: %v", args.RegistryOptions.KubeOptions.ClusterID, err)
		}
		return nil
	})

	// Start the multicluster controller and wait for it to shutdown before exiting the server.
	s.addTerminatingStartFunc(mc.Run)

	// start remote cluster controllers
	s.addStartFunc(func(stop <-chan struct{}) error {
		mc.InitSecretController(stop)
		return nil
	})

	s.multicluster = mc
	return
}

func (s *Server) initMockRegistry() {
	// MemServiceDiscovery implementation
	discovery := mock.NewDiscovery(map[host.Name]*model.Service{}, 2)

	registry := serviceregistry.Simple{
		ProviderID:       provider.Mock,
		ServiceDiscovery: discovery,
		Controller:       &mock.Controller{},
	}

	s.ServiceController().AddRegistry(registry)
}
