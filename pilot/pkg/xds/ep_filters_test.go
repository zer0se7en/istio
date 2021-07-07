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

package xds

import (
	"sort"
	"testing"

	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"

	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	security "istio.io/api/security/v1beta1"
	"istio.io/api/type/v1beta1"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/model"
	memregistry "istio.io/istio/pilot/pkg/serviceregistry/memory"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/network"
)

type LbEpInfo struct {
	address string
	// nolint: structcheck
	weight uint32
}

type LocLbEpInfo struct {
	lbEps  []LbEpInfo
	weight uint32
}

func (i LocLbEpInfo) getAddrs() []string {
	addrs := make([]string, 0)
	for _, ep := range i.lbEps {
		addrs = append(addrs, ep.address)
	}
	return addrs
}

var networkFiltered = []networkFilterCase{
	{
		name: "from_network1_cluster1a",
		conn: xdsConnection("network1", "cluster1a"),
		want: []LocLbEpInfo{
			{
				lbEps: []LbEpInfo{
					// 2 local endpoints on network1
					{address: "10.0.0.1", weight: 2},
					{address: "10.0.0.2", weight: 2},
					// 1 endpoint on network2, cluster2a
					{address: "2.2.2.2", weight: 2},
					// 2 endpoints on network2, cluster2b
					{address: "2.2.2.20", weight: 4},
					// 1 endpoint on network4 with no gateway (i.e. directly accessible)
					{address: "40.0.0.1", weight: 2},
				},
				weight: 12,
			},
		},
	},
	{
		name: "from_network1_cluster1b",
		conn: xdsConnection("network1", "cluster1b"),
		want: []LocLbEpInfo{
			{
				lbEps: []LbEpInfo{
					// 2 local endpoints on network1
					{address: "10.0.0.1", weight: 2},
					{address: "10.0.0.2", weight: 2},
					// 1 endpoint on network2, cluster2a
					{address: "2.2.2.2", weight: 2},
					// 2 endpoints on network2, cluster2b
					{address: "2.2.2.20", weight: 4},
					// 1 endpoint on network4 with no gateway (i.e. directly accessible)
					{address: "40.0.0.1", weight: 2},
				},
				weight: 12,
			},
		},
	},
	{
		name: "from_network2_cluster2a",
		conn: xdsConnection("network2", "cluster2a"),
		want: []LocLbEpInfo{
			{
				lbEps: []LbEpInfo{
					// 3 local endpoints in network2
					{address: "20.0.0.1", weight: 2},
					{address: "20.0.0.2", weight: 2},
					{address: "20.0.0.3", weight: 2},
					// 2 endpoint on network1 with weight aggregated at the gateway
					{address: "1.1.1.1", weight: 4},
					// 1 endpoint on network4 with no gateway (i.e. directly accessible)
					{address: "40.0.0.1", weight: 2},
				},
				weight: 12,
			},
		},
	},
	{
		name: "from_network2_cluster2b",
		conn: xdsConnection("network2", "cluster2b"),
		want: []LocLbEpInfo{
			{
				lbEps: []LbEpInfo{
					// 3 local endpoints in network2
					{address: "20.0.0.1", weight: 2},
					{address: "20.0.0.2", weight: 2},
					{address: "20.0.0.3", weight: 2},
					// 2 endpoint on network1 with weight aggregated at the gateway
					{address: "1.1.1.1", weight: 4},
					// 1 endpoint on network4 with no gateway (i.e. directly accessible)
					{address: "40.0.0.1", weight: 2},
				},
				weight: 12,
			},
		},
	},
	{
		name: "from_network3_cluster3",
		conn: xdsConnection("network3", "cluster3"),
		want: []LocLbEpInfo{
			{
				lbEps: []LbEpInfo{
					// 2 endpoint on network2 with weight aggregated at the gateway
					{address: "1.1.1.1", weight: 4},
					// 1 endpoint on network2, cluster2a
					{address: "2.2.2.2", weight: 2},
					// 2 endpoints on network2, cluster2b
					{address: "2.2.2.20", weight: 4},
					// 1 endpoint on network4 with no gateway (i.e. directly accessible)
					{address: "40.0.0.1", weight: 2},
				},
				weight: 12,
			},
		},
	},
	{
		name: "from_network4_cluster4",
		conn: xdsConnection("network4", "cluster4"),
		want: []LocLbEpInfo{
			{
				lbEps: []LbEpInfo{
					// 1 local endpoint on network4
					{address: "40.0.0.1", weight: 2},
					// 2 endpoint on network2 with weight aggregated at the gateway
					{address: "1.1.1.1", weight: 4},
					// 1 endpoint on network2, cluster2a
					{address: "2.2.2.2", weight: 2},
					// 2 endpoints on network2, cluster2b
					{address: "2.2.2.20", weight: 4},
				},
				weight: 12,
			},
		},
	},
}

