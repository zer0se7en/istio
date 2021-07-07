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

package common

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test"
	echoclient "istio.io/istio/pkg/test/echo/client"
	"istio.io/istio/pkg/test/echo/common/scheme"
	epb "istio.io/istio/pkg/test/echo/proto"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/common"
	"istio.io/istio/pkg/test/framework/components/echo/echotest"
	"istio.io/istio/pkg/test/framework/components/istio/ingress"
	"istio.io/istio/pkg/test/scopes"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/test/util/tmpl"
	ingressutil "istio.io/istio/tests/integration/security/sds_ingress/util"
)

const httpVirtualServiceTmpl = `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: {{.VirtualServiceHost}}
spec:
  gateways:
  - {{.Gateway}}
  hosts:
  - {{.VirtualServiceHost}}
  http:
  - route:
    - destination:
        host: {{.VirtualServiceHost}}
        port:
          number: {{.Port}}
{{- if .MatchScheme }}
    match:
    - scheme:
        exact: {{.MatchScheme}}
    headers:
      request:
        add:
          istio-custom-header: user-defined-value
{{- end }}
---
`

func httpVirtualService(gateway, host string, port int) string {
	return tmpl.MustEvaluate(httpVirtualServiceTmpl, struct {
		Gateway            string
		VirtualServiceHost string
		Port               int
		MatchScheme        string
	}{gateway, host, port, ""})
}

const gatewayTmpl = `
apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: gateway
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: {{.GatewayPort}}
      name: {{.GatewayPortName}}
      protocol: {{.GatewayProtocol}}
{{- if .Credential }}
    tls:
      mode: SIMPLE
      credentialName: {{.Credential}}
{{- end }}
    hosts:
    - "{{.GatewayHost}}"
---
`

func httpGateway(host string) string {
	return tmpl.MustEvaluate(gatewayTmpl, struct {
		GatewayHost     string
		GatewayPort     int
		GatewayPortName string
		GatewayProtocol string
		Credential      string
	}{
		host, 80, "http", "HTTP", "",
	})
}

