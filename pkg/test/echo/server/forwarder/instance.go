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

package forwarder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"golang.org/x/sync/semaphore"

	"istio.io/istio/pkg/test/echo/common"
	"istio.io/istio/pkg/test/echo/proto"
)

var _ io.Closer = &Instance{}

const maxConcurrency = 20

// Config for a forwarder Instance.
type Config struct {
	Request *proto.ForwardEchoRequest
	UDS     string
	Dialer  common.Dialer
}

func (c Config) fillInDefaults() Config {
	c.Dialer = c.Dialer.FillInDefaults()
	common.FillInDefaults(c.Request)
	return c
}

// Instance processes a single proto.ForwardEchoRequest, sending individual echo requests to the destination URL.
type Instance struct {
	p           protocol
	url         string
	serverFirst bool
	timeout     time.Duration
	count       int
	qps         int
	header      http.Header
	message     string
	// Method for the request. Only valid for HTTP
	method string
}

// New creates a new forwarder Instance.
func New(cfg Config) (*Instance, error) {
	cfg = cfg.fillInDefaults()

	p, err := newProtocol(cfg)
	if err != nil {
		return nil, err
	}

	return &Instance{
		p:           p,
		url:         cfg.Request.Url,
		serverFirst: cfg.Request.ServerFirst,
		method:      cfg.Request.Method,
		timeout:     common.GetTimeout(cfg.Request),
		count:       common.GetCount(cfg.Request),
		qps:         int(cfg.Request.Qps),
		header:      common.GetHeaders(cfg.Request),
		message:     cfg.Request.Message,
	}, nil
}

// Run the forwarder and collect the responses.
func (i *Instance) Run(ctx context.Context) (*proto.ForwardEchoResponse, error) {
	g := multierror.Group{}
	responsesMu := sync.RWMutex{}
	responses := make([]string, i.count)

	var throttle *time.Ticker

	if i.qps > 0 {
		sleepTime := time.Second / time.Duration(i.qps)
		fwLog.Debugf("Sleeping %v between requests", sleepTime)
		throttle = time.NewTicker(sleepTime)
	}

	// make the timeout apply to the entire set of requests
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	var canceled bool
	defer func() {
		cancel()
		canceled = true
	}()

	sem := semaphore.NewWeighted(maxConcurrency)
	for reqIndex := 0; reqIndex < i.count; reqIndex++ {
		r := request{
			RequestID:   reqIndex,
			URL:         i.url,
			Message:     i.message,
			Header:      i.header,
			Timeout:     i.timeout,
			ServerFirst: i.serverFirst,
			Method:      i.method,
		}

		if throttle != nil {
			<-throttle.C
		}

		if err := sem.Acquire(ctx, 1); err != nil {
			return nil, fmt.Errorf("failed acquiring semaphore: %v", err)
		}
		g.Go(func() error {
			defer sem.Release(1)
			if canceled {
				return fmt.Errorf("request set timed out")
			}
			resp, err := i.p.makeRequest(ctx, &r)
			if err != nil {
				return err
			}
			responsesMu.Lock()
			responses[r.RequestID] = resp
			responsesMu.Unlock()
			return nil
		})
	}

	requestsDone := make(chan *multierror.Error)
	go func() {
		requestsDone <- g.Wait()
	}()

	select {
	case err := <-requestsDone:
		if err != nil {
			return nil, fmt.Errorf("%d/%d requests had errors; first error: %v", err.Len(), i.count, err.Errors[0])
		}
	case <-ctx.Done():
		responsesMu.RLock()
		defer responsesMu.RUnlock()
		c := 0
		for _, res := range responses {
			if res != "" {
				c++
			}
		}
		return nil, fmt.Errorf("request set timed out after %v and only %d/%d requests completed", i.timeout, c, i.count)
	}

	return &proto.ForwardEchoResponse{
		Output: responses,
	}, nil
}

func (i *Instance) Close() error {
	if i != nil && i.p != nil {
		return i.p.Close()
	}
	return nil
}