func TestEndpointsByNetworkFilter(t *testing.T) {
	env := environment()
	env.Init()
	// The tests below are calling the endpoints filter from each one of the
	// networks and examines the returned filtered endpoints

	runNetworkFilterTest(t, env, networkFiltered)
}

func TestEndpointsByNetworkFilter_WithConfig(t *testing.T) {
	noCrossNetwork := []networkFilterCase{
		{
			name: "from_network1_cluster1a",
			conn: xdsConnection("network1", "cluster1a"),
			want: []LocLbEpInfo{
				{
					lbEps: []LbEpInfo{
						// 2 local endpoints on network1
						{address: "10.0.0.1", weight: 2},
						{address: "10.0.0.2", weight: 2},
						// 1 endpoint on network4 with no gateway (i.e. directly accessible)
						{address: "40.0.0.1", weight: 2},
					},
					weight: 6,
				},
			},
		},
		{
			name: "from_network1_cluster1b",
			conn: xdsConnection("network1", "cluster1b"),
			want: []LocLbEpInfo{
				{
					lbEps: []LbEpInfo{
						// 2 local endpoints on network1
						{address: "10.0.0.1", weight: 2},
						{address: "10.0.0.2", weight: 2},
						// 1 endpoint on network4 with no gateway (i.e. directly accessible)
						{address: "40.0.0.1", weight: 2},
					},
					weight: 6,
				},
			},
		},
		{
			name: "from_network2_cluster2a",
			conn: xdsConnection("network2", "cluster2a"),
			want: []LocLbEpInfo{
				{
					lbEps: []LbEpInfo{
						// 1 local endpoint on network2
						{address: "20.0.0.1", weight: 2},
						{address: "20.0.0.2", weight: 2},
						{address: "20.0.0.3", weight: 2},
						// 1 endpoint on network4 with no gateway (i.e. directly accessible)
						{address: "40.0.0.1", weight: 2},
					},
					weight: 8,
				},
			},
		},
		{
			name: "from_network2_cluster2b",
			conn: xdsConnection("network2", "cluster2b"),
			want: []LocLbEpInfo{
				{
					lbEps: []LbEpInfo{
						// 1 local endpoint on network2
						{address: "20.0.0.1", weight: 2},
						{address: "20.0.0.2", weight: 2},
						{address: "20.0.0.3", weight: 2},
						// 1 endpoint on network4 with no gateway (i.e. directly accessible)
						{address: "40.0.0.1", weight: 2},
					},
					weight: 8,
				},
			},
		},
		{
			name: "from_network3_cluster3",
			conn: xdsConnection("network3", "cluster3"),
			want: []LocLbEpInfo{
				{
					lbEps: []LbEpInfo{
						// 1 endpoint on network4 with no gateway (i.e. directly accessible)
						{address: "40.0.0.1", weight: 2},
					},
					weight: 2,
				},
			},
		},
		{
			name: "from_network4_cluster4",
			conn: xdsConnection("network4", "cluster4"),
			want: []LocLbEpInfo{
				{
					lbEps: []LbEpInfo{
						// 1 local endpoint on network4
						{address: "40.0.0.1", weight: 2},
					},
					weight: 2,
				},
			},
		},
	}

	cases := map[string]map[string]struct {
		Config  config.Config
		Configs []config.Config
		Tests   []networkFilterCase
	}{
		gvk.PeerAuthentication.String(): {
			"mtls-off-ineffective": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.PeerAuthentication,
						Name:             "mtls-partial",
						Namespace:        "istio-system",
					},
					Spec: &security.PeerAuthentication{
						Selector: &v1beta1.WorkloadSelector{
							// shouldn't affect our test workload
							MatchLabels: map[string]string{"app": "b"},
						},
						Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
					},
				},
				Tests: networkFiltered,
			},
			"mtls-on-strict": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.PeerAuthentication,
						Name:             "mtls-on",
						Namespace:        "istio-system",
					},
					Spec: &security.PeerAuthentication{
						Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_STRICT},
					},
				},
				Tests: networkFiltered,
			},
			"mtls-off-global": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.PeerAuthentication,
						Name:             "mtls-off",
						Namespace:        "istio-system",
					},
					Spec: &security.PeerAuthentication{
						Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
					},
				},
				Tests: noCrossNetwork,
			},
			"mtls-off-namespace": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.PeerAuthentication,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &security.PeerAuthentication{
						Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
					},
				},
				Tests: noCrossNetwork,
			},
			"mtls-off-workload": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.PeerAuthentication,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &security.PeerAuthentication{
						Selector: &v1beta1.WorkloadSelector{
							MatchLabels: map[string]string{"app": "example"},
						},
						Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
					},
				},
				Tests: noCrossNetwork,
			},
			"mtls-off-port": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.PeerAuthentication,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &security.PeerAuthentication{
						Selector: &v1beta1.WorkloadSelector{
							MatchLabels: map[string]string{"app": "example"},
						},
						PortLevelMtls: map[uint32]*security.PeerAuthentication_MutualTLS{
							8080: {Mode: security.PeerAuthentication_MutualTLS_DISABLE},
						},
					},
				},
				Tests: noCrossNetwork,
			},
		},
		gvk.DestinationRule.String(): {
			"mtls-on-override-pa": {
				Configs: []config.Config{
					{
						Meta: config.Meta{
							GroupVersionKind: gvk.PeerAuthentication,
							Name:             "mtls-off",
							Namespace:        "ns",
						},
						Spec: &security.PeerAuthentication{
							Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
						},
					},
					{
						Meta: config.Meta{
							GroupVersionKind: gvk.DestinationRule,
							Name:             "mtls-on",
							Namespace:        "ns",
						},
						Spec: &networking.DestinationRule{
							Host: "example.ns.svc.cluster.local",
							TrafficPolicy: &networking.TrafficPolicy{
								Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_ISTIO_MUTUAL},
							},
						},
					},
				},
				Tests: networkFiltered,
			},
			"mtls-off-innefective": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "other.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_DISABLE},
						},
					},
				},
				Tests: networkFiltered,
			},
			"mtls-on-destination-level": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-on",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "example.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_ISTIO_MUTUAL},
						},
					},
				},
				Tests: networkFiltered,
			},
			"mtls-on-port-level": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-on",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "example.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							PortLevelSettings: []*networking.TrafficPolicy_PortTrafficPolicy{{
								Port: &networking.PortSelector{Number: 80},
								Tls:  &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_ISTIO_MUTUAL},
							}},
						},
					},
				},
				Tests: networkFiltered,
			},
			"mtls-off-destination-level": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "example.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_DISABLE},
						},
					},
				},
				Tests: noCrossNetwork,
			},
			"mtls-off-port-level": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "example.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							PortLevelSettings: []*networking.TrafficPolicy_PortTrafficPolicy{{
								Port: &networking.PortSelector{Number: 80},
								Tls:  &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_DISABLE},
							}},
						},
					},
				},
				Tests: noCrossNetwork,
			},
			"mtls-off-subset-level": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-off",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "example.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							// should be overridden by subset
							Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_ISTIO_MUTUAL},
						},
						Subsets: []*networking.Subset{{
							Name:   "disable-tls",
							Labels: map[string]string{"app": "example"},
							TrafficPolicy: &networking.TrafficPolicy{
								Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_DISABLE},
							},
						}},
					},
				},
				Tests: noCrossNetwork,
			},
			"mtls-on-subset-level": {
				Config: config.Config{
					Meta: config.Meta{
						GroupVersionKind: gvk.DestinationRule,
						Name:             "mtls-on",
						Namespace:        "ns",
					},
					Spec: &networking.DestinationRule{
						Host: "example.ns.svc.cluster.local",
						TrafficPolicy: &networking.TrafficPolicy{
							// should be overridden by subset
							Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_DISABLE},
						},
						Subsets: []*networking.Subset{{
							Name:   "disable-tls",
							Labels: map[string]string{"app": "example"},
							TrafficPolicy: &networking.TrafficPolicy{
								Tls: &networking.ClientTLSSettings{Mode: networking.ClientTLSSettings_ISTIO_MUTUAL},
							},
						}},
					},
				},
				Tests: networkFiltered,
			},
		},
	}

	for configType, cases := range cases {
		t.Run(configType, func(t *testing.T) {
			for name, pa := range cases {
				t.Run(name, func(t *testing.T) {
					env := environment()
					cfgs := pa.Configs
					if pa.Config.Name != "" {
						cfgs = append(cfgs, pa.Config)
					}
					for _, cfg := range cfgs {
						_, err := env.IstioConfigStore.Create(cfg)
						if err != nil {
							t.Fatalf("failed creating %s: %v", cfg.Name, err)
						}
					}
					env.Init()
					runNetworkFilterTest(t, env, pa.Tests)
				})
			}
		})
	}
}