func virtualServiceCases(skipVM bool) []TrafficTestCase {
	noTProxy := echotest.FilterMatch(func(instance echo.Instance) bool {
		return !instance.Config().IsTProxy()
	})
	var cases []TrafficTestCase
	cases = append(cases,
		TrafficTestCase{
			name: "added header",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
  - {{ .dstSvc }}
  http:
  - route:
    - destination:
        host: {{ .dstSvc }}
    headers:
      request:
        add:
          istio-custom-header: user-defined-value`,
			opts: echo.CallOptions{
				PortName: "http",
				Count:    1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.RawResponse["Istio-Custom-Header"], "user-defined-value", "request header")
							})
						})),
			},
			workloadAgnostic: true,
		},
		TrafficTestCase{
			name: "set header",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
  - {{ (index .dst 0).Config.Service }}
  http:
  - route:
    - destination:
        host: {{ (index .dst 0).Config.Service }}
    headers:
      request:
        set:
          x-custom: some-value`,
			opts: echo.CallOptions{
				PortName: "http",
				Count:    1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.RawResponse["X-Custom"], "some-value", "added request header")
							})
						})),
			},
			workloadAgnostic: true,
		},
		TrafficTestCase{
			name: "set authority header",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
  - {{ (index .dst 0).Config.Service }}
  http:
  - route:
    - destination:
        host: {{ (index .dst 0).Config.Service }}
    headers:
      request:
        set:
          :authority: my-custom-authority`,
			opts: echo.CallOptions{
				PortName: "http",
				Count:    1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.RawResponse["Host"], "my-custom-authority", "added authority header")
							})
						})),
			},
			workloadAgnostic: true,
			minIstioVersion:  "1.10.0",
		},
		TrafficTestCase{
			name: "set host header in destination",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
  - {{ (index .dst 0).Config.Service }}
  http:
  - route:
    - destination:
        host: {{ (index .dst 0).Config.Service }}
      headers:
        request:
          set:
            Host: my-custom-authority`,
			opts: echo.CallOptions{
				PortName: "http",
				Count:    1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.RawResponse["Host"], "my-custom-authority", "added authority header")
							})
						})),
			},
			workloadAgnostic: true,
			minIstioVersion:  "1.10.0",
		},
		TrafficTestCase{
			name: "redirect",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
    - {{ .dstSvc }}
  http:
  - match:
    - uri:
        exact: /foo
    redirect:
      uri: /new/path
  - match:
    - uri:
        exact: /new/path
    route:
    - destination:
        host: {{ .dstSvc }}`,
			opts: echo.CallOptions{
				PortName:        "http",
				Path:            "/foo?key=value",
				FollowRedirects: true,
				Count:           1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.URL, "/new/path?key=value", "URL")
							})
						})),
			},
			workloadAgnostic: true,
		},
		TrafficTestCase{
			name: "rewrite uri",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
    - {{ .dstSvc }}
  http:
  - match:
    - uri:
        exact: /foo
    rewrite:
      uri: /new/path
    route:
    - destination:
        host: {{ .dstSvc }}`,
			opts: echo.CallOptions{
				PortName: "http",
				Path:     "/foo?key=value#hash",
				Count:    1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.URL, "/new/path?key=value#hash", "URL")
							})
						})),
			},
			workloadAgnostic: true,
		},
		TrafficTestCase{
			name: "rewrite authority",
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
    - {{ .dstSvc }}
  http:
  - match:
    - uri:
        exact: /foo
    rewrite:
      authority: new-authority
    route:
    - destination:
        host: {{ .dstSvc }}`,
			opts: echo.CallOptions{
				PortName: "http",
				Path:     "/foo",
				Count:    1,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								return ExpectString(response.Host, "new-authority", "authority")
							})
						})),
			},
			workloadAgnostic: true,
		},
		TrafficTestCase{
			name: "cors",
			// TODO https://github.com/istio/istio/issues/31532
			targetFilters: []echotest.Filter{noTProxy, echotest.Not(echotest.VirtualMachines)},
			config: `
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
    - {{ .dstSvc }}
  http:
  - corsPolicy:
      allowOrigins:
      - exact: cors.com
      allowMethods:
      - POST
      - GET
      allowCredentials: false
      allowHeaders:
      - X-Foo-Bar
      - X-Foo-Baz
      maxAge: "24h"
    route:
    - destination:
        host: {{ .dstSvc }}
`,
			children: []TrafficCall{
				{
					name: "preflight",
					opts: func() echo.CallOptions {
						header := http.Header{}
						header.Add("Origin", "cors.com")
						header.Add("Access-Control-Request-Method", "DELETE")
						return echo.CallOptions{
							PortName: "http",
							Method:   "OPTIONS",
							Headers:  header,
							Count:    1,
							Validator: echo.And(
								echo.ExpectOK(),
								echo.ValidatorFunc(
									func(response echoclient.ParsedResponses, _ error) error {
										return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
											if err := ExpectString(response.RawResponse["Access-Control-Allow-Origin"],
												"cors.com", "preflight CORS origin"); err != nil {
												return err
											}
											if err := ExpectString(response.RawResponse["Access-Control-Allow-Methods"],
												"POST,GET", "preflight CORS method"); err != nil {
												return err
											}
											if err := ExpectString(response.RawResponse["Access-Control-Allow-Headers"],
												"X-Foo-Bar,X-Foo-Baz", "preflight CORS headers"); err != nil {
												return err
											}
											if err := ExpectString(response.RawResponse["Access-Control-Max-Age"],
												"86400", "preflight CORS max age"); err != nil {
												return err
											}
											return nil
										})
									})),
						}
					}(),
				},
				{
					name: "get",
					opts: func() echo.CallOptions {
						header := http.Header{}
						header.Add("Origin", "cors.com")
						return echo.CallOptions{
							PortName: "http",
							Headers:  header,
							Count:    1,
							Validator: echo.And(
								echo.ExpectOK(),
								echo.ValidatorFunc(
									func(response echoclient.ParsedResponses, _ error) error {
										return ExpectString(response[0].RawResponse["Access-Control-Allow-Origin"],
											"cors.com", "GET CORS origin")
									})),
						}
					}(),
				},
				{
					// GET without matching origin
					name: "get no origin match",
					opts: echo.CallOptions{
						PortName: "http",
						Count:    1,
						Validator: echo.And(
							echo.ExpectOK(),
							echo.ValidatorFunc(
								func(response echoclient.ParsedResponses, _ error) error {
									return ExpectString(response[0].RawResponse["Access-Control-Allow-Origin"], "", "mismatched CORS origin")
								})),
					},
				},
			},
			workloadAgnostic: true,
		},
	)

	// reduce the total # of subtests that don't give valuable coverage or just don't work
	noNaked := echotest.FilterMatch(echo.Not(echo.IsNaked()))
	noHeadless := echotest.FilterMatch(echo.Not(echo.IsHeadless()))
	noExternal := echotest.FilterMatch(echo.Not(echo.IsExternal()))
	for i, tc := range cases {
		// TODO include proxyless as different features become supported
		tc.sourceFilters = append(tc.sourceFilters, noNaked, noHeadless, noProxyless)
		tc.targetFilters = append(tc.targetFilters, noNaked, noHeadless, noProxyless)
		cases[i] = tc
	}

	splits := [][]int{
		{50, 25, 25},
		{80, 10, 10},
	}
	if skipVM {
		splits = [][]int{
			{50, 50},
			{80, 20},
		}
	}
	for _, split := range splits {
		split := split
		cases = append(cases, TrafficTestCase{
			name:          fmt.Sprintf("shifting-%d", split[0]),
			toN:           len(split),
			sourceFilters: []echotest.Filter{noHeadless, noNaked, noProxyless},
			targetFilters: []echotest.Filter{noHeadless, noExternal, noProxyless},
			templateVars: func(_ echo.Callers, _ echo.Instances) map[string]interface{} {
				return map[string]interface{}{
					"split": split,
				}
			},
			config: `
{{ $split := .split }} 
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: default
spec:
  hosts:
    - {{ ( index .dstSvcs 0) }}
  http:
  - route:
{{- range $idx, $svc := .dstSvcs }}
    - destination:
        host: {{ $svc }}
      weight: {{ ( index $split $idx ) }}
{{- end }}
`,
			validateForN: func(src echo.Caller, dests echo.Services) echo.Validator {
				return echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(func(responses echoclient.ParsedResponses, err error) error {
						errorThreshold := 10
						if len(split) != len(dests) {
							// shouldn't happen
							return fmt.Errorf("split configured for %d destinations, but framework gives %d", len(split), len(dests))
						}
						splitPerHost := map[string]int{}
						for i, pct := range split {
							splitPerHost[dests.Services()[i]] = pct
						}
						for host, exp := range splitPerHost {
							hostResponses := responses.Match(func(r *echoclient.ParsedResponse) bool {
								return strings.HasPrefix(r.Hostname, host)
							})
							if !AlmostEquals(len(hostResponses), exp, errorThreshold) {
								return fmt.Errorf("expected %v calls to %q, got %v", exp, host, len(hostResponses))
							}
							// echotest should have filtered the deployment to only contain reachable clusters
							hostDests := dests.Instances().Match(echo.Service(host))
							targetClusters := hostDests.Clusters()
							// don't check headless since lb is unpredictable
							headlessTarget := hostDests.ContainsMatch(echo.IsHeadless())
							if !headlessTarget && len(targetClusters.ByNetwork()[src.(echo.Instance).Config().Cluster.NetworkName()]) > 1 {
								// Conditionally check reached clusters to work around connection load balancing issues
								// See https://github.com/istio/istio/issues/32208 for details
								// We want to skip this for requests from the cross-network pod
								if err := hostResponses.CheckReachedClusters(targetClusters); err != nil {
									return fmt.Errorf("did not reach all clusters for %s: %v", host, err)
								}
							}
						}
						return nil
					}))
			},
			opts: echo.CallOptions{
				PortName: "http",
				Count:    100,
			},
			workloadAgnostic: true,
		})
	}

	return cases
}

func HostHeader(header string) http.Header {
	h := http.Header{}
	h["Host"] = []string{header}
	return h
}

// tlsOriginationCases contains tests TLS origination from DestinationRule
func tlsOriginationCases(apps *EchoDeployments) []TrafficTestCase {
	tc := TrafficTestCase{
		name: "",
		config: fmt.Sprintf(`
apiVersion: networking.istio.io/v1alpha3
kind: DestinationRule
metadata:
  name: external
spec:
  host: %s
  trafficPolicy:
    tls:
      mode: SIMPLE
`, apps.External[0].Config().DefaultHostHeader),
		children: []TrafficCall{},
	}
	expects := []struct {
		port int
		alpn string
	}{
		{8888, "http/1.1"},
		{8882, "h2"},
	}
	for _, c := range apps.PodA {
		for _, e := range expects {
			c := c
			e := e

			tc.children = append(tc.children, TrafficCall{
				name: fmt.Sprintf("%s: %s", c.Config().Cluster.StableName(), e.alpn),
				opts: echo.CallOptions{
					Port:      &echo.Port{ServicePort: e.port, Protocol: protocol.HTTP},
					Count:     1,
					Address:   apps.External[0].Address(),
					Headers:   HostHeader(apps.External[0].Config().DefaultHostHeader),
					Scheme:    scheme.HTTP,
					Validator: echo.And(echo.ExpectOK(), echo.ExpectKey("Alpn", e.alpn)),
				},
				call: c.CallWithRetryOrFail,
			})
		}
	}
	return []TrafficTestCase{tc}
}

// useClientProtocolCases contains tests use_client_protocol from DestinationRule
func useClientProtocolCases(apps *EchoDeployments) []TrafficTestCase {
	var cases []TrafficTestCase
	client := apps.PodA
	destination := apps.PodC[0]
	cases = append(cases,
		TrafficTestCase{
			name:   "use client protocol with h2",
			config: useClientProtocolDestinationRule("use-client-protocol-h2", destination.Config().Service),
			call:   client[0].CallWithRetryOrFail,
			opts: echo.CallOptions{
				Target:   destination,
				PortName: "http",
				Count:    1,
				HTTP2:    true,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ExpectKey("Proto", "HTTP/2.0"),
				),
			},
			minIstioVersion: "1.10.0",
		},
		TrafficTestCase{
			name:   "use client protocol with h1",
			config: useClientProtocolDestinationRule("use-client-protocol-h1", destination.Config().Service),
			call:   client[0].CallWithRetryOrFail,
			opts: echo.CallOptions{
				PortName: "http",
				Count:    1,
				Target:   destination,
				HTTP2:    false,
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ExpectKey("Proto", "HTTP/1.1"),
				),
			},
		},
	)
	return cases
}

// destinationRuleCases contains tests some specific DestinationRule tests.
func destinationRuleCases(apps *EchoDeployments) []TrafficTestCase {
	var cases []TrafficTestCase
	client := apps.PodA
	destination := apps.PodC[0]
	cases = append(cases,
		// Validates the config is generated correctly when only idletimeout is specified in DR.
		TrafficTestCase{
			name:   "only idletimeout specified in DR",
			config: idletimeoutDestinationRule("idletimeout-dr", destination.Config().Service),
			call:   client[0].CallWithRetryOrFail,
			opts: echo.CallOptions{
				Target:    destination,
				PortName:  "http",
				Count:     1,
				HTTP2:     true,
				Validator: echo.ExpectOK(),
			},
			minIstioVersion: "1.10.0",
		},
	)
	return cases
}

// trafficLoopCases contains tests to ensure traffic does not loop through the sidecar
func trafficLoopCases(apps *EchoDeployments) []TrafficTestCase {
	cases := []TrafficTestCase{}
	for _, c := range apps.PodA {
		for _, d := range apps.PodB {
			for _, port := range []string{"15001", "15006"} {
				c, d, port := c, d, port
				cases = append(cases, TrafficTestCase{
					name: port,
					call: func(t test.Failer, options echo.CallOptions, retryOptions ...retry.Option) echoclient.ParsedResponses {
						dwl := d.WorkloadsOrFail(t)[0]
						cwl := c.WorkloadsOrFail(t)[0]
						resp, err := cwl.ForwardEcho(context.Background(), &epb.ForwardEchoRequest{
							Url:   fmt.Sprintf("http://%s:%s", dwl.Address(), port),
							Count: 1,
						})
						// Ideally we would actually check to make sure we do not blow up the pod,
						// but I couldn't find a way to reliably detect this.
						if err == nil {
							t.Fatalf("expected request to fail, but it didn't: %v", resp)
						}
						return nil
					},
				})
			}
		}
	}
	return cases
}

// autoPassthroughCases tests that we cannot hit unexpected destinations when using AUTO_PASSTHROUGH
func autoPassthroughCases(apps *EchoDeployments) []TrafficTestCase {
	cases := []TrafficTestCase{}
	// We test the cross product of all Istio ALPNs (or no ALPN), all mTLS modes, and various backends
	alpns := []string{"istio", "istio-peer-exchange", "istio-http/1.0", "istio-http/1.1", "istio-h2", ""}
	modes := []string{"STRICT", "PERMISSIVE", "DISABLE"}

	mtlsHost := host.Name(apps.PodA[0].Config().FQDN())
	nakedHost := host.Name(apps.Naked[0].Config().FQDN())
	httpsPort := FindPortByName("https").ServicePort
	httpsAutoPort := FindPortByName("auto-https").ServicePort
	snis := []string{
		model.BuildSubsetKey(model.TrafficDirectionOutbound, "", mtlsHost, httpsPort),
		model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, "", mtlsHost, httpsPort),
		model.BuildSubsetKey(model.TrafficDirectionOutbound, "", nakedHost, httpsPort),
		model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, "", nakedHost, httpsPort),
		model.BuildSubsetKey(model.TrafficDirectionOutbound, "", mtlsHost, httpsAutoPort),
		model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, "", mtlsHost, httpsAutoPort),
		model.BuildSubsetKey(model.TrafficDirectionOutbound, "", nakedHost, httpsAutoPort),
		model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, "", nakedHost, httpsAutoPort),
	}
	for _, mode := range modes {
		childs := []TrafficCall{}
		for _, sni := range snis {
			for _, alpn := range alpns {
				alpn, sni, mode := alpn, sni, mode
				al := &epb.Alpn{Value: []string{alpn}}
				if alpn == "" {
					al = nil
				}
				childs = append(childs, TrafficCall{
					name: fmt.Sprintf("mode:%v,sni:%v,alpn:%v", mode, sni, alpn),
					call: apps.Ingress.CallWithRetryOrFail,
					opts: echo.CallOptions{
						Port: &echo.Port{
							ServicePort: 443,
							Protocol:    protocol.HTTPS,
						},
						ServerName: sni,
						Alpn:       al,
						Validator:  echo.ExpectError(),
					},
				},
				)
			}
		}
		cases = append(cases, TrafficTestCase{
			config: globalPeerAuthentication(mode) + `
---
apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: cross-network-gateway-test
  namespace: istio-system
spec:
  selector:
    istio: ingressgateway
  servers:
    - port:
        number: 443
        name: tls
        protocol: TLS
      tls:
        mode: AUTO_PASSTHROUGH
      hosts:
        - "*.local"
`,
			children: childs,
		})
	}

	return cases
}

func gatewayCases() []TrafficTestCase {
	templateParams := func(protocol protocol.Instance, src echo.Callers, dests echo.Instances) map[string]interface{} {
		host, dest, portN, cred := "*", dests[0], 80, ""
		if protocol.IsTLS() {
			host, portN, cred = dest.Config().FQDN(), 443, "cred"
		}
		return map[string]interface{}{
			"IngressNamespace":   src[0].(ingress.Instance).Namespace(),
			"GatewayHost":        host,
			"GatewayPort":        portN,
			"GatewayPortName":    strings.ToLower(string(protocol)),
			"GatewayProtocol":    string(protocol),
			"Gateway":            "gateway",
			"VirtualServiceHost": dest.Config().FQDN(),
			"Port":               dest.Config().PortByName("http").ServicePort,
			"Credential":         cred,
		}
	}

	// clears the Target to avoid echo internals trying to match the protocol with the port on echo.Config
	noTarget := func(_ echo.Caller, _ echo.Instances, opts *echo.CallOptions) {
		opts.Target = nil
	}
	// allows setting the target indirectly via the host header
	fqdnHostHeader := func(src echo.Caller, dsts echo.Instances, opts *echo.CallOptions) {
		if opts.Headers == nil {
			opts.Headers = map[string][]string{}
		}
		opts.Headers["Host"] = []string{dsts[0].Config().FQDN()}
		noTarget(src, dsts, opts)
	}

	// SingleRegualrPod is already applied leaving one regular pod, to only regular pods should leave a single workload.
	singleTarget := []echotest.Filter{echotest.FilterMatch(echotest.RegularPod)}
	// the following cases don't actually target workloads, we use the singleTarget filter to avoid duplicate cases
	cases := []TrafficTestCase{
		{
			name:             "404",
			targetFilters:    singleTarget,
			workloadAgnostic: true,
			viaIngress:       true,
			config:           httpGateway("*"),
			opts: echo.CallOptions{
				Count: 1,
				Port: &echo.Port{
					Protocol: protocol.HTTP,
				},
				Headers: map[string][]string{
					"Host": {"foo.bar"},
				},
				Validator: echo.ExpectCode("404"),
			},
			setupOpts: noTarget,
		},
		{
			name:             "https redirect",
			targetFilters:    singleTarget,
			workloadAgnostic: true,
			viaIngress:       true,
			config: `apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: gateway
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts:
    - "*"
    tls:
      httpsRedirect: true
---
`,
			opts: echo.CallOptions{
				Count: 1,
				Port: &echo.Port{
					Protocol: protocol.HTTP,
				},
				Validator: echo.ExpectCode("301"),
			},
			setupOpts: fqdnHostHeader,
		},
		{
			// See https://github.com/istio/istio/issues/27315
			name:             "https with x-forwarded-proto",
			targetFilters:    singleTarget,
			workloadAgnostic: true,
			viaIngress:       true,
			config: `apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: gateway
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts:
    - "*"
    tls:
      httpsRedirect: true
---
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: ingressgateway-redirect-config
  namespace: istio-system
spec:
  configPatches:
  - applyTo: NETWORK_FILTER
    match:
      context: GATEWAY
      listener:
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
    patch:
      operation: MERGE
      value:
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          xff_num_trusted_hops: 1
          normalize_path: true
  workloadSelector:
    labels:
      istio: ingressgateway
---
` + httpVirtualServiceTmpl,
			opts: echo.CallOptions{
				Count: 1,
				Port: &echo.Port{
					Protocol: protocol.HTTP,
				},
				Headers: map[string][]string{
					// In real world, this may be set by a downstream LB that terminates the TLS
					"X-Forwarded-Proto": {"https"},
				},
				Validator: echo.ExpectOK(),
			},
			setupOpts: fqdnHostHeader,
			templateVars: func(_ echo.Callers, dests echo.Instances) map[string]interface{} {
				dest := dests[0]
				return map[string]interface{}{
					"Gateway":            "gateway",
					"VirtualServiceHost": dest.Config().FQDN(),
					"Port":               dest.Config().PortByName("http").ServicePort,
				}
			},
		},
	}

	for _, proto := range []protocol.Instance{protocol.HTTP, protocol.HTTPS} {
		proto, secret := proto, ""
		if proto.IsTLS() {
			secret = ingressutil.IngressKubeSecretYAML("cred", "{{.IngressNamespace}}", ingressutil.TLS, ingressutil.IngressCredentialA)
		}
		cases = append(
			cases,
			TrafficTestCase{
				name:   string(proto),
				config: gatewayTmpl + httpVirtualServiceTmpl + secret,
				templateVars: func(src echo.Callers, dests echo.Instances) map[string]interface{} {
					return templateParams(proto, src, dests)
				},
				setupOpts: fqdnHostHeader,
				opts: echo.CallOptions{
					Count: 1,
					Port: &echo.Port{
						Protocol: proto,
					},
				},
				viaIngress:       true,
				workloadAgnostic: true,
			},
			TrafficTestCase{
				name:   fmt.Sprintf("%s scheme match", proto),
				config: gatewayTmpl + httpVirtualServiceTmpl + secret,
				templateVars: func(src echo.Callers, dests echo.Instances) map[string]interface{} {
					params := templateParams(proto, src, dests)
					params["MatchScheme"] = strings.ToLower(string(proto))
					return params
				},
				setupOpts: fqdnHostHeader,
				opts: echo.CallOptions{
					Count: 1,
					Port: &echo.Port{
						Protocol: proto,
					},
					Validator: echo.And(
						echo.ExpectOK(),
						echo.ValidatorFunc(
							func(response echoclient.ParsedResponses, _ error) error {
								return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
									// We check a header is added to ensure our VS actually applied
									return ExpectString(response.RawResponse["Istio-Custom-Header"], "user-defined-value", "request header")
								})
							})),
				},
				// to keep tests fast, we only run the basic protocol test per-workload and scheme match once (per cluster)
				targetFilters:    singleTarget,
				viaIngress:       true,
				workloadAgnostic: true,
			},
		)
	}

	return cases
}

func XFFGatewayCase(apps *EchoDeployments, gateway string) []TrafficTestCase {
	cases := []TrafficTestCase{}

	destinationSets := []echo.Instances{
		apps.PodA,
	}

	for _, d := range destinationSets {
		d := d
		if len(d) == 0 {
			continue
		}
		fqdn := d[0].Config().FQDN()
		cases = append(cases, TrafficTestCase{
			name:   d[0].Config().Service,
			config: httpGateway("*") + httpVirtualService("gateway", fqdn, d[0].Config().PortByName("http").ServicePort),
			skip:   false,
			call:   apps.Naked[0].CallWithRetryOrFail,
			opts: echo.CallOptions{
				Count:   1,
				Port:    &echo.Port{ServicePort: 80},
				Scheme:  scheme.HTTP,
				Address: gateway,
				Headers: map[string][]string{
					"X-Forwarded-For": {"56.5.6.7, 72.9.5.6, 98.1.2.3"},
					"Host":            {fqdn},
				},
				Validator: echo.ValidatorFunc(
					func(response echoclient.ParsedResponses, _ error) error {
						return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
							externalAddress, ok := response.RawResponse["X-Envoy-External-Address"]
							if !ok {
								return fmt.Errorf("missing X-Envoy-External-Address Header")
							}
							if err := ExpectString(externalAddress, "72.9.5.6", "envoy-external-address header"); err != nil {
								return err
							}
							xffHeader, ok := response.RawResponse["X-Forwarded-For"]
							if !ok {
								return fmt.Errorf("missing X-Forwarded-For Header")
							}

							xffIPs := strings.Split(xffHeader, ",")
							if len(xffIPs) != 4 {
								return fmt.Errorf("did not receive expected 4 hosts in X-Forwarded-For header")
							}

							return ExpectString(strings.TrimSpace(xffIPs[1]), "72.9.5.6", "ip in xff header")
						})
					}),
			},
		})
	}
	return cases
}

// serviceCases tests overlapping Services. There are a few cases.
// Consider we have our base service B, with service port P and target port T
// 1) Another service, B', with P -> T. In this case, both the listener and the cluster will conflict.
//    Because everything is workload oriented, this is not a problem unless they try to make them different
//    protocols (this is explicitly called out as "not supported") or control inbound connectionPool settings
//    (which is moving to Sidecar soon)
// 2) Another service, B', with P -> T'. In this case, the listener will be distinct, since its based on the target.
//    The cluster, however, will be shared, which is broken, because we should be forwarding to T when we call B, and T' when we call B'.
// 3) Another service, B', with P' -> T. In this case, the listener is shared. This is fine, with the exception of different protocols
//    The cluster is distinct.
// 4) Another service, B', with P' -> T'. There is no conflicts here at all.
func serviceCases(apps *EchoDeployments) []TrafficTestCase {
	cases := []TrafficTestCase{}
	for _, c := range apps.PodA {
		c := c

		// Case 1
		// Identical to port "http" or service B, just behind another service name
		svc := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: b-alt-1
  labels:
    app: b
spec:
  ports:
  - name: http
    port: %d
    targetPort: %d
  selector:
    app: b`, FindPortByName("http").ServicePort, FindPortByName("http").InstancePort)
		cases = append(cases, TrafficTestCase{
			name:   fmt.Sprintf("case 1 both match in cluster %v", c.Config().Cluster.StableName()),
			config: svc,
			call:   c.CallWithRetryOrFail,
			opts: echo.CallOptions{
				Count:     1,
				Address:   "b-alt-1",
				Port:      &echo.Port{ServicePort: FindPortByName("http").ServicePort, Protocol: protocol.HTTP},
				Timeout:   time.Millisecond * 100,
				Validator: echo.ExpectOK(),
			},
		})

		// Case 2
		// We match the service port, but forward to a different port
		// Here we make the new target tcp so the test would fail if it went to the http port
		svc = fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: b-alt-2
  labels:
    app: b
