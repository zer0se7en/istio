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
	"strings"

	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/istio/ingress"
)

type (
	perDeploymentTest  func(t framework.TestContext, instances echo.Instances)
	perNDeploymentTest func(t framework.TestContext, deployments echo.Services)
	perInstanceTest    func(t framework.TestContext, inst echo.Instance)
	perClusterTest     func(t framework.TestContext, c cluster.Cluster)

	oneToOneTest      func(t framework.TestContext, src echo.Instance, dst echo.Instances)
	oneToNTest        func(t framework.TestContext, src echo.Instance, dsts echo.Services)
	oneClusterOneTest func(t framework.TestContext, src cluster.Cluster, dst echo.Instances)
	ingressTest       func(t framework.TestContext, src ingress.Instance, dst echo.Instances)
)

// Run will generate and run one subtest to send traffic between each combination
// of source instance to target deployment.
//
// For example, in a test named `a/to_b/from_cluster-0`,
// `a` is the source deployment, `b` is the destination deployment and
// `cluster-0` marks which instance of the source deployment.
//
// We use a combination of deployment name and cluster name to identify
// a particular source instance, as there should typically be one instance
// per cluster for any deployment. However we do not identify a destination
// cluster, as we expect most tests will cause load-balancing across all possible
// clusters.
func (t *T) Run(testFn oneToOneTest) {
	t.rootCtx.Logf("Running tests with: sources %v -> destinations %v", t.sources.Services().Services(), t.destinations.Services().Services())
	t.fromEachDeployment(t.rootCtx, func(ctx framework.TestContext, srcInstances echo.Instances) {
		t.setup(ctx, srcInstances.Callers())
		t.toEachDeployment(ctx, func(ctx framework.TestContext, dstInstances echo.Instances) {
			t.setupPair(ctx, srcInstances.Callers(), echo.Services{dstInstances})
			t.fromEachWorkloadCluster(ctx, srcInstances, func(ctx framework.TestContext, src echo.Instance) {
				filteredDst := t.applyCombinationFilters(src, dstInstances)
				if len(filteredDst) == 0 {
					// this only happens due to conditional filters and when an entire deployment is filtered we should be noisy
					ctx.Skipf("cases from %s in %s with %s as destination are removed by filters ",
						src.Config().Service, src.Config().Cluster.StableName(), dstInstances[0].Config().Service)
				}
				testFn(ctx, src, filteredDst)
			})
		})
	})
}

// RunFromClusters will generate and run one subtest to send traffic to
// destination instance. This is for ingress gateway testing when source instance
// destination instances. This can be used when we're not using echo workloads
// as the source of traffic, such as from the ingress gateway. For example:
//
//    RunFromClusters(func(t framework.TestContext, src cluster.Cluster, dst echo.Instances)) {
//      ingr := ist.IngressFor(src)
//      ingr.CallWithRetryOrFail(...)
//    })
func (t *T) RunFromClusters(testFn oneClusterOneTest) {
	t.toEachDeployment(t.rootCtx, func(ctx framework.TestContext, dstInstances echo.Instances) {
		t.setupPair(ctx, nil, echo.Services{dstInstances})
		if len(ctx.Clusters()) == 1 {
			testFn(ctx, ctx.Clusters()[0], dstInstances)
		} else {
			t.fromEachCluster(ctx, func(ctx framework.TestContext, c cluster.Cluster) {
				testFn(ctx, c, dstInstances)
			})
		}
	})
}

// fromEachCluster runs test from each cluster without requiring source deployment.
func (t *T) fromEachCluster(ctx framework.TestContext, testFn perClusterTest) {
	for _, srcCluster := range t.sources.Clusters() {
		srcCluster := srcCluster
		ctx.NewSubTestf("from %s", srcCluster.StableName()).Run(func(ctx framework.TestContext) {
			testFn(ctx, srcCluster)
		})
	}
}

// RunToN will generate nested subtests for every instance in every deployment subsets of the full set of deployments,
// such that every deployment is a destination at least once. To create as few subtests as possible, the same deployment
// may appear as a target in multiple subtests.
//
// Example: Given a as the only source, with a, b, c, d as destinationsand n = 3, we get the following subtests:
//     a/to_a_b_c/from_cluster_1:
//     a/to_a_b_c/from_cluster_2:
//     a/to_b_c_d/from_cluster_1:
//     a/to_b_c_d/from_cluster_2:
//
func (t *T) RunToN(n int, testFn oneToNTest) {
	t.fromEachDeployment(t.rootCtx, func(ctx framework.TestContext, srcInstances echo.Instances) {
		t.setup(ctx, srcInstances.Callers())
		t.toNDeployments(ctx, n, srcInstances, func(ctx framework.TestContext, destDeployments echo.Services) {
			t.setupPair(ctx, srcInstances.Callers(), destDeployments)
			t.fromEachWorkloadCluster(ctx, srcInstances, func(ctx framework.TestContext, src echo.Instance) {
				// reapply destination filters to only get the reachable instances for this cluster
				// this can be done safely since toNDeployments asserts the Services won't change
				destDeployments := t.applyCombinationFilters(src, destDeployments.Instances()).Services()
				testFn(ctx, src, destDeployments)
			})
		})
	})
}