func TestEndpointsByNetworkFilter_SkipLBWithHostname(t *testing.T) {
	//  - 1 IP gateway for network1
	//  - 1 DNS gateway for network2
	//  - 1 IP gateway for network3
	//  - 0 gateways for network4
	env := environment()
	origServices, _ := env.Services()
	origGateways := env.NetworkGateways()
	serviceDiscovery := memregistry.NewServiceDiscovery(append([]*model.Service{{
		Hostname: "istio-ingressgateway.istio-system.svc.cluster.local",
		Attributes: model.ServiceAttributes{
			ClusterExternalAddresses: map[cluster.ID][]string{
				"cluster2a": {""},
				"cluster2b": {""},
			},
		},
	}}, origServices...))
	serviceDiscovery.AddGateways(origGateways...)
	// Also add a hostname-based Gateway, which will be rejected.
	serviceDiscovery.AddGateways(&model.NetworkGateway{
		Network: "network2",
		Addr:    "aeiou.scooby.do",
		Port:    80,
	})

	env.ServiceDiscovery = serviceDiscovery
	env.Init()
	// Run the tests and ensure that the new gateway is never used.
	runNetworkFilterTest(t, env, networkFiltered)
}

type networkFilterCase struct {
	name string
	conn *Connection
	want []LocLbEpInfo
}

