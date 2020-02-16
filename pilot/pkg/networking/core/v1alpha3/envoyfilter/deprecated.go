// Copyright 2019 Istio Authors
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

package envoyfilter

import (
	"net"
	"strings"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	xdslistener "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	http_conn "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	xdsutil "github.com/envoyproxy/go-control-plane/pkg/wellknown"

	"istio.io/istio/pkg/util/gogo"

	networking "istio.io/api/networking/v1alpha3"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/pkg/log"
)

// DeprecatedInsertUserFilters inserts envoy filters from deprecated envoy filter config.
// We process EnvoyFilter CRDs after calling all plugins and building the listener with the required filter chains
// Having the entire filter chain is essential because users can specify one or more filters to be inserted
// before/after  a filter or remove one or more filters.
// We use the plugin.InputParams as a convenience object to pass around parameters like proxy, proxyInstances, ports,
// etc., instead of having a long argument list
// If one or more filters are added to the HTTP connection manager, we will update the last filter in the listener
// filter chain (which is the http connection manager) with the updated object.
func DeprecatedInsertUserFilters(in *plugin.InputParams, listener *xdsapi.Listener,
	httpConnectionManagers []*http_conn.HttpConnectionManager) error { //nolint: unparam
	filterCRD := in.Push.EnvoyFilters(in.Node)
	if filterCRD == nil {
		return nil
	}

	listenerIPAddress := getListenerIPAddress(listener.Address)
	if listenerIPAddress == nil {
		log.Warnf("Failed to parse IP Address from plugin listener")
	}

	for _, f := range filterCRD.DeprecatedFilters {
		if !deprecatedListenerMatch(in, listenerIPAddress, f.ListenerMatch) {
			continue
		}
		// 4 cases of filter insertion
		// http listener, http filter
		// tcp listener, tcp filter
		// http listener, tcp filter
		// tcp listener, http filter -- invalid

		for cnum, lFilterChain := range listener.FilterChains {
			if util.IsHTTPFilterChain(lFilterChain) {
				// The listener match logic does not take into account the listener protocol
				// because filter chains in a listener can have multiple protocols.
				// for each filter chain, if the filter chain has a http connection manager,
				// treat it as a http listener
				// ListenerProtocol defaults to ALL. But if user specified listener protocol TCP, then
				// skip this filter chain as its a HTTP filter chain
				if f.ListenerMatch != nil &&
					!(f.ListenerMatch.ListenerProtocol == networking.EnvoyFilter_DeprecatedListenerMatch_ALL ||
						f.ListenerMatch.ListenerProtocol == networking.EnvoyFilter_DeprecatedListenerMatch_HTTP) {
					continue
				}

				// Now that the match condition is true, insert the filter if compatible
				// http listener, http filter case
				if f.FilterType == networking.EnvoyFilter_Filter_HTTP {
					// Insert into http connection manager
					deprecatedInsertHTTPFilter(listener.Name, listener.FilterChains[cnum], httpConnectionManagers[cnum], f)
				} else {
					// http listener, tcp filter
					deprecatedInsertNetworkFilter(listener.Name, listener.FilterChains[cnum], f)
				}
			} else {
				// The listener match logic does not take into account the listener protocol
				// because filter chains in a listener can have multiple protocols.
				// for each filter chain, if the filter chain does not have a http connection manager,
				// treat it as a tcp listener
				// ListenerProtocol defaults to ALL. But if user specified listener protocol HTTP, then
				// skip this filter chain as its a TCP filter chain
				if f.ListenerMatch != nil &&
					!(f.ListenerMatch.ListenerProtocol == networking.EnvoyFilter_DeprecatedListenerMatch_ALL ||
						f.ListenerMatch.ListenerProtocol == networking.EnvoyFilter_DeprecatedListenerMatch_TCP) {
					continue
				}

				// treat both as insert network filter X into network filter chain.
				// We cannot insert a HTTP in filter in network filter chain.
				// Even HTTP connection manager is a network filter
				if f.FilterType == networking.EnvoyFilter_Filter_HTTP {
					log.Warnf("Ignoring filter %s. Cannot insert HTTP filter in network filter chain",
						f.FilterName)
					continue
				}
				deprecatedInsertNetworkFilter(listener.Name, listener.FilterChains[cnum], f)
			}
		}
	}
	return nil
}

