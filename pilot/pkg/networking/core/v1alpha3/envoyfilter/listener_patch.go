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

package envoyfilter

import (
	"fmt"

	xdslistener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/any"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/runtime"
	"istio.io/istio/pkg/config/xds"
	"istio.io/pkg/log"
)

// ApplyListenerPatches applies patches to LDS output
func ApplyListenerPatches(
	patchContext networking.EnvoyFilter_PatchContext,
	proxy *model.Proxy,
	push *model.PushContext,
	efw *model.EnvoyFilterWrapper,
	listeners []*xdslistener.Listener,
	skipAdds bool) (out []*xdslistener.Listener) {
	defer runtime.HandleCrash(runtime.LogPanic, func(interface{}) {
		IncrementEnvoyFilterErrorMetric(efw.Key(), Listener)
		log.Errorf("listeners patch caused panic, so the patches did not take effect")
	})
	// In case the patches cause panic, use the listeners generated before to reduce the influence.
	out = listeners

	if efw == nil {
		return
	}

	return patchListeners(patchContext, efw, listeners, skipAdds)
}

func patchListeners(
	patchContext networking.EnvoyFilter_PatchContext,
	efw *model.EnvoyFilterWrapper,
	listeners []*xdslistener.Listener,
	skipAdds bool) []*xdslistener.Listener {
	listenersRemoved := false
	filterKey := efw.Key()

	// do all the changes for a single envoy filter crd object. [including adds]
	// then move on to the next one

	// only removes/merges plus next level object operations [add/remove/merge]
	for _, listener := range listeners {
		if listener.Name == "" {
			// removed by another op
			continue
		}
		patchListener(patchContext, filterKey, efw.Patches, listener, &listenersRemoved)
	}
	// adds at listener level if enabled
	applied := false
	if !skipAdds {
		for _, lp := range efw.Patches[networking.EnvoyFilter_LISTENER] {
			if lp.Operation == networking.EnvoyFilter_Patch_ADD {
				if !commonConditionMatch(patchContext, lp) {
					continue
				}

				// clone before append. Otherwise, subsequent operations on this listener will corrupt
				// the master value stored in CP..
				listeners = append(listeners, proto.Clone(lp.Value).(*xdslistener.Listener))
				applied = true
			}
		}
	}
	IncrementEnvoyFilterMetric(filterKey, Listener, applied)
	if listenersRemoved {
		tempArray := make([]*xdslistener.Listener, 0, len(listeners))
		for _, l := range listeners {
			if l.Name != "" {
				tempArray = append(tempArray, l)
			}
		}
		return tempArray
	}
	return listeners
}

func patchListener(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener, listenersRemoved *bool) {
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_LISTENER] {
		if !commonConditionMatch(patchContext, lp) ||
			!listenerMatch(listener, lp) {
			continue
		}
		applied = true
		if lp.Operation == networking.EnvoyFilter_Patch_REMOVE {
			listener.Name = ""
			*listenersRemoved = true
			// terminate the function here as we have nothing more do to for this listener
			return
		} else if lp.Operation == networking.EnvoyFilter_Patch_MERGE {
			proto.Merge(listener, lp.Value)
		}
	}
	IncrementEnvoyFilterMetric(filterKey, Listener, applied)
	patchFilterChains(patchContext, filterKey, patches, listener)
}

func patchFilterChains(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener) {
	filterChainsRemoved := false
	for i, fc := range listener.FilterChains {
		if fc.Filters == nil {
			continue
		}
		patchFilterChain(patchContext, filterKey, patches, listener, listener.FilterChains[i], &filterChainsRemoved)
	}
	if fc := listener.GetDefaultFilterChain(); fc.GetFilters() != nil {
		removed := false
		patchFilterChain(patchContext, filterKey, patches, listener, fc, &removed)
		if removed {
			listener.DefaultFilterChain = nil
		}
	}
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_FILTER_CHAIN] {
		if lp.Operation == networking.EnvoyFilter_Patch_ADD {
			if !commonConditionMatch(patchContext, lp) ||
				!listenerMatch(listener, lp) {
				continue
			}
			applied = true
			listener.FilterChains = append(listener.FilterChains, proto.Clone(lp.Value).(*xdslistener.FilterChain))
		}
	}
	IncrementEnvoyFilterMetric(filterKey, FilterChain, applied)
	if filterChainsRemoved {
		tempArray := make([]*xdslistener.FilterChain, 0, len(listener.FilterChains))
		for _, fc := range listener.FilterChains {
			if fc.Filters != nil {
				tempArray = append(tempArray, fc)
			}
		}
		listener.FilterChains = tempArray
	}
}