spec:
  ports:
  - name: tcp
    port: %d
    targetPort: %d
  selector:
    app: b`, FindPortByName("http").ServicePort, common.WorkloadPorts[0].Port)
		cases = append(cases, TrafficTestCase{
			name:   fmt.Sprintf("case 2 service port match in cluster %v", c.Config().Cluster.StableName()),
			config: svc,
			call:   c.CallWithRetryOrFail,
			opts: echo.CallOptions{
				Count:     1,
				Address:   "b-alt-2",
				Port:      &echo.Port{ServicePort: FindPortByName("http").ServicePort, Protocol: protocol.TCP},
				Scheme:    scheme.TCP,
				Timeout:   time.Millisecond * 100,
				Validator: echo.ExpectOK(),
			},
		})

		// Case 3
		// We match the target port, but front with a different service port
		svc = fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: b-alt-3
  labels:
    app: b
spec:
  ports:
  - name: http
    port: 12345
    targetPort: %d
  selector:
    app: b`, FindPortByName("http").InstancePort)
		cases = append(cases, TrafficTestCase{
			name:   fmt.Sprintf("case 3 target port match in cluster %v", c.Config().Cluster.StableName()),
			config: svc,
			call:   c.CallWithRetryOrFail,
			opts: echo.CallOptions{
				Count:     1,
				Address:   "b-alt-3",
				Port:      &echo.Port{ServicePort: 12345, Protocol: protocol.HTTP},
				Timeout:   time.Millisecond * 100,
				Validator: echo.ExpectOK(),
			},
		})

		// Case 4
		// Completely new set of ports
		svc = fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: b-alt-4
  labels:
    app: b