func getListenerIPAddress(address *core.Address) net.IP {
	if address != nil && address.Address != nil {
		switch t := address.Address.(type) {
		case *core.Address_SocketAddress:
			if t.SocketAddress != nil {
				ip := "0.0.0.0"
				if t.SocketAddress.Address != "::" {
					ip = t.SocketAddress.Address
				}
				return net.ParseIP(ip)
			}
		}
	}
	return nil
}

func deprecatedListenerMatch(in *plugin.InputParams, listenerIP net.IP,
	matchCondition *networking.EnvoyFilter_DeprecatedListenerMatch) bool {
	if matchCondition == nil {
		return true
	}

	// match by port first
	if matchCondition.PortNumber > 0 && in.Port.Port != int(matchCondition.PortNumber) {
		return false
	}

	// match by port name prefix
	if matchCondition.PortNamePrefix != "" {
		if !strings.HasPrefix(strings.ToLower(in.Port.Name), strings.ToLower(matchCondition.PortNamePrefix)) {
			return false
		}
	}

	// case ANY implies do not care about proxy type or direction
	if matchCondition.ListenerType != networking.EnvoyFilter_DeprecatedListenerMatch_ANY {
		// check if the current listener category matches with the user specified type
		if matchCondition.ListenerType != in.DeprecatedListenerCategory {
			return false
		}

		// Check if the node's role matches properly with the listener category
		switch matchCondition.ListenerType {
		case networking.EnvoyFilter_DeprecatedListenerMatch_GATEWAY:
			if in.Node.Type != model.Router {
				return false // We don't care about direction for gateways
			}
		case networking.EnvoyFilter_DeprecatedListenerMatch_SIDECAR_INBOUND,
			networking.EnvoyFilter_DeprecatedListenerMatch_SIDECAR_OUTBOUND:
			if in.Node.Type != model.SidecarProxy {
				return false
			}
		}
	}

	// Listener protocol will be matched as we try to insert the filters

	if len(matchCondition.Address) > 0 {
		matched := false
		// if any of the addresses here match, return true
		for _, address := range matchCondition.Address {
			if strings.Contains(address, "/") {
				var ipNet *net.IPNet
				var err error
				if _, ipNet, err = net.ParseCIDR(address); err != nil {
					log.Warnf("Failed to parse address %s in EnvoyFilter: %v", address, err)
					continue
				}
				if ipNet.Contains(listenerIP) {
					matched = true
					break
				}
			} else if net.ParseIP(address).Equal(listenerIP) {
				matched = true
				break
			}
		}
		return matched
	}

	return true
}