func patchFilterChain(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener,
	fc *xdslistener.FilterChain, filterChainRemoved *bool) {
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_FILTER_CHAIN] {
		if !commonConditionMatch(patchContext, lp) ||
			!listenerMatch(listener, lp) ||
			!filterChainMatch(listener, fc, lp) {
			continue
		}
		applied = true
		if lp.Operation == networking.EnvoyFilter_Patch_REMOVE {
			fc.Filters = nil
			*filterChainRemoved = true
			// nothing more to do in other patches as we removed this filter chain
			return
		} else if lp.Operation == networking.EnvoyFilter_Patch_MERGE {
			ret, err := mergeTransportSocketListener(fc, lp)
			if err != nil {
				log.Debugf("merge of transport socket failed for listener: %v", err)
				continue
			}
			if !ret {
				proto.Merge(fc, lp.Value)
			}
		}
	}
	IncrementEnvoyFilterMetric(filterKey, FilterChain, applied)
	patchNetworkFilters(patchContext, filterKey, patches, listener, fc)
}

// Test if the patch contains a config for TransportSocket
func mergeTransportSocketListener(fc *xdslistener.FilterChain, lp *model.EnvoyFilterConfigPatchWrapper) (bool, error) {
	lpValueCast, ok := (lp.Value).(*xdslistener.FilterChain)
	if !ok {
		return false, fmt.Errorf("cast of cp.Value failed: %v", ok)
	}

	// Test if the patch contains a config for TransportSocket
	applyPatch := false
	if lpValueCast.GetTransportSocket() != nil {
		// Test if the listener contains a config for TransportSocket
		applyPatch = fc.GetTransportSocket() != nil && lpValueCast.GetTransportSocket().Name == fc.GetTransportSocket().Name
	} else {
		return false, nil
	}

	if applyPatch {
		// Merge the patch and the listener at a lower level
		dstListener := fc.GetTransportSocket().GetTypedConfig()
		srcPatch := lpValueCast.GetTransportSocket().GetTypedConfig()

		if dstListener != nil && srcPatch != nil {

			retVal, errMerge := util.MergeAnyWithAny(dstListener, srcPatch)
			if errMerge != nil {
				return false, fmt.Errorf("function mergeAnyWithAny failed for doFilterChainOperation: %v", errMerge)
			}

			// Merge the above result with the whole listener
			proto.Merge(dstListener, retVal)
		}
	}
	// Default: we won't call proto.Merge() if the patch is transportSocket and the listener isn't
	return true, nil
}