func (t *T) RunViaIngress(testFn ingressTest) {
	i := istio.GetOrFail(t.rootCtx, t.rootCtx)
	t.toEachDeployment(t.rootCtx, func(ctx framework.TestContext, dstInstances echo.Instances) {
		t.setupPair(ctx, i.Ingresses().Callers(), echo.Services{dstInstances})
		doTest := func(ctx framework.TestContext, src cluster.Cluster, dst echo.Instances) {
			ingr := i.IngressFor(src)
			if ingr == nil {
				ctx.Skipf("no ingress for %s", src.StableName())
			}
			testFn(ctx, ingr, dst)
		}
		if len(ctx.Clusters()) == 1 {
			doTest(ctx, ctx.Clusters()[0], dstInstances)
		} else {
			t.fromEachCluster(ctx, func(ctx framework.TestContext, c cluster.Cluster) {
				doTest(ctx, c, dstInstances)
			})
		}
	})
}

// fromEachDeployment enumerates subtests for deployment with the structure <src>
// Intended to be used in combination with other helpers to enumerate subtests for destinations.
func (t *T) fromEachDeployment(ctx framework.TestContext, testFn perDeploymentTest) {
	duplicateShortnames := false
	shortnames := map[string]struct{}{}
	for _, src := range t.sources.Services() {
		svc := src[0].Config().Service
		if _, ok := shortnames[svc]; ok {
			duplicateShortnames = true
			break
		}
		shortnames[svc] = struct{}{}
	}

	for _, src := range t.sources.Services() {
		src := src
		subtestName := src[0].Config().Service
		if duplicateShortnames {
			subtestName += "." + src[0].Config().Namespace.Prefix()
		}
		ctx.NewSubTest(subtestName).Run(func(ctx framework.TestContext) {
			testFn(ctx, src)
		})
	}
}

// toEachDeployment enumerates subtests for every deployment as a destination, adding /to_<dst> to the parent test.
// Intended to be used in combination with other helpers which enumerates the subtests and chooses the srcInstnace.
func (t *T) toEachDeployment(ctx framework.TestContext, testFn perDeploymentTest) {
	for _, dst := range t.destinations.Services() {
		dst := dst
		ctx.NewSubTestf("to %s", dst[0].Config().Service).Run(func(ctx framework.TestContext) {
			testFn(ctx, dst)
		})
	}
}

func (t *T) fromEachWorkloadCluster(ctx framework.TestContext, src echo.Instances, testFn perInstanceTest) {
	for _, srcInstance := range src {
		srcInstance := srcInstance
		if len(ctx.Clusters()) == 1 && len(src) == 1 {
			testFn(ctx, srcInstance)
		} else {
			ctx.NewSubTestf("from %s", srcInstance.Config().Cluster.StableName()).Run(func(ctx framework.TestContext) {
				// assumes we don't change config from cluster to cluster
				ctx.SkipDumping()
				testFn(ctx, srcInstance)
			})
		}
	}
}

func (t *T) toNDeployments(ctx framework.TestContext, n int, srcs echo.Instances, testFn perNDeploymentTest) {
	// every eligible target instance from any cluster (map to dedupe)
	var commonTargets []string
	for _, src := range srcs {
		// eligible target instnaces from the src cluster
		filteredForSource := t.applyCombinationFilters(src, t.destinations)
		// make sure this src targets the same deployments (not necessarily the same instances) as other srcs
		targetNames := filteredForSource.Services().FQDNs()
		if len(commonTargets) == 0 {
			commonTargets = targetNames
		} else if !util.StringSliceEqual(targetNames, commonTargets) {
			ctx.Fatalf("%s in each cluster each cluster would not target the same set of deploments", src.Config().Service)
		}
	}
	// we take all instances that match the deployments
	// combination filters should be run again for individual sources
	filteredTargets := t.destinations.Services().MatchFQDNs(commonTargets...)
	for _, set := range nDestinations(ctx, n, filteredTargets) {
		set := set
		ctx.NewSubTestf("to %s", strings.Join(set.Services(), " ")).Run(func(ctx framework.TestContext) {
			testFn(ctx, set)
		})
	}
}

// nDestinations splits the given deployments into subsets of size n. A deployment may be present in multiple subsets to
// ensure every deployment is included.
func nDestinations(ctx framework.TestContext, n int, deployments echo.Services) (out []echo.Services) {
	nDests := len(deployments)
	if nDests < n {
		ctx.Fatalf("want to run with %d destinations but there are only %d total", n, nDests)
	}
	for i := 0; i < nDests; i += n {
		start := i
		if start+n-1 >= nDests {
			// re-use a few destinations to fit the entire slice in
			start = nDests - n
		}
		out = append(out, deployments[start:start+n])
	}
	return
}
