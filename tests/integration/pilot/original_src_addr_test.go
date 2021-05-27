// +build integ
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

package pilot

import (
	"fmt"
	"testing"

	"istio.io/istio/pkg/test/echo/client"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
)

func TestTproxy(t *testing.T) {
	framework.
		NewTest(t).
		Features("traffic.original-source-ip").
		RequiresSingleCluster().
		Run(func(t framework.TestContext) {
			workloads, err := apps.PodA[0].Workloads()
			if err != nil {
				t.Errorf("failed to get Subsets: %v", err)
				return
			}
			// check the server can see the client's original ip
			var srcIps []string
			for _, w := range workloads {
				srcIps = append(srcIps, w.Address())
			}
			checkOriginalSrcIP(t, apps.PodA[0], apps.PodTproxy[0], srcIps)
		})
}

func checkOriginalSrcIP(t framework.TestContext, src echo.Caller, dest echo.Instance, expected []string) {
	t.Helper()
	validator := echo.ValidatorFunc(func(resp client.ParsedResponses, inErr error) error {
		// Check that each response saw one of the workload IPs for the src echo instance
		for _, r := range resp {
			found := false
			for _, ip := range expected {
				if r.IP == ip {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("unexpected IP %s, expected to be contained in %v",
					r.IP, expected)
			}
		}

		return nil
	})
	_ = src.CallWithRetryOrFail(t, echo.CallOptions{
		Target:    dest,
		PortName:  "http",
		Scheme:    scheme.HTTP,
		Count:     1,
		Validator: validator,
	})
}
