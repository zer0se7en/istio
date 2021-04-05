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

package echotest

import (
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
)

type (
	srcSetupFn     func(ctx framework.TestContext, src echo.Instances) error
	svcPairSetupFn func(ctx framework.TestContext, src echo.Instances, dsts echo.Services) error
	pairSetupFn    func(ctx framework.TestContext, src, dsts echo.Instances) error
)

// Setup runs the given function in the source deployment context.
//
// For example, given apps a, b, and c in 2 clusters,
// these tests would all run before the the context is cleaned up:
//     a/to_b/from_cluster-1
//     a/to_b/from_cluster-2
//     a/to_c/from_cluster-1
//     a/to_c/from_cluster-2
//     cleanup...
//     b/to_a/from_cluster-1
//     ...
func (t *T) Setup(setupFn srcSetupFn) *T {
	t.sourceDeploymentSetup = append(t.sourceDeploymentSetup, setupFn)
	return t
}

func (t *T) setup(ctx framework.TestContext, srcInstances echo.Instances) {
	for _, setupFn := range t.sourceDeploymentSetup {
		if err := setupFn(ctx, srcInstances); err != nil {
			ctx.Fatal(err)
		}
	}
}

// SetupForPair runs the given function in the source + destination deployment context. The setup function
// takes an echo.Services. When using Run there will always be 1 destination deployment. When using RunForN,
// the length will always be N.
//
// Example of how long this setup lasts before the given context is cleaned up:
//     a/to_b/from_cluster-1
//     a/to_b/from_cluster-2
//     cleanup...
//     a/to_b/from_cluster-2
//     ...
func (t *T) SetupForPair(setupFn pairSetupFn) *T {
	return t.SetupForServicePair(func(ctx framework.TestContext, src echo.Instances, dsts echo.Services) error {
		return setupFn(ctx, src, dsts[0])
	})
}

func (t *T) SetupForServicePair(setupFn svcPairSetupFn) *T {
	t.deploymentPairSetup = append(t.deploymentPairSetup, setupFn)
	return t
}

func (t *T) setupPair(ctx framework.TestContext, src echo.Instances, dsts echo.Services) {
	for _, setupFn := range t.deploymentPairSetup {
		if err := setupFn(ctx, src, dsts); err != nil {
			ctx.Fatal(err)
		}
	}
}