// runNetworkFilterTest calls the endpoints filter from each one of the
// networks and examines the returned filtered endpoints
func runNetworkFilterTest(t *testing.T, env *model.Environment, tests []networkFilterCase) {
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			push := model.NewPushContext()
			_ = push.InitContext(env, nil, nil)
			b := NewEndpointBuilder("outbound|80||example.ns.svc.cluster.local", tt.conn.proxy, push)
			testEndpoints := b.buildLocalityLbEndpointsFromShards(testShards(), &model.Port{Name: "http", Port: 80, Protocol: protocol.HTTP})
			filtered := b.EndpointsByNetworkFilter(testEndpoints)
			for _, e := range testEndpoints {
				e.AssertInvarianceInTest()
			}
			if len(filtered) != len(tt.want) {
				t.Errorf("Unexpected number of filtered endpoints: got %v, want %v", len(filtered), len(tt.want))
				return
			}

			sort.Slice(filtered, func(i, j int) bool {
				addrI := filtered[i].llbEndpoints.LbEndpoints[0].GetEndpoint().Address.GetSocketAddress().Address
				addrJ := filtered[j].llbEndpoints.LbEndpoints[0].GetEndpoint().Address.GetSocketAddress().Address
				return addrI < addrJ
			})

			for i, ep := range filtered {
				if len(ep.llbEndpoints.LbEndpoints) != len(tt.want[i].lbEps) {
					t.Errorf("Unexpected number of LB endpoints within endpoint %d: %v, want %v",
						i, getLbEndpointAddrs(&ep.llbEndpoints), tt.want[i].getAddrs())
				}

				if ep.llbEndpoints.LoadBalancingWeight.GetValue() != tt.want[i].weight {
					t.Errorf("Unexpected weight for endpoint %d: got %v, want %v", i, ep.llbEndpoints.LoadBalancingWeight.GetValue(), tt.want[i].weight)
				}

				for _, lbEp := range ep.llbEndpoints.LbEndpoints {
					addr := lbEp.GetEndpoint().Address.GetSocketAddress().Address
					found := false
					for _, wantLbEp := range tt.want[i].lbEps {
						if addr == wantLbEp.address {
							found = true

							// Now compare the weight.
							if lbEp.GetLoadBalancingWeight().Value != wantLbEp.weight {
								t.Errorf("Unexpected weight for endpoint %s: got %v, want %v",
									addr, lbEp.GetLoadBalancingWeight().Value, wantLbEp.weight)
							}
							break
						}
					}
					if !found {
						t.Errorf("Unexpected address for endpoint %d: %v", i, addr)
					}
				}
			}
		})
	}
}