func patchNetworkFilters(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener, fc *xdslistener.FilterChain) {
	networkFiltersRemoved := false
	for i, filter := range fc.Filters {
		if filter.Name == "" {
			continue
		}
		patchNetworkFilter(patchContext, filterKey, patches, listener, fc, fc.Filters[i], &networkFiltersRemoved)
	}
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_NETWORK_FILTER] {
		if !commonConditionMatch(patchContext, lp) ||
			!listenerMatch(listener, lp) ||
			!filterChainMatch(listener, fc, lp) {
			continue
		}
		if lp.Operation == networking.EnvoyFilter_Patch_ADD {
			fc.Filters = append(fc.Filters, proto.Clone(lp.Value).(*xdslistener.Filter))
			applied = true
		} else if lp.Operation == networking.EnvoyFilter_Patch_INSERT_FIRST {
			fc.Filters = append([]*xdslistener.Filter{proto.Clone(lp.Value).(*xdslistener.Filter)}, fc.Filters...)
			applied = true
		} else if lp.Operation == networking.EnvoyFilter_Patch_INSERT_AFTER {
			// Insert after without a filter match is same as ADD in the end
			if !hasNetworkFilterMatch(lp) {
				fc.Filters = append(fc.Filters, proto.Clone(lp.Value).(*xdslistener.Filter))
				continue
			}
			// find the matching filter first
			insertPosition := -1
			for i := 0; i < len(fc.Filters); i++ {
				if networkFilterMatch(fc.Filters[i], lp) {
					insertPosition = i + 1
					break
				}
			}

			if insertPosition == -1 {
				continue
			}
			applied = true
			clonedVal := proto.Clone(lp.Value).(*xdslistener.Filter)
			fc.Filters = append(fc.Filters, clonedVal)
			if insertPosition < len(fc.Filters)-1 {
				copy(fc.Filters[insertPosition+1:], fc.Filters[insertPosition:])
				fc.Filters[insertPosition] = clonedVal
			}
		} else if lp.Operation == networking.EnvoyFilter_Patch_INSERT_BEFORE {
			// insert before without a filter match is same as insert in the beginning
			if !hasNetworkFilterMatch(lp) {
				fc.Filters = append([]*xdslistener.Filter{proto.Clone(lp.Value).(*xdslistener.Filter)}, fc.Filters...)
				continue
			}
			// find the matching filter first
			insertPosition := -1
			for i := 0; i < len(fc.Filters); i++ {
				if networkFilterMatch(fc.Filters[i], lp) {
					insertPosition = i
					break
				}
			}

			// If matching filter is not found, then don't insert and continue.
			if insertPosition == -1 {
				continue
			}
			applied = true
			clonedVal := proto.Clone(lp.Value).(*xdslistener.Filter)
			fc.Filters = append(fc.Filters, clonedVal)
			copy(fc.Filters[insertPosition+1:], fc.Filters[insertPosition:])
			fc.Filters[insertPosition] = clonedVal
		} else if lp.Operation == networking.EnvoyFilter_Patch_REPLACE {
			if !hasNetworkFilterMatch(lp) {
				continue
			}
			// find the matching filter first
			replacePosition := -1
			for i := 0; i < len(fc.Filters); i++ {
				if networkFilterMatch(fc.Filters[i], lp) {
					replacePosition = i
					break
				}
			}
			if replacePosition == -1 {
				continue
			}
			applied = true
			fc.Filters[replacePosition] = proto.Clone(lp.Value).(*xdslistener.Filter)
		}
	}
	IncrementEnvoyFilterMetric(filterKey, NetworkFilter, applied)
	if networkFiltersRemoved {
		tempArray := make([]*xdslistener.Filter, 0, len(fc.Filters))
		for _, filter := range fc.Filters {
			if filter.Name != "" {
				tempArray = append(tempArray, filter)
			}
		}
		fc.Filters = tempArray
	}
}

func patchNetworkFilter(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener, fc *xdslistener.FilterChain,
	filter *xdslistener.Filter, networkFilterRemoved *bool) {
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_NETWORK_FILTER] {
		if !commonConditionMatch(patchContext, lp) ||
			!listenerMatch(listener, lp) ||
			!filterChainMatch(listener, fc, lp) ||
			!networkFilterMatch(filter, lp) {
			continue
		}
		if lp.Operation == networking.EnvoyFilter_Patch_REMOVE {
			filter.Name = ""
			*networkFilterRemoved = true
			// nothing more to do in other patches as we removed this filter
			return
		} else if lp.Operation == networking.EnvoyFilter_Patch_MERGE {
			// proto merge doesn't work well when merging two filters with ANY typed configs
			// especially when the incoming cp.Value is a struct that could contain the json config
			// of an ANY typed filter. So convert our filter's typed config to Struct (retaining the any
			// typed output of json)
			if filter.GetTypedConfig() == nil {
				// TODO(rshriram): fixme
				// skip this op as we would possibly have to do a merge of Any with struct
				// which doesn't seem to work well.
				continue
			}
			userFilter := lp.Value.(*xdslistener.Filter)
			var err error
			// we need to be able to overwrite filter names or simply empty out a filter's configs
			// as they could be supplied through per route filter configs
			filterName := filter.Name
			if userFilter.Name != "" {
				filterName = userFilter.Name
			}
			var retVal *any.Any
			if userFilter.GetTypedConfig() != nil {
				applied = true
				// user has any typed struct
				// The type may not match up exactly. For example, if we use v2 internally but they use v3.
				// Assuming they are not using deprecated/new fields, we can safely swap out the TypeUrl
				// If we did not do this, proto.Merge below will panic (which is recovered), so even though this
				// is not 100% reliable its better than doing nothing
				if userFilter.GetTypedConfig().TypeUrl != filter.GetTypedConfig().TypeUrl {
					userFilter.ConfigType.(*xdslistener.Filter_TypedConfig).TypedConfig.TypeUrl = filter.GetTypedConfig().TypeUrl
				}
				if retVal, err = util.MergeAnyWithAny(filter.GetTypedConfig(), userFilter.GetTypedConfig()); err != nil {
					retVal = filter.GetTypedConfig()
				}
			}
			filter.Name = toCanonicalName(filterName)
			if retVal != nil {
				filter.ConfigType = &xdslistener.Filter_TypedConfig{TypedConfig: retVal}
			}
		}
	}
	IncrementEnvoyFilterMetric(filterKey, NetworkFilter, applied)
	if filter.Name == wellknown.HTTPConnectionManager {
		patchHTTPFilters(patchContext, filterKey, patches, listener, fc, filter)
	}
}