spec:
  ports:
  - name: http
    port: 12346
    targetPort: %d
  selector:
    app: b`, common.WorkloadPorts[1].Port)
		cases = append(cases, TrafficTestCase{
			name:   fmt.Sprintf("case 4 no match in cluster %v", c.Config().Cluster.StableName()),
			config: svc,
			call:   c.CallWithRetryOrFail,
			opts: echo.CallOptions{
				Count:     1,
				Address:   "b-alt-4",
				Port:      &echo.Port{ServicePort: 12346, Protocol: protocol.HTTP},
				Timeout:   time.Millisecond * 100,
				Validator: echo.ExpectOK(),
			},
		})
	}

	return cases
}

// consistentHashCases tests destination rule's consistent hashing mechanism
func consistentHashCases(apps *EchoDeployments) []TrafficTestCase {
	cases := []TrafficTestCase{}
	for _, c := range apps.PodA {
		c := c

		// First setup a service selecting a few services. This is needed to ensure we can load balance across many pods.
		svcName := "consistent-hash"
		if nw := c.Config().Cluster.NetworkName(); nw != "" {
			svcName += "-" + nw
		}
		svc := tmpl.MustEvaluate(`apiVersion: v1
kind: Service
metadata:
  name: {{.Service}}
spec:
  ports:
  - name: http
    port: {{.Port}}
    targetPort: {{.TargetPort}}
  selector:
    test.istio.io/class: standard
    {{- if .Network }}
    topology.istio.io/network: {{.Network}}
	{{- end }}