func xdsConnection(nw network.ID, c cluster.ID) *Connection {
	return &Connection{
		proxy: &model.Proxy{
			Metadata: &model.NodeMetadata{
				Network:   nw,
				ClusterID: c,
			},
		},
	}
}

// environment defines the networks with:
//  - 1 gateway for network1
//  - 2 gateway for network2
//  - 1 gateway for network3
//  - 0 gateways for network4
func environment() *model.Environment {
	sd := memregistry.NewServiceDiscovery([]*model.Service{
		{
			Hostname:   "example.ns.svc.cluster.local",
			Attributes: model.ServiceAttributes{Name: "example", Namespace: "ns"},
		},
	})
	env := &model.Environment{
		ServiceDiscovery: sd,
		IstioConfigStore: model.MakeIstioStore(memory.Make(collections.Pilot)),
		Watcher:          mesh.NewFixedWatcher(&meshconfig.MeshConfig{RootNamespace: "istio-system"}),
		NetworksWatcher:  mesh.NewFixedNetworksWatcher(&meshconfig.MeshNetworks{}),
	}

	// Configure the network gateways.
	sd.AddGateways(
		// network1 has only 1 gateway in cluster1a, which will be used for the endpoints
		// in both cluster1a and cluster1b.
		&model.NetworkGateway{
			Network: "network1",
			Cluster: "cluster1a",
			Addr:    "1.1.1.1",
			Port:    80,
		},

		// network2 has one gateway in each cluster2a and cluster2b. When targeting a particular
		// endpoint, only the gateway for its cluster will be selected. Since the clusters do not
		// have the same number of endpoints, the weights for the gateways will be different.
		&model.NetworkGateway{
			Network: "network2",
			Cluster: "cluster2a",
			Addr:    "2.2.2.2",
			Port:    80,
		},
		&model.NetworkGateway{
			Network: "network2",
			Cluster: "cluster2b",
			Addr:    "2.2.2.20",
			Port:    80,
		},

		// network3 has a gateway in cluster3, but no endpoints.
		&model.NetworkGateway{
			Network: "network3",
			Cluster: "cluster3",
			Addr:    "3.3.3.3",
			Port:    443,
		},

		// network4 has no gateways, so its endpoints will be considered reachable from every
		// other cluster.
	)
	return env
}

// testShards creates endpoints to be handed to the filter:
//  - 2 endpoints in network1
//  - 1 endpoints in network2
//  - 0 endpoints in network3
//  - 1 endpoints in network4
//
// All endpoints are part of service example.ns.svc.cluster.local on port 80 (http).
func testShards() *EndpointShards {
	shards := &EndpointShards{Shards: map[string][]*model.IstioEndpoint{
		// network1 has one endpoint in each cluster
		"cluster1a": {
			{Network: "network1", Address: "10.0.0.1"},
		},
		"cluster1b": {
			{Network: "network1", Address: "10.0.0.2"},
		},

		// network2 has an imbalance of endpoints between its clusters
		"cluster2a": {
			{Network: "network2", Address: "20.0.0.1"},
		},
		"cluster2b": {
			{Network: "network2", Address: "20.0.0.2"},
			{Network: "network2", Address: "20.0.0.3"},
		},

		// network3 has no endpoints.

		// network4 has a single endpoint, but not gateway so it will always
		// be considered directly reachable.
		"cluster4": {
			{Network: "network4", Address: "40.0.0.1"},
		},
	}}
	// apply common properties
	for clusterID, shard := range shards.Shards {
		for i, ep := range shard {
			ep.ServicePortName = "http"
			ep.Namespace = "ns"
			ep.HostName = "example.ns.svc.cluster.local"
			ep.EndpointPort = 8080
			ep.TLSMode = "istio"
			ep.Labels = map[string]string{"app": "example"}
			ep.Locality.ClusterID = cluster.ID(clusterID)
			shards.Shards[clusterID][i] = ep
		}
	}
	return shards
}

func getLbEndpointAddrs(ep *endpoint.LocalityLbEndpoints) []string {
	addrs := make([]string, 0)
	for _, lbEp := range ep.LbEndpoints {
		addrs = append(addrs, lbEp.GetEndpoint().Address.GetSocketAddress().Address)
	}
	return addrs
}