func patchHTTPFilters(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener, fc *xdslistener.FilterChain, filter *xdslistener.Filter) {
	httpconn := &hcm.HttpConnectionManager{}
	if filter.GetTypedConfig() != nil {
		if err := filter.GetTypedConfig().UnmarshalTo(httpconn); err != nil {
			return
			// todo: figure out a non noisy logging option here
			//  as this loop will be called very frequently
		}
	}
	httpFiltersRemoved := false
	for _, httpFilter := range httpconn.HttpFilters {
		if httpFilter.Name == "" {
			continue
		}
		patchHTTPFilter(patchContext, filterKey, patches, listener, fc, filter, httpFilter, &httpFiltersRemoved)
	}
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_HTTP_FILTER] {
		if !commonConditionMatch(patchContext, lp) ||
			!listenerMatch(listener, lp) ||
			!filterChainMatch(listener, fc, lp) ||
			!networkFilterMatch(filter, lp) {
			continue
		}
		if lp.Operation == networking.EnvoyFilter_Patch_ADD {
			applied = true
			httpconn.HttpFilters = append(httpconn.HttpFilters, proto.Clone(lp.Value).(*hcm.HttpFilter))
		} else if lp.Operation == networking.EnvoyFilter_Patch_INSERT_FIRST {
			httpconn.HttpFilters = append([]*hcm.HttpFilter{proto.Clone(lp.Value).(*hcm.HttpFilter)}, httpconn.HttpFilters...)
		} else if lp.Operation == networking.EnvoyFilter_Patch_INSERT_AFTER {
			// Insert after without a filter match is same as ADD in the end
			if !hasHTTPFilterMatch(lp) {
				httpconn.HttpFilters = append(httpconn.HttpFilters, proto.Clone(lp.Value).(*hcm.HttpFilter))
				continue
			}

			// find the matching filter first
			insertPosition := -1
			for i := 0; i < len(httpconn.HttpFilters); i++ {
				if httpFilterMatch(httpconn.HttpFilters[i], lp) {
					insertPosition = i + 1
					break
				}
			}

			if insertPosition == -1 {
				continue
			}
			applied = true
			clonedVal := proto.Clone(lp.Value).(*hcm.HttpFilter)
			httpconn.HttpFilters = append(httpconn.HttpFilters, clonedVal)
			if insertPosition < len(httpconn.HttpFilters)-1 {
				copy(httpconn.HttpFilters[insertPosition+1:], httpconn.HttpFilters[insertPosition:])
				httpconn.HttpFilters[insertPosition] = clonedVal
			}
		} else if lp.Operation == networking.EnvoyFilter_Patch_INSERT_BEFORE {
			// insert before without a filter match is same as insert in the beginning
			if !hasHTTPFilterMatch(lp) {
				httpconn.HttpFilters = append([]*hcm.HttpFilter{proto.Clone(lp.Value).(*hcm.HttpFilter)}, httpconn.HttpFilters...)
				continue
			}

			// find the matching filter first
			insertPosition := -1
			for i := 0; i < len(httpconn.HttpFilters); i++ {
				if httpFilterMatch(httpconn.HttpFilters[i], lp) {
					insertPosition = i
					break
				}
			}

			if insertPosition == -1 {
				continue
			}
			applied = true
			clonedVal := proto.Clone(lp.Value).(*hcm.HttpFilter)
			httpconn.HttpFilters = append(httpconn.HttpFilters, clonedVal)
			copy(httpconn.HttpFilters[insertPosition+1:], httpconn.HttpFilters[insertPosition:])
			httpconn.HttpFilters[insertPosition] = clonedVal
		} else if lp.Operation == networking.EnvoyFilter_Patch_REPLACE {
			if !hasHTTPFilterMatch(lp) {
				continue
			}

			// find the matching filter first
			replacePosition := -1
			for i := 0; i < len(httpconn.HttpFilters); i++ {
				if httpFilterMatch(httpconn.HttpFilters[i], lp) {
					replacePosition = i
					break
				}
			}
			if replacePosition == -1 {
				log.Debugf("EnvoyFilter patch %v is not applied because no matching HTTP filter found.", lp)
				continue
			}
			applied = true
			clonedVal := proto.Clone(lp.Value).(*hcm.HttpFilter)
			httpconn.HttpFilters[replacePosition] = clonedVal
		}
	}
	if httpFiltersRemoved {
		tempArray := make([]*hcm.HttpFilter, 0, len(httpconn.HttpFilters))
		for _, filter := range httpconn.HttpFilters {
			if filter.Name != "" {
				tempArray = append(tempArray, filter)
			}
		}
		httpconn.HttpFilters = tempArray
	}
	IncrementEnvoyFilterMetric(filterKey, HttpFilter, applied)
	if filter.GetTypedConfig() != nil {
		// convert to any type
		filter.ConfigType = &xdslistener.Filter_TypedConfig{TypedConfig: util.MessageToAny(httpconn)}
	}
}