`, map[string]interface{}{
			"Service":    svcName,
			"Network":    c.Config().Cluster.NetworkName(),
			"Port":       FindPortByName("http").ServicePort,
			"TargetPort": FindPortByName("http").InstancePort,
		})

		destRule := fmt.Sprintf(`
---
apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: %s
spec:
  host: %s
  trafficPolicy:
    loadBalancer:
      consistentHash:
        {{. | indent 8}}
`, svcName, svcName)
		// Add a negative test case. This ensures that the test is actually valid; its not a super trivial check
		// and could be broken by having only 1 pod so its good to have this check in place
		cases = append(cases, TrafficTestCase{
			name:   "no consistent",
			config: svc,
			call:   c.CallWithRetryOrFail,
			opts: echo.CallOptions{
				Count:   10,
				Address: svcName,
				Port:    &echo.Port{ServicePort: FindPortByName("http").ServicePort, Protocol: protocol.HTTP},
				Validator: echo.And(
					echo.ExpectOK(),
					echo.ValidatorFunc(func(responses echoclient.ParsedResponses, rerr error) error {
						err := ConsistentHostValidator.Validate(responses, rerr)
						if err == nil {
							return fmt.Errorf("expected inconsistent hash, but it was consistent")
						}
						return nil
					}),
				),
			},
		})
		headers := http.Header{}
		headers.Add("x-some-header", "baz")
		callOpts := echo.CallOptions{
			Count:   10,
			Address: svcName,
			Path:    "/?some-query-param=bar",
			Headers: headers,
			Port:    &echo.Port{ServicePort: FindPortByName("http").ServicePort, Protocol: protocol.HTTP},
			Validator: echo.And(
				echo.ExpectOK(),
				ConsistentHostValidator,
			),
		}
		// Setup tests for various forms of the API
		// TODO: it may be necessary to vary the inputs of the hash and ensure we get a different backend
		// But its pretty hard to test that, so for now just ensure we hit the same one.
		cases = append(cases, TrafficTestCase{
			name:   "source ip",
			config: svc + tmpl.MustEvaluate(destRule, "useSourceIp: true"),
			call:   c.CallWithRetryOrFail,
			opts:   callOpts,
		}, TrafficTestCase{
			name:   "query param",
			config: svc + tmpl.MustEvaluate(destRule, "httpQueryParameterName: some-query-param"),
			call:   c.CallWithRetryOrFail,
			opts:   callOpts,
		}, TrafficTestCase{
			name:   "http header",
			config: svc + tmpl.MustEvaluate(destRule, "httpHeaderName: x-some-header"),
			call:   c.CallWithRetryOrFail,
			opts:   callOpts,
		})
	}

	return cases
}

var ConsistentHostValidator echo.Validator = echo.ValidatorFunc(func(responses echoclient.ParsedResponses, _ error) error {
	hostnames := make([]string, len(responses))
	_ = responses.Check(func(i int, response *echoclient.ParsedResponse) error {
		hostnames[i] = response.Hostname
		return nil
	})
	scopes.Framework.Infof("requests landed on hostnames: %v", hostnames)
	unique := sets.NewSet(hostnames...).SortedList()
	if len(unique) != 1 {
		return fmt.Errorf("excepted only one destination, got: %v", unique)
	}
	return nil
})

func flatten(clients ...[]echo.Instance) []echo.Instance {
	instances := []echo.Instance{}
	for _, c := range clients {
		instances = append(instances, c...)
	}
	return instances
}

// selfCallsCases checks that pods can call themselves
func selfCallsCases() []TrafficTestCase {
	cases := []TrafficTestCase{
		// Calls to the Service will go through envoy outbound and inbound, so we get envoy headers added
		{
			name:             "to service",
			workloadAgnostic: true,
			opts: echo.CallOptions{
				Count:     1,
				PortName:  "http",
				Validator: echo.And(echo.ExpectOK(), echo.ExpectKey("X-Envoy-Attempt-Count", "1")),
			},
		},
		// Localhost calls will go directly to localhost, bypassing Envoy. No envoy headers added.
		{
			name:             "to localhost",
			workloadAgnostic: true,
			setupOpts: func(_ echo.Caller, _ echo.Instances, opts *echo.CallOptions) {
				// the framework will try to set this when enumerating test cases
				opts.Target = nil
			},
			opts: echo.CallOptions{
				Count:     1,
				Address:   "localhost",
				Port:      &echo.Port{ServicePort: 8080},
				Scheme:    scheme.HTTP,
				Validator: echo.And(echo.ExpectOK(), echo.ExpectKey("X-Envoy-Attempt-Count", "")),
			},
		},
		// PodIP calls will go directly to podIP, bypassing Envoy. No envoy headers added.
		{
			name:             "to podIP",
			workloadAgnostic: true,
			setupOpts: func(srcCaller echo.Caller, _ echo.Instances, opts *echo.CallOptions) {
				src := srcCaller.(echo.Instance)
				workloads, _ := src.Workloads()
				opts.Address = workloads[0].Address()
				// the framework will try to set this when enumerating test cases
				opts.Target = nil
			},
			opts: echo.CallOptions{
				Count:     1,
				Scheme:    scheme.HTTP,
				Port:      &echo.Port{ServicePort: 8080},
				Validator: echo.And(echo.ExpectOK(), echo.ExpectKey("X-Envoy-Attempt-Count", "")),
			},
		},
	}
	for i, tc := range cases {
		// proxyless doesn't get valuable coverage here
		tc.sourceFilters = []echotest.Filter{
			echotest.Not(echotest.ExternalServices),
			echotest.Not(echotest.FilterMatch(echo.IsNaked())),
			echotest.Not(echotest.FilterMatch(echo.IsHeadless())),
			noProxyless,
		}
		tc.comboFilters = []echotest.CombinationFilter{func(from echo.Instance, to echo.Instances) echo.Instances {
			return to.Match(echo.FQDN(from.Config().FQDN()))
		}}
		cases[i] = tc
	}

	return cases
}

// Todo merge with security TestReachability code
func protocolSniffingCases() []TrafficTestCase {
	cases := []TrafficTestCase{}

	type protocolCase struct {
		// The port we call
		port string
		// The actual type of traffic we send to the port
		scheme scheme.Instance
	}
	protocols := []protocolCase{
		{"http", scheme.HTTP},
		{"auto-http", scheme.HTTP},
		{"tcp", scheme.TCP},
		{"auto-tcp", scheme.TCP},
		{"grpc", scheme.GRPC},
		{"auto-grpc", scheme.GRPC},
	}

	// so we can validate all clusters are hit
	for _, call := range protocols {
		call := call
		cases = append(cases, TrafficTestCase{
			// TODO(https://github.com/istio/istio/issues/26798) enable sniffing tcp
			skip: call.scheme == scheme.TCP,
			name: call.port,
			opts: echo.CallOptions{
				Count:    1,
				PortName: call.port,
				Scheme:   call.scheme,
				Timeout:  time.Second * 5,
			},
			validate: func(src echo.Caller, dst echo.Instances) echo.Validator {
				if call.scheme == scheme.TCP {
					// no host header for TCP
					return echo.ExpectOK()
				}
				return echo.And(
					echo.ExpectOK(),
					echo.ExpectHost(dst[0].Config().HostHeader()))
			},
			comboFilters: func() []echotest.CombinationFilter {
				if call.scheme != scheme.GRPC {
					return []echotest.CombinationFilter{func(from echo.Instance, to echo.Instances) echo.Instances {
						if from.Config().IsProxylessGRPC() && to.ContainsMatch(echo.IsVirtualMachine()) {
							return nil
						}
						return to
					}}
				}
				return nil
			}(),
			workloadAgnostic: true,
		})
	}
	return cases
}

// Todo merge with security TestReachability code
func instanceIPTests(apps *EchoDeployments) []TrafficTestCase {
	cases := []TrafficTestCase{}
	ipCases := []struct {
		name            string
		endpoint        string
		disableSidecar  bool
		port            string
		code            int
		minIstioVersion string
	}{
		// instance IP bind
		{
			name:           "instance IP without sidecar",
			disableSidecar: true,
			port:           "http-instance",
			code:           200,
		},
		{
			name:     "instance IP with wildcard sidecar",
			endpoint: "0.0.0.0",
			port:     "http-instance",
			code:     200,
		},
		{
			name:     "instance IP with localhost sidecar",
			endpoint: "127.0.0.1",
			port:     "http-instance",
			code:     503,
		},
		{
			name:     "instance IP with empty sidecar",
			endpoint: "",
			port:     "http-instance",
			code:     200,
		},

		// Localhost bind
		{
			name:           "localhost IP without sidecar",
			disableSidecar: true,
			port:           "http-localhost",
			code:           503,
			// when testing with pre-1.10 versions this request succeeds
			minIstioVersion: "1.10.0",
		},
		{
			name:     "localhost IP with wildcard sidecar",
			endpoint: "0.0.0.0",
			port:     "http-localhost",
			code:     503,
		},
		{
			name:     "localhost IP with localhost sidecar",
			endpoint: "127.0.0.1",
			port:     "http-localhost",
			code:     200,
		},
		{
			name:     "localhost IP with empty sidecar",
			endpoint: "",
			port:     "http-localhost",
			code:     503,
			// when testing with pre-1.10 versions this request succeeds
			minIstioVersion: "1.10.0",
		},

		// Wildcard bind
		{
			name:           "wildcard IP without sidecar",
			disableSidecar: true,
			port:           "http",
			code:           200,
		},
		{
			name:     "wildcard IP with wildcard sidecar",
			endpoint: "0.0.0.0",
			port:     "http",
			code:     200,
		},
		{
			name:     "wildcard IP with localhost sidecar",
			endpoint: "127.0.0.1",
			port:     "http",
			code:     200,
		},
		{
			name:     "wildcard IP with empty sidecar",
			endpoint: "",
			port:     "http",
			code:     200,
		},
	}
	for _, ipCase := range ipCases {
		for _, client := range apps.PodA {
			ipCase := ipCase
			client := client
			destination := apps.PodB[0]
			var config string
			if !ipCase.disableSidecar {
				config = fmt.Sprintf(`
