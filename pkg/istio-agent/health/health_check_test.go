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

package health

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/atomic"

	"istio.io/api/networking/v1alpha3"
	"istio.io/istio/pkg/test/util/reserveport"
	"istio.io/istio/pkg/test/util/retry"
)

func TestWorkloadHealthChecker_PerformApplicationHealthCheck(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		port := reserveport.NewPortManagerOrFail(t).ReservePortNumberOrFail(t)
		tcpHealthChecker := NewWorkloadHealthChecker(&v1alpha3.ReadinessProbe{
			InitialDelaySeconds: 0,
			TimeoutSeconds:      1,
			PeriodSeconds:       1,
			SuccessThreshold:    1,
			FailureThreshold:    1,
			HealthCheckMethod: &v1alpha3.ReadinessProbe_TcpSocket{
				TcpSocket: &v1alpha3.TCPHealthCheckConfig{
					Host: "localhost",
					Port: uint32(port),
				},
			},
		}, nil)
		// Speed up tests
		tcpHealthChecker.config.CheckFrequency = time.Millisecond

		quitChan := make(chan struct{})

		expectedTCPEvents := [6]*ProbeEvent{
			{Healthy: true},
			{Healthy: false},
			{Healthy: true},
			{Healthy: false},
			{Healthy: true},
			{Healthy: false}}
		tcpHealthStatuses := [6]bool{true, false, true, false, true, false}

		cont := make(chan struct{}, 6)
		// wait for go-ahead for state change
		go func() {
			for i := 0; i < len(tcpHealthStatuses); i++ {
				if tcpHealthStatuses[i] {
					// open port until we get confirmation that
					srv, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
					if err != nil {
						t.Log(err)
						return
					}
					<-cont
					srv.Close()
				} else {
					<-cont
				}
			}
		}()

		eventNum := atomic.NewInt32(0)
		go tcpHealthChecker.PerformApplicationHealthCheck(func(event *ProbeEvent) {
			if eventNum.Load() >= 6 {
				return
			}
			if event.Healthy != expectedTCPEvents[eventNum.Load()].Healthy {
				t.Errorf("%s: got event healthy: %v at idx %v when expected healthy: %v", "tcp", event.Healthy, eventNum.Load(), expectedTCPEvents[eventNum.Load()].Healthy)
			}
			cont <- struct{}{}
			eventNum.Inc()
		}, quitChan)
		retry.UntilSuccessOrFail(t, func() error {
			if int(eventNum.Load()) != len(expectedTCPEvents) {
				return fmt.Errorf("waiting for %v events", len(expectedTCPEvents)-int(eventNum.Load()))
			}
			return nil
		}, retry.Delay(time.Millisecond*10), retry.Timeout(time.Second))
		close(quitChan)
	})
	t.Run("http", func(t *testing.T) {
		httpPath := "/test/health/check"
		port := reserveport.NewPortManagerOrFail(t).ReservePortNumberOrFail(t)
		httpHealthChecker := NewWorkloadHealthChecker(&v1alpha3.ReadinessProbe{
			InitialDelaySeconds: 0,
			TimeoutSeconds:      1,
			PeriodSeconds:       1,
			SuccessThreshold:    1,
			FailureThreshold:    1,
			HealthCheckMethod: &v1alpha3.ReadinessProbe_HttpGet{
				HttpGet: &v1alpha3.HTTPHealthCheckConfig{
					Path:   httpPath,
					Port:   uint32(port),
					Scheme: "http",
					Host:   "127.0.0.1",
				},
			},
		}, nil)
		// Speed up tests
		httpHealthChecker.config.CheckFrequency = time.Millisecond
		quitChan := make(chan struct{})
		expectedHTTPEvents := [4]*ProbeEvent{
			{Healthy: true},
			{Healthy: false},
			{Healthy: true},
			{Healthy: false},
		}
		httpHealthStatuses := [4]bool{true, false, true, false}

		mockListener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			t.Errorf("unable to start mock listener: %v", err)
		}
		httpServerEventCount := 0
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if httpServerEventCount < len(httpHealthStatuses) && httpHealthStatuses[httpServerEventCount] {
				writer.WriteHeader(200)
				writer.Write([]byte("foobar"))
			} else {
				writer.WriteHeader(500)
			}
			httpServerEventCount++
		}))
		srv.Listener.Close()
		srv.Listener = mockListener
		srv.Start()
		defer srv.Close()

		eventNum := atomic.NewInt32(0)
		go httpHealthChecker.PerformApplicationHealthCheck(func(event *ProbeEvent) {
			if event.Healthy != expectedHTTPEvents[eventNum.Load()].Healthy {
				t.Errorf("tcp: got event healthy: %v at idx %v when expected healthy: %v",
					event.Healthy, eventNum.Load(), expectedHTTPEvents[eventNum.Load()].Healthy)
			}
			eventNum.Inc()
		}, quitChan)

		retry.UntilSuccessOrFail(t, func() error {
			if int(eventNum.Load()) != len(expectedHTTPEvents) {
				return fmt.Errorf("waiting for %v events", len(expectedHTTPEvents)-int(eventNum.Load()))
			}
			return nil
		}, retry.Delay(time.Millisecond*10), retry.Timeout(time.Second))
		close(quitChan)
	})
}