func patchHTTPFilter(patchContext networking.EnvoyFilter_PatchContext,
	filterKey string,
	patches map[networking.EnvoyFilter_ApplyTo][]*model.EnvoyFilterConfigPatchWrapper,
	listener *xdslistener.Listener, fc *xdslistener.FilterChain, filter *xdslistener.Filter,
	httpFilter *hcm.HttpFilter, httpFilterRemoved *bool) {
	applied := false
	for _, lp := range patches[networking.EnvoyFilter_HTTP_FILTER] {
		if !commonConditionMatch(patchContext, lp) ||
			!listenerMatch(listener, lp) ||
			!filterChainMatch(listener, fc, lp) ||
			!networkFilterMatch(filter, lp) ||
			!httpFilterMatch(httpFilter, lp) {
			continue
		}
		if lp.Operation == networking.EnvoyFilter_Patch_REMOVE {
			httpFilter.Name = ""
			*httpFilterRemoved = true
			// nothing more to do in other patches as we removed this filter
			return
		} else if lp.Operation == networking.EnvoyFilter_Patch_MERGE {
			// proto merge doesn't work well when merging two filters with ANY typed configs
			// especially when the incoming cp.Value is a struct that could contain the json config
			// of an ANY typed filter. So convert our filter's typed config to Struct (retaining the any
			// typed output of json)
			if httpFilter.GetTypedConfig() == nil {
				// TODO(rshriram): fixme
				// skip this op as we would possibly have to do a merge of Any with struct
				// which doesn't seem to work well.
				continue
			}
			userHTTPFilter := lp.Value.(*hcm.HttpFilter)
			var err error
			// we need to be able to overwrite filter names or simply empty out a filter's configs
			// as they could be supplied through per route filter configs
			httpFilterName := httpFilter.Name
			if userHTTPFilter.Name != "" {
				httpFilterName = userHTTPFilter.Name
			}
			var retVal *any.Any
			if userHTTPFilter.GetTypedConfig() != nil {
				// user has any typed struct
				// The type may not match up exactly. For example, if we use v2 internally but they use v3.
				// Assuming they are not using deprecated/new fields, we can safely swap out the TypeUrl
				// If we did not do this, proto.Merge below will panic (which is recovered), so even though this
				// is not 100% reliable its better than doing nothing
				if userHTTPFilter.GetTypedConfig().TypeUrl != httpFilter.GetTypedConfig().TypeUrl {
					userHTTPFilter.ConfigType.(*hcm.HttpFilter_TypedConfig).TypedConfig.TypeUrl = httpFilter.GetTypedConfig().TypeUrl
				}
				if retVal, err = util.MergeAnyWithAny(httpFilter.GetTypedConfig(), userHTTPFilter.GetTypedConfig()); err != nil {
					retVal = httpFilter.GetTypedConfig()
				}
			}
			applied = true
			httpFilter.Name = toCanonicalName(httpFilterName)
			if retVal != nil {
				httpFilter.ConfigType = &hcm.HttpFilter_TypedConfig{TypedConfig: retVal}
			}
		}
	}
	IncrementEnvoyFilterMetric(filterKey, HttpFilter, applied)
}

func listenerMatch(listener *xdslistener.Listener, lp *model.EnvoyFilterConfigPatchWrapper) bool {
	lMatch := lp.Match.GetListener()
	if lMatch == nil {
		return true
	}

	if lMatch.Name != "" && lMatch.Name != listener.Name {
		return false
	}

	// skip listener port check for special virtual inbound and outbound listeners
	// to support portNumber listener filter field within those special listeners as well
	if lp.ApplyTo != networking.EnvoyFilter_LISTENER &&
		(listener.Name == model.VirtualInboundListenerName || listener.Name == model.VirtualOutboundListenerName) {
		return true
	}

	// FIXME: Ports on a listener can be 0. the API only takes uint32 for ports
	// We should either make that field in API as a wrapper type or switch to int
	if lMatch.PortNumber != 0 {
		sockAddr := listener.Address.GetSocketAddress()
		if sockAddr == nil || sockAddr.GetPortValue() != lMatch.PortNumber {
			return false
		}
	}

	return true
}