apiVersion: networking.istio.io/v1alpha3
kind: Sidecar
metadata:
  name: sidecar
spec:
  workloadSelector:
    labels:
      app: b
  egress:
  - hosts:
    - "./*"
  ingress:
  - port:
      number: %d
      protocol: HTTP
    defaultEndpoint: %s:%d
`, FindPortByName(ipCase.port).InstancePort, ipCase.endpoint, FindPortByName(ipCase.port).InstancePort)
			}
			cases = append(cases,
				TrafficTestCase{
					name:   ipCase.name,
					call:   client.CallWithRetryOrFail,
					config: config,
					opts: echo.CallOptions{
						Count:     1,
						Target:    destination,
						PortName:  ipCase.port,
						Scheme:    scheme.HTTP,
						Timeout:   time.Second * 5,
						Validator: echo.ExpectCode(fmt.Sprint(ipCase.code)),
					},
					minIstioVersion: ipCase.minIstioVersion,
				})
		}
	}

	for _, tc := range cases {
		// proxyless doesn't get valuable coverage here
		noProxyless := echotest.FilterMatch(echo.Not(echo.IsProxylessGRPC()))
		tc.sourceFilters = append(tc.sourceFilters, noProxyless)
		tc.targetFilters = append(tc.targetFilters, noProxyless)
	}

	return cases
}

type vmCase struct {
	name string
	from echo.Instance
	to   echo.Instances
	host string
}

func DNSTestCases(apps *EchoDeployments, cniEnabled bool) []TrafficTestCase {
	makeSE := func(ips ...string) string {
		return tmpl.MustEvaluate(`