func deprecatedInsertHTTPFilter(listenerName string, filterChain *xdslistener.FilterChain, hcm *http_conn.HttpConnectionManager,
	envoyFilter *networking.EnvoyFilter_Filter) {
	filter := &http_conn.HttpFilter{
		Name:       envoyFilter.FilterName,
		ConfigType: &http_conn.HttpFilter_Config{Config: gogo.StructToProtoStruct(envoyFilter.FilterConfig)},
	}

	position := networking.EnvoyFilter_InsertPosition_FIRST
	if envoyFilter.InsertPosition != nil {
		position = envoyFilter.InsertPosition.Index
	}

	oldLen := len(hcm.HttpFilters)
	switch position {
	case networking.EnvoyFilter_InsertPosition_FIRST, networking.EnvoyFilter_InsertPosition_BEFORE:
		hcm.HttpFilters = append([]*http_conn.HttpFilter{filter}, hcm.HttpFilters...)
		if position == networking.EnvoyFilter_InsertPosition_BEFORE {
			// bubble the filter to the right position scanning from beginning
			for i := 1; i < len(hcm.HttpFilters); i++ {
				if hcm.HttpFilters[i].Name != envoyFilter.InsertPosition.RelativeTo {
					hcm.HttpFilters[i-1], hcm.HttpFilters[i] = hcm.HttpFilters[i], hcm.HttpFilters[i-1]
				} else {
					break
				}
			}
		}
	case networking.EnvoyFilter_InsertPosition_LAST, networking.EnvoyFilter_InsertPosition_AFTER:
		hcm.HttpFilters = append(hcm.HttpFilters, filter)
		if position == networking.EnvoyFilter_InsertPosition_AFTER {
			// bubble the filter to the right position scanning from end
			for i := len(hcm.HttpFilters) - 2; i >= 0; i-- {
				if hcm.HttpFilters[i].Name != envoyFilter.InsertPosition.RelativeTo {
					hcm.HttpFilters[i+1], hcm.HttpFilters[i] = hcm.HttpFilters[i], hcm.HttpFilters[i+1]
				} else {
					break
				}
			}
		}
	}

	// Rebuild the HTTP connection manager in the network filter chain
	// Its the last filter in the filter chain
	filterStruct := xdslistener.Filter{
		Name:       xdsutil.HTTPConnectionManager,
		ConfigType: &xdslistener.Filter_TypedConfig{TypedConfig: util.MessageToAny(hcm)},
	}
	filterChain.Filters[len(filterChain.Filters)-1] = &filterStruct
	log.Debugf("EnvoyFilters: Rebuilt HTTP Connection Manager %s (from %d filters to %d filters)",
		listenerName, oldLen, len(hcm.HttpFilters))
}

func deprecatedInsertNetworkFilter(listenerName string, filterChain *xdslistener.FilterChain,
	envoyFilter *networking.EnvoyFilter_Filter) {
	filter := &xdslistener.Filter{
		Name:       envoyFilter.FilterName,
		ConfigType: &xdslistener.Filter_Config{Config: gogo.StructToProtoStruct(envoyFilter.FilterConfig)},
	}

	position := networking.EnvoyFilter_InsertPosition_FIRST
	if envoyFilter.InsertPosition != nil {
		position = envoyFilter.InsertPosition.Index
	}

	oldLen := len(filterChain.Filters)
	switch position {
	case networking.EnvoyFilter_InsertPosition_FIRST, networking.EnvoyFilter_InsertPosition_BEFORE:
		filterChain.Filters = append([]*xdslistener.Filter{filter}, filterChain.Filters...)
		if position == networking.EnvoyFilter_InsertPosition_BEFORE {
			// bubble the filter to the right position scanning from beginning
			for i := 1; i < len(filterChain.Filters); i++ {
				if filterChain.Filters[i].Name != envoyFilter.InsertPosition.RelativeTo {
					filterChain.Filters[i-1], filterChain.Filters[i] = filterChain.Filters[i], filterChain.Filters[i-1]
					break
				}
			}
		}
	case networking.EnvoyFilter_InsertPosition_LAST, networking.EnvoyFilter_InsertPosition_AFTER:
		filterChain.Filters = append(filterChain.Filters, filter)
		if position == networking.EnvoyFilter_InsertPosition_AFTER {
			// bubble the filter to the right position scanning from end
			for i := len(filterChain.Filters) - 2; i >= 0; i-- {
				if filterChain.Filters[i].Name != envoyFilter.InsertPosition.RelativeTo {
					filterChain.Filters[i+1], filterChain.Filters[i] = filterChain.Filters[i], filterChain.Filters[i+1]
					break
				}
			}
		}
	}
	log.Debugf("EnvoyFilters: Rebuilt network filter stack for listener %s (from %d filters to %d filters)",
		listenerName, oldLen, len(filterChain.Filters))
}
