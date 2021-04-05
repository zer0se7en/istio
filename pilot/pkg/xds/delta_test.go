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
	"reflect"
	"testing"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"

	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/tests/util/leak"
)

func TestDeltaAds(t *testing.T) {
	leak.Check(t)
	s := NewFakeDiscoveryServer(t, FakeOptions{})
	ads := s.ConnectDeltaADS().WithType(v3.ClusterType)
	ads.RequestResponseAck(nil)
}

func TestDeltaAdsClusterUpdate(t *testing.T) {
	s := NewFakeDiscoveryServer(t, FakeOptions{})
	ads := s.ConnectDeltaADS().WithType(v3.EndpointType)

	nonce := ""
	sendEDSReqAndVerify := func(add, remove, expect []string) {
		t.Helper()
		res := ads.RequestResponseAck(&discovery.DeltaDiscoveryRequest{
			ResourceNamesSubscribe:   add,
			ResourceNamesUnsubscribe: remove,
			ResponseNonce:            nonce,
		})
		nonce = res.Nonce
		got := xdstest.MapKeys(xdstest.ExtractLoadAssignments(xdstest.UnmarshalClusterLoadAssignment(t, ConvertDeltaToResponse(res.Resources))))
		if !reflect.DeepEqual(expect, got) {
			t.Fatalf("expected clusters %v got %v", expect, got)
		}
	}

	sendEDSReqAndVerify([]string{"outbound|80||local.default.svc.cluster.local"}, nil, []string{"outbound|80||local.default.svc.cluster.local"})
	// Only send the one that is requested
	sendEDSReqAndVerify([]string{"outbound|81||local.default.svc.cluster.local"}, nil, []string{"outbound|81||local.default.svc.cluster.local"})
	// TODO: should we just respond with nothing here? Probably...
	sendEDSReqAndVerify(nil, []string{"outbound|81||local.default.svc.cluster.local"}, []string{"outbound|80||local.default.svc.cluster.local"})
}
