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

package model

import (
	"net"

	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/network"
)

// NetworkGateway is the gateway of a network
type NetworkGateway struct {
	// Network is the ID of the network where this Gateway resides.
	Network network.ID
	// Cluster is the ID of the k8s cluster where this Gateway resides.
	Cluster cluster.ID
	// gateway ip address
	Addr string
	// gateway port
	Port uint32
}

// NewNetworkManager creates a new NetworkManager from the Environment by merging
// together the MeshNetworks and ServiceRegistry-specific gateways.
func NewNetworkManager(env *Environment) *NetworkManager {
	// Generate the a snapshot of the state of gateways by merging the contents of
	// MeshNetworks and the ServiceRegistries.
	byNetwork := make(map[network.ID][]*NetworkGateway)
	byNetworkAndCluster := make(map[networkAndCluster][]*NetworkGateway)

	addGateway := func(gateway *NetworkGateway) {
		byNetwork[gateway.Network] = append(byNetwork[gateway.Network], gateway)
		nc := networkAndClusterForGateway(gateway)
		byNetworkAndCluster[nc] = append(byNetworkAndCluster[nc], gateway)
	}

	// First, load gateways from the static MeshNetworks config.
	meshNetworks := env.Networks()
	if meshNetworks != nil {
		for nw, networkConf := range meshNetworks.Networks {
			gws := networkConf.Gateways
			for _, gw := range gws {
				if gwIP := net.ParseIP(gw.GetAddress()); gwIP != nil {
					addGateway(&NetworkGateway{
						Cluster: "", /* TODO(nmittler): Add Cluster to the API */
						Network: network.ID(nw),
						Addr:    gw.GetAddress(),
						Port:    gw.Port,
					})
				} else {
					log.Warnf("Failed parsing gateway address %s in MeshNetworks config. "+
						"Hostnames are not supported for gateways",
						gw.GetAddress())
				}
			}
		}
	}

	// Second, load registry-specific gateways.
	for _, gw := range env.NetworkGateways() {
		if gwIP := net.ParseIP(gw.Addr); gwIP != nil {
			// - the internal map of label gateways - these get deleted if the service is deleted, updated if the ip changes etc.
			// - the computed map from meshNetworks (triggered by reloadNetworkLookup, the ported logic from getGatewayAddresses)
			addGateway(gw)
		} else {
			log.Warnf("Failed parsing gateway address %s from Service Registry. "+
				"Hostnames are not supported for gateways",
				gw.Addr)
		}
	}

	// Calculate the upper-bound on the number of gateways per network.
	var maxGatewaysPerNetwork int
	for _, gws := range byNetwork {
		if len(gws) > maxGatewaysPerNetwork {
			maxGatewaysPerNetwork = len(gws)
		}
	}

	return &NetworkManager{
		maxGatewaysPerNetwork: uint32(maxGatewaysPerNetwork),
		byNetwork:             byNetwork,
		byNetworkAndCluster:   byNetworkAndCluster,
	}
}

// NetworkManager provides gateway details for accessing remote networks.
type NetworkManager struct {
	maxGatewaysPerNetwork uint32
	byNetwork             map[network.ID][]*NetworkGateway
	byNetworkAndCluster   map[networkAndCluster][]*NetworkGateway
}

func (mgr *NetworkManager) IsMultiNetworkEnabled() bool {
	return len(mgr.byNetwork) > 0
}

// GetMaxGatewaysPerNetwork returns an upper bound on the number of gateways there
// could be for any one network.
func (mgr *NetworkManager) GetMaxGatewaysPerNetwork() uint32 {
	return mgr.maxGatewaysPerNetwork
}

func (mgr *NetworkManager) AllGateways() []*NetworkGateway {
	out := make([]*NetworkGateway, 0)
	for _, gateways := range mgr.byNetwork {
		out = append(out, gateways...)
	}
	return out
}

func (mgr *NetworkManager) GatewaysForNetwork(nw network.ID) []*NetworkGateway {
	return mgr.byNetwork[nw]
}

func (mgr *NetworkManager) GatewaysForNetworkAndCluster(nw network.ID, c cluster.ID) []*NetworkGateway {
	return mgr.byNetworkAndCluster[networkAndClusterFor(nw, c)]
}

type networkAndCluster struct {
	network network.ID
	cluster cluster.ID
}

func networkAndClusterForGateway(g *NetworkGateway) networkAndCluster {
	return networkAndClusterFor(g.Network, g.Cluster)
}

func networkAndClusterFor(nw network.ID, c cluster.ID) networkAndCluster {
	return networkAndCluster{
		network: nw,
		cluster: c,
	}
}