apiVersion: networking.istio.io/v1alpha3
kind: ServiceEntry
metadata:
  name: dns
spec:
  hosts:
  - "fake.service.local"
  addresses:
{{ range $ip := .IPs }}
  - "{{$ip}}"
{{ end }}
  resolution: STATIC
  endpoints: []
  ports:
  - number: 80
    name: http
    protocol: HTTP
`, map[string]interface{}{"IPs": ips})
	}
	tcases := []TrafficTestCase{}
	ipv4 := "1.2.3.4"
	ipv6 := "1234:1234:1234::1234:1234:1234"
	dummyLocalhostServer := "127.0.0.1"
	cases := []struct {
		name string
		// TODO(https://github.com/istio/istio/issues/30282) support multiple vips
		ips      string
		protocol string
		server   string
		skipCNI  bool
		expected []string
	}{
		{
			name:     "tcp ipv4",
			ips:      ipv4,
			expected: []string{ipv4},
			protocol: "tcp",
		},
		{
			name:     "udp ipv4",
			ips:      ipv4,
			expected: []string{ipv4},
			protocol: "udp",
		},
		{
			name:     "tcp ipv6",
			ips:      ipv6,
			expected: []string{ipv6},
			protocol: "tcp",
		},
		{
			name:     "udp ipv6",
			ips:      ipv6,
			expected: []string{ipv6},
			protocol: "udp",
		},
		{
			// We should only capture traffic to servers in /etc/resolv.conf nameservers
			// This checks we do not capture traffic to other servers.
			// This is important for cases like app -> istio dns server -> dnsmasq -> upstream
			// If we captured all DNS traffic, we would loop dnsmasq traffic back to our server.
			name:     "tcp localhost server",
			ips:      ipv4,
			expected: []string{},
			protocol: "tcp",
			skipCNI:  true,
			server:   dummyLocalhostServer,
		},
		{
			name:     "udp localhost server",
			ips:      ipv4,
			expected: []string{},
			protocol: "udp",
			skipCNI:  true,
			server:   dummyLocalhostServer,
		},
	}
	for _, client := range flatten(apps.VM, apps.PodA, apps.PodTproxy) {
		for _, tt := range cases {
			if tt.skipCNI && cniEnabled {
				continue
			}
			tt, client := tt, client
			address := "fake.service.local?"
			if tt.protocol != "" {
				address += "&protocol=" + tt.protocol
			}
			if tt.server != "" {
				address += "&server=" + tt.server
			}
			tcases = append(tcases, TrafficTestCase{
				name:   fmt.Sprintf("%s/%s", client.Config().Service, tt.name),
				config: makeSE(tt.ips),
				call:   client.CallWithRetryOrFail,
				opts: echo.CallOptions{
					Scheme:  scheme.DNS,
					Count:   1,
					Address: address,
					Validator: echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								ips := []string{}
								for _, v := range response.RawResponse {
									ips = append(ips, v)
								}
								sort.Strings(ips)
								if !reflect.DeepEqual(ips, tt.expected) {
									return fmt.Errorf("unexpected dns response: wanted %v, got %v", tt.expected, ips)
								}
								return nil
							})
						}),
				},
			})
		}
	}
	svcCases := []struct {
		name     string
		protocol string
		server   string
	}{
		{
			name:     "tcp",
			protocol: "tcp",
		},
		{
			name:     "udp",
			protocol: "udp",
		},
	}
	for _, client := range flatten(apps.VM, apps.PodA, apps.PodTproxy) {
		for _, tt := range svcCases {
			tt, client := tt, client
			aInCluster := apps.PodA.Match(echo.InCluster(client.Config().Cluster))
			if len(aInCluster) == 0 {
				// The cluster doesn't contain A, but connects to a cluster containing A
				aInCluster = apps.PodA.Match(echo.InCluster(client.Config().Cluster.Primary()))
			}
			address := aInCluster[0].Config().FQDN() + "?"
			if tt.protocol != "" {
				address += "&protocol=" + tt.protocol
			}
			if tt.server != "" {
				address += "&server=" + tt.server
			}
			expected := aInCluster[0].Address()
			tcases = append(tcases, TrafficTestCase{
				name: fmt.Sprintf("svc/%s/%s", client.Config().Service, tt.name),
				call: client.CallWithRetryOrFail,
				opts: echo.CallOptions{
					Count:   1,
					Scheme:  scheme.DNS,
					Address: address,
					Validator: echo.ValidatorFunc(
						func(response echoclient.ParsedResponses, _ error) error {
							return response.Check(func(_ int, response *echoclient.ParsedResponse) error {
								ips := []string{}
								for _, v := range response.RawResponse {
									ips = append(ips, v)
								}
								sort.Strings(ips)
								exp := []string{expected}
								if !reflect.DeepEqual(ips, exp) {
									return fmt.Errorf("unexpected dns response: wanted %v, got %v", exp, ips)
								}
								return nil
							})
						}),
				},
			})
		}
	}
	return tcases
}

func VMTestCases(vms echo.Instances, apps *EchoDeployments) []TrafficTestCase {
	var testCases []vmCase

	for _, vm := range vms {
		testCases = append(testCases,
			vmCase{
				name: "dns: VM to k8s cluster IP service name.namespace host",
				from: vm,
				to:   apps.PodA,
				host: PodASvc + "." + apps.Namespace.Name(),
			},
			vmCase{
				name: "dns: VM to k8s cluster IP service fqdn host",
				from: vm,
				to:   apps.PodA,
				host: apps.PodA[0].Config().FQDN(),
			},
			vmCase{
				name: "dns: VM to k8s cluster IP service short name host",
				from: vm,
				to:   apps.PodA,
				host: PodASvc,
			},
			vmCase{
				name: "dns: VM to k8s headless service",
				from: vm,
				to:   apps.Headless.Match(echo.InCluster(vm.Config().Cluster.Primary())),
				host: apps.Headless[0].Config().FQDN(),
			},
			vmCase{
				name: "dns: VM to k8s statefulset service",
				from: vm,
				to:   apps.StatefulSet.Match(echo.InCluster(vm.Config().Cluster.Primary())),
				host: apps.StatefulSet[0].Config().FQDN(),
			},
			// TODO(https://github.com/istio/istio/issues/32552) re-enable
			//vmCase{
			//	name: "dns: VM to k8s statefulset instance.service",
			//	from: vm,
			//	to:   apps.StatefulSet.Match(echo.InCluster(vm.Config().Cluster.Primary())),
			//	host: fmt.Sprintf("%s-v1-0.%s", StatefulSetSvc, StatefulSetSvc),
			//},
			//vmCase{
			//	name: "dns: VM to k8s statefulset instance.service.namespace",
			//	from: vm,
			//	to:   apps.StatefulSet.Match(echo.InCluster(vm.Config().Cluster.Primary())),
			//	host: fmt.Sprintf("%s-v1-0.%s.%s", StatefulSetSvc, StatefulSetSvc, apps.Namespace.Name()),
			//},
			//vmCase{
			//	name: "dns: VM to k8s statefulset instance.service.namespace.svc",
			//	from: vm,
			//	to:   apps.StatefulSet.Match(echo.InCluster(vm.Config().Cluster.Primary())),
			//	host: fmt.Sprintf("%s-v1-0.%s.%s.svc", StatefulSetSvc, StatefulSetSvc, apps.Namespace.Name()),
			//},
			//vmCase{
			//	name: "dns: VM to k8s statefulset instance FQDN",
			//	from: vm,
			//	to:   apps.StatefulSet.Match(echo.InCluster(vm.Config().Cluster.Primary())),
			//	host: fmt.Sprintf("%s-v1-0.%s", StatefulSetSvc, apps.StatefulSet[0].Config().FQDN()),
			//},
		)
	}
	for _, podA := range apps.PodA {
		testCases = append(testCases, vmCase{
			name: "k8s to vm",
			from: podA,
			to:   vms,
		})
	}
	cases := make([]TrafficTestCase, 0)
	for _, c := range testCases {
		c := c
		validators := []echo.Validator{echo.ExpectOK()}
		if !c.to.ContainsMatch(echo.IsHeadless()) {
			// headless load-balancing can be inconsistent
			validators = append(validators, echo.ExpectReachedClusters(c.to.Clusters()))
		}
		cases = append(cases, TrafficTestCase{
			name: fmt.Sprintf("%s from %s", c.name, c.from.Config().Cluster.StableName()),
			call: c.from.CallWithRetryOrFail,
			opts: echo.CallOptions{
				// assume that all echos in `to` only differ in which cluster they're deployed in
				Target:    c.to[0],
				PortName:  "http",
				Address:   c.host,
				Count:     callsPerCluster * len(c.to),
				Validator: echo.And(validators...),
			},
		})
	}
	return cases
}

func destinationRule(app, mode string) string {
	return fmt.Sprintf(`apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: %s