// We assume that the parent listener has already been matched
func filterChainMatch(listener *xdslistener.Listener, fc *xdslistener.FilterChain, lp *model.EnvoyFilterConfigPatchWrapper) bool {
	lMatch := lp.Match.GetListener()
	if lMatch == nil {
		return true
	}

	match := lMatch.FilterChain
	if match == nil {
		return true
	}
	if match.Name != "" {
		if match.Name != fc.Name {
			return false
		}
	}
	if match.Sni != "" {
		if fc.FilterChainMatch == nil || len(fc.FilterChainMatch.ServerNames) == 0 {
			return false
		}
		sniMatched := false
		for _, sni := range fc.FilterChainMatch.ServerNames {
			if sni == match.Sni {
				sniMatched = true
				break
			}
		}
		if !sniMatched {
			return false
		}
	}

	if match.TransportProtocol != "" {
		if fc.FilterChainMatch == nil || fc.FilterChainMatch.TransportProtocol != match.TransportProtocol {
			return false
		}
	}

	// check match for destination port within the FilterChainMatch
	if match.DestinationPort > 0 {
		if fc.FilterChainMatch == nil || fc.FilterChainMatch.DestinationPort == nil {
			return false
		} else if fc.FilterChainMatch.DestinationPort.Value != match.DestinationPort {
			return false
		}
	}
	isVirtual := listener.Name == model.VirtualInboundListenerName || listener.Name == model.VirtualOutboundListenerName
	// We only do this for virtual listeners, which will move the listener port into a FCM. For non-virtual listeners,
	// we will handle this in the proper listener match.
	if isVirtual && lMatch.GetPortNumber() > 0 && fc.GetFilterChainMatch().GetDestinationPort().GetValue() != lMatch.GetPortNumber() {
		return false
	}

	return true
}

func hasNetworkFilterMatch(lp *model.EnvoyFilterConfigPatchWrapper) bool {
	lMatch := lp.Match.GetListener()
	if lMatch == nil {
		return false
	}

	fcMatch := lMatch.FilterChain
	if fcMatch == nil {
		return false
	}

	return fcMatch.Filter != nil
}

// We assume that the parent listener and filter chain have already been matched
func networkFilterMatch(filter *xdslistener.Filter, cp *model.EnvoyFilterConfigPatchWrapper) bool {
	if !hasNetworkFilterMatch(cp) {
		return true
	}

	return nameMatches(cp.Match.GetListener().FilterChain.Filter.Name, filter.Name)
}

func hasHTTPFilterMatch(lp *model.EnvoyFilterConfigPatchWrapper) bool {
	if !hasNetworkFilterMatch(lp) {
		return false
	}

	match := lp.Match.GetListener().FilterChain.Filter.SubFilter
	return match != nil
}

// We assume that the parent listener and filter chain, and network filter have already been matched
func httpFilterMatch(filter *hcm.HttpFilter, lp *model.EnvoyFilterConfigPatchWrapper) bool {
	if !hasHTTPFilterMatch(lp) {
		return true
	}

	match := lp.Match.GetListener().FilterChain.Filter.SubFilter

	return nameMatches(match.Name, filter.Name)
}

func patchContextMatch(patchContext networking.EnvoyFilter_PatchContext,
	lp *model.EnvoyFilterConfigPatchWrapper) bool {
	return lp.Match.Context == patchContext || lp.Match.Context == networking.EnvoyFilter_ANY
}

func commonConditionMatch(patchContext networking.EnvoyFilter_PatchContext,
	lp *model.EnvoyFilterConfigPatchWrapper) bool {
	return patchContextMatch(patchContext, lp)
}

// toCanonicalName converts a deprecated filter name to the replacement, if present. Otherwise, the
// same name is returned.
func toCanonicalName(name string) string {
	if nn, f := xds.ReverseDeprecatedFilterNames[name]; f {
		return nn
	}
	return name
}

// nameMatches compares two filter names, matching even if a deprecated filter name is used.
func nameMatches(matchName, filterName string) bool {
	return matchName == filterName || matchName == xds.DeprecatedFilterNames[filterName]
}