spec:
  host: %s
  trafficPolicy:
    tls:
      mode: %s
---
`, app, app, mode)
}

func useClientProtocolDestinationRule(name, app string) string {
	return fmt.Sprintf(`apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: %s
spec:
  host: %s
  trafficPolicy:
    tls:
      mode: DISABLE
    connectionPool:
      http:
        useClientProtocol: true
---
`, name, app)
}

func idletimeoutDestinationRule(name, app string) string {
	return fmt.Sprintf(`apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: %s
spec:
  host: %s
  trafficPolicy:
    tls:
      mode: DISABLE
    connectionPool:
      http:
        idleTimeout: 100s
---
`, name, app)
}

func peerAuthentication(app, mode string) string {
	return fmt.Sprintf(`apiVersion: security.istio.io/v1beta1
kind: PeerAuthentication
metadata:
  name: %s
spec:
  selector:
    matchLabels:
      app: %s
  mtls:
    mode: %s
---
`, app, app, mode)
}

func globalPeerAuthentication(mode string) string {
	return fmt.Sprintf(`apiVersion: security.istio.io/v1beta1
kind: PeerAuthentication
metadata:
  name: default
spec:
  mtls:
    mode: %s
---
`, mode)
}

func serverFirstTestCases(apps *EchoDeployments) []TrafficTestCase {
	cases := make([]TrafficTestCase, 0)
	clients := apps.PodA
	destination := apps.PodC[0]
	configs := []struct {
		port      string
		dest      string
		auth      string
		validator echo.Validator
	}{
		// TODO: All these cases *should* succeed (except the TLS mismatch cases) - but don't due to issues in our implementation

		// For auto port, outbound request will be delayed by the protocol sniffer, regardless of configuration
		{"auto-tcp-server", "DISABLE", "DISABLE", echo.ExpectError()},
		{"auto-tcp-server", "DISABLE", "PERMISSIVE", echo.ExpectError()},
		{"auto-tcp-server", "DISABLE", "STRICT", echo.ExpectError()},
		{"auto-tcp-server", "ISTIO_MUTUAL", "DISABLE", echo.ExpectError()},
		{"auto-tcp-server", "ISTIO_MUTUAL", "PERMISSIVE", echo.ExpectError()},
		{"auto-tcp-server", "ISTIO_MUTUAL", "STRICT", echo.ExpectError()},

		// These is broken because we will still enable inbound sniffing for the port. Since there is no tls,
		// there is no server-first "upgrading" to client-first
		{"tcp-server", "DISABLE", "DISABLE", echo.ExpectOK()},
		{"tcp-server", "DISABLE", "PERMISSIVE", echo.ExpectError()},

		// Expected to fail, incompatible configuration
		{"tcp-server", "DISABLE", "STRICT", echo.ExpectError()},
		{"tcp-server", "ISTIO_MUTUAL", "DISABLE", echo.ExpectError()},

		// In these cases, we expect success
		// There is no sniffer on either side
		{"tcp-server", "DISABLE", "DISABLE", echo.ExpectOK()},

		// On outbound, we have no sniffer involved
		// On inbound, the request is TLS, so its not server first
		{"tcp-server", "ISTIO_MUTUAL", "PERMISSIVE", echo.ExpectOK()},
		{"tcp-server", "ISTIO_MUTUAL", "STRICT", echo.ExpectOK()},
	}
	for _, client := range clients {
		for _, c := range configs {
			client, c := client, c
			cases = append(cases, TrafficTestCase{
				name:   fmt.Sprintf("%v:%v/%v", c.port, c.dest, c.auth),
				skip:   apps.IsMulticluster(), // TODO stabilize tcp connection breaks
				config: destinationRule(destination.Config().Service, c.dest) + peerAuthentication(destination.Config().Service, c.auth),
				call:   client.CallWithRetryOrFail,
				opts: echo.CallOptions{
					Target:   destination,
					PortName: c.port,
					Scheme:   scheme.TCP,
					// Inbound timeout is 1s. We want to test this does not hit the listener filter timeout
					Timeout:   time.Millisecond * 100,
					Count:     1,
					Validator: c.validator,
				},
			})
		}
	}

	return cases
}
