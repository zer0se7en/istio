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
	"errors"
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tls "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/golang/protobuf/jsonpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/sets"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
)

func (s *DiscoveryServer) StreamDeltas(stream DeltaDiscoveryStream) error {
	if knativeEnv != "" && firstRequest.Load() {
		// How scaling works in knative is the first request is the "loading" request. During
		// loading request, concurrency=1. Once that request is done, concurrency is enabled.
		// However, the XDS stream is long lived, so the first request would block all others. As a
		// result, we should exit the first request immediately; clients will retry.
		firstRequest.Store(false)
		return status.Error(codes.Unavailable, "server warmup not complete; try again")
	}
	// Check if server is ready to accept clients and process new requests.
	// Currently ready means caches have been synced and hence can build
	// clusters correctly. Without this check, InitContext() call below would
	// initialize with empty config, leading to reconnected Envoys loosing
	// configuration. This is an additional safety check inaddition to adding
	// cachesSynced logic to readiness probe to handle cases where kube-proxy
	// ip tables update latencies.
	// See https://github.com/istio/istio/issues/25495.
	if !s.IsServerReady() {
		return errors.New("server is not ready to serve discovery information")
	}

	ctx := stream.Context()
	peerAddr := "0.0.0.0"
	if peerInfo, ok := peer.FromContext(ctx); ok {
		peerAddr = peerInfo.Addr.String()
	}

	ids, err := s.authenticate(ctx)
	if err != nil {
		return err
	}
	if ids != nil {
		log.Debugf("Authenticated XDS: %v with identity %v", peerAddr, ids)
	} else {
		log.Debug("Unauthenticated XDS: ", peerAddr)
	}

	// InitContext returns immediately if the context was already initialized.
	if err = s.globalPushContext().InitContext(s.Env, nil, nil); err != nil {
		// Error accessing the data - log and close, maybe a different pilot replica
		// has more luck
		log.Warnf("Error reading config %v", err)
		return err
	}
	con := newDeltaConnection(peerAddr, stream)
	con.Identities = ids

	// Do not call: defer close(con.pushChannel). The push channel will be garbage collected
	// when the connection is no longer used. Closing the channel can cause subtle race conditions
	// with push. According to the spec: "It's only necessary to close a channel when it is important
	// to tell the receiving goroutines that all data have been sent."

	// Reading from a stream is a blocking operation. Each connection needs to read
	// discovery requests and wait for push commands on config change, so we add a
	// go routine. If go grpc adds gochannel support for streams this will not be needed.
	// This also detects close.
	var receiveError error
	reqChannel := make(chan *discovery.DeltaDiscoveryRequest, 1)
	go s.receiveDelta(con, reqChannel, &receiveError)

	// Wait for the proxy to be fully initialized before we start serving traffic. Because
	// initialization doesn't have dependencies that will block, there is no need to add any timeout
	// here. Prior to this explicit wait, we were implicitly waiting by receive() not sending to
	// reqChannel and the connection not being enqueued for pushes to pushChannel until the
	// initialization is complete.
	<-con.initialized

	for {
		// Block until either a request is received or a push is triggered.
		// We need 2 go routines because 'read' blocks in Recv().
		//
		// To avoid 2 routines, we tried to have Recv() in StreamAggregateResource - and the push
		// on different short-lived go routines started when the push is happening. This would cut in 1/2
		// the number of long-running go routines, since push is throttled. The main problem is with
		// closing - the current gRPC library didn't allow closing the stream.
		select {
		case req, ok := <-reqChannel:
			if !ok {
				// Remote side closed connection or error processing the request.
				return receiveError
			}
			// processRequest is calling pushXXX, accessing common structs with pushConnection.
			// Adding sync is the second issue to be resolved if we want to save 1/2 of the threads.
			log.Debugf("Got Delta Request: %+v", req.TypeUrl)
			err := s.processDeltaRequest(req, con)
			if err != nil {
				return err
			}

		case pushEv := <-con.pushChannel:
			err := s.pushConnectionDelta(con, pushEv)
			pushEv.done()
			if err != nil {
				return err
			}
		case <-con.stop:
			return nil
		}
	}
}

// Compute and send the new configuration for a connection. This is blocking and may be slow
// for large configs. The method will hold a lock on con.pushMutex.
func (s *DiscoveryServer) pushConnectionDelta(con *Connection, pushEv *Event) error {
	pushRequest := pushEv.pushRequest

	if pushRequest.Full {
		// Update Proxy with current information.
		s.updateProxy(con.proxy, pushRequest.Push)
	}

	if !s.ProxyNeedsPush(con.proxy, pushRequest) {
		log.Debugf("Skipping push to %v, no updates required", con.ConID)
		if pushRequest.Full {
			// Only report for full versions, incremental pushes do not have a new version
			reportAllEvents(s.StatusReporter, con.ConID, pushRequest.Push.LedgerVersion, nil)
		}
		return nil
	}

	currentVersion := versionInfo()

	// Send pushes to all generators
	// Each Generator is responsible for determining if the push event requires a push
	for _, w := range getWatchedResources(con.proxy.WatchedResources) {
		if !features.EnableFlowControl {
			// Always send the push if flow control disabled
			if err := s.pushDeltaXds(con, pushRequest.Push, currentVersion, w, nil, pushRequest); err != nil {
				return err
			}
			continue
		}
		// If flow control is enabled, we will only push if we got an ACK for the previous response
		synced, timeout := con.Synced(w.TypeUrl)
		if !synced && timeout {
			// We are not synced, but we have been stuck for too long. We will trigger the push anyways to
			// avoid any scenario where this may deadlock.
			// This can possibly be removed in the future if we find this never causes issues
			totalDelayedPushes.With(typeTag.Value(v3.GetMetricType(w.TypeUrl))).Increment()
			log.Warnf("%s: QUEUE TIMEOUT for node:%s", v3.GetShortType(w.TypeUrl), con.proxy.ID)
		}
		if synced || timeout {
			// Send the push now
			if err := s.pushDeltaXds(con, pushRequest.Push, currentVersion, w, nil, pushRequest); err != nil {
				return err
			}
		} else {
			// The type is not yet synced. Instead of pushing now, which may overload Envoy,
			// we will wait until the last push is ACKed and trigger the push. See
			// https://github.com/istio/istio/issues/25685 for details on the performance
			// impact of sending pushes before Envoy ACKs.
			totalDelayedPushes.With(typeTag.Value(v3.GetMetricType(w.TypeUrl))).Increment()
			log.Debugf("%s: QUEUE for node:%s", v3.GetShortType(w.TypeUrl), con.proxy.ID)
			con.proxy.Lock()
			con.blockedPushes[w.TypeUrl] = con.blockedPushes[w.TypeUrl].Merge(pushEv.pushRequest)
			con.proxy.Unlock()
		}
	}
	if pushRequest.Full {
		// Report all events for unwatched resources. Watched resources will be reported in pushXds or on ack.
		reportAllEvents(s.StatusReporter, con.ConID, pushRequest.Push.LedgerVersion, con.proxy.WatchedResources)
	}

	proxiesConvergeDelay.Record(time.Since(pushRequest.Start).Seconds())
	return nil
}

func (s *DiscoveryServer) receiveDelta(con *Connection, reqChannel chan *discovery.DeltaDiscoveryRequest, errP *error) {
	defer func() {
		close(reqChannel)
		// Close the initialized channel, if its not already closed, to prevent blocking the stream
		select {
		case <-con.initialized:
		default:
			close(con.initialized)
		}
	}()
	firstReq := true
	for {
		req, err := con.deltaStream.Recv()
		if err != nil {
			if isExpectedGRPCError(err) {
				log.Infof("ADS: %q %s terminated %v", con.PeerAddr, con.ConID, err)
				return
			}
			*errP = err
			log.Errorf("ADS: %q %s terminated with error: %v", con.PeerAddr, con.ConID, err)
			totalXDSInternalErrors.Increment()
			return
		}
		// This should be only set for the first request. The node id may not be set - for example malicious clients.
		if firstReq {
			firstReq = false
			if req.Node == nil || req.Node.Id == "" {
				*errP = errors.New("missing node ID")
				return
			}
			// TODO: We should validate that the namespace in the cert matches the claimed namespace in metadata.
			if err := s.initConnection(req.Node, con); err != nil {
				*errP = err
				return
			}
			log.Infof("ADS: new connection for node:%s", con.ConID)
			defer func() {
				s.removeCon(con.ConID)
				if s.StatusGen != nil {
					s.StatusGen.OnDisconnect(con)
				}
				s.WorkloadEntryController.QueueUnregisterWorkload(con.proxy, con.Connect)
			}()
		}

		select {
		case reqChannel <- req:
		case <-con.deltaStream.Context().Done():
			log.Infof("ADS: %q %s terminated with stream closed", con.PeerAddr, con.ConID)
			return
		}
	}
}

func (conn *Connection) sendDelta(res *discovery.DeltaDiscoveryResponse) error {
	errChan := make(chan error, 1)

	// sendTimeout may be modified via environment
	t := time.NewTimer(sendTimeout)
	go func() {
		start := time.Now()
		defer func() { recordSendTime(time.Since(start)) }()
		errChan <- conn.deltaStream.Send(res)
		close(errChan)
	}()

	select {
	case <-t.C:
		log.Infof("Timeout writing %s", conn.ConID)
		xdsResponseWriteTimeouts.Increment()
		return status.Errorf(codes.DeadlineExceeded, "timeout sending")
	case err := <-errChan:
		if err == nil {
			sz := 0
			for _, rc := range res.Resources {
				sz += len(rc.Resource.Value)
			}
			conn.proxy.Lock()
			if res.Nonce != "" {
				if conn.proxy.WatchedResources[res.TypeUrl] == nil {
					conn.proxy.WatchedResources[res.TypeUrl] = &model.WatchedResource{TypeUrl: res.TypeUrl}
				}
				conn.proxy.WatchedResources[res.TypeUrl].NonceSent = res.Nonce
				conn.proxy.WatchedResources[res.TypeUrl].VersionSent = res.SystemVersionInfo
				conn.proxy.WatchedResources[res.TypeUrl].LastSent = time.Now()
				conn.proxy.WatchedResources[res.TypeUrl].LastSize = sz
			}
			conn.proxy.Unlock()
		}
		// To ensure the channel is empty after a call to Stop, check the
		// return value and drain the channel (from Stop docs).
		if !t.Stop() {
			<-t.C
		}
		return err
	}
}

// processRequest is handling one request. This is currently called from the 'main' thread, which also
// handles 'push' requests and close - the code will eventually call the 'push' code, and it needs more mutex
// protection. Original code avoided the mutexes by doing both 'push' and 'process requests' in same thread.
func (s *DiscoveryServer) processDeltaRequest(req *discovery.DeltaDiscoveryRequest, con *Connection) error {
	if !s.preProcessRequest(con.proxy, deltaToSotwRequest(req)) {
		return nil
	}

	if s.StatusReporter != nil {
		s.StatusReporter.RegisterEvent(con.ConID, req.TypeUrl, req.ResponseNonce)
	}
	shouldRespond := s.shouldRespondDelta(con, req)

	// Check if we have a blocked push. If this was an ACK, we will send it. Either way we remove the blocked push
	// as we will send a push.
	con.proxy.Lock()
	request, haveBlockedPush := con.blockedPushes[req.TypeUrl]
	delete(con.blockedPushes, req.TypeUrl)
	con.proxy.Unlock()

	if shouldRespond {
		debugRequest(req)
		// This is a request, trigger a full push for this type
		// Override the blocked push (if it exists), as this full push is guaranteed to be a superset
		// of what we would have pushed from the blocked push.
		request = &model.PushRequest{Full: true}
	} else if !haveBlockedPush {
		// This is an ACK, no delayed push
		// Return immediately, no action needed
		return nil
	} else {
		// we have a blocked push which we will use
		log.Debugf("%s: DEQUEUE for node:%s", v3.GetShortType(req.TypeUrl), con.proxy.ID)
	}

	push := s.globalPushContext()

	return s.pushDeltaXds(con, push, versionInfo(), con.Watched(req.TypeUrl), req.ResourceNamesSubscribe, request)
}

// shouldRespond determines whether this request needs to be responded back. It applies the ack/nack rules as per xds protocol
// using WatchedResource for previous state and discovery request for the current state.
func (s *DiscoveryServer) shouldRespondDelta(con *Connection, request *discovery.DeltaDiscoveryRequest) bool {
	stype := v3.GetShortType(request.TypeUrl)

	// If there is an error in request that means previous response is erroneous.
	// We do not have to respond in that case. In this case request's version info
	// will be different from the version sent. But it is fragile to rely on that.
	if request.ErrorDetail != nil {
		errCode := codes.Code(request.ErrorDetail.Code)
		log.Warnf("dADS:%s: ACK ERROR %s %s:%s", stype, con.ConID, errCode.String(), request.ErrorDetail.GetMessage())
		incrementXDSRejects(request.TypeUrl, con.proxy.ID, errCode.String())
		if s.StatusGen != nil {
			s.StatusGen.OnNack(con.proxy, deltaToSotwRequest(request))
		}
		con.proxy.Lock()
		con.proxy.WatchedResources[request.TypeUrl].NonceNacked = request.ResponseNonce
		con.proxy.Unlock()
		return false
	}

	con.proxy.RLock()
	previousInfo := con.proxy.WatchedResources[request.TypeUrl]
	con.proxy.RUnlock()

	// This is a case of Envoy reconnecting Istiod i.e. Istiod does not have
	// information about this typeUrl, but Envoy sends response nonce - either
	// because Istiod is restarted or Envoy disconnects and reconnects.
	// We should always respond with the current resource names.
	if previousInfo == nil {
		// TODO: can we distinguish init and reconnect? Do we care?
		log.Debugf("dADS:%s: INIT/RECONNECT %s %s", stype, con.ConID, request.ResponseNonce)
		con.proxy.Lock()
		con.proxy.WatchedResources[request.TypeUrl] = &model.WatchedResource{
			TypeUrl:       request.TypeUrl,
			ResourceNames: deltaWatchedResources(nil, request),
			LastRequest:   deltaToSotwRequest(request),
		}
		con.proxy.Unlock()
		return true
	}

	// If there is mismatch in the nonce, that is a case of expired/stale nonce.
	// A nonce becomes stale following a newer nonce being sent to Envoy.
	// TODO: due to concurrent unsubscribe, this probably doesn't make sense. Do we need any logic here?
	if request.ResponseNonce != "" && request.ResponseNonce != previousInfo.NonceSent {
		log.Debugf("dADS:%s: REQ %s Expired nonce received %s, sent %s", stype,
			con.ConID, request.ResponseNonce, previousInfo.NonceSent)
		xdsExpiredNonce.With(typeTag.Value(v3.GetMetricType(request.TypeUrl))).Increment()
		con.proxy.Lock()
		con.proxy.WatchedResources[request.TypeUrl].NonceNacked = ""
		con.proxy.WatchedResources[request.TypeUrl].LastRequest = deltaToSotwRequest(request)
		con.proxy.Unlock()
		return false
	}

	// If it comes here, that means nonce match. This an ACK. We should record
	// the ack details and respond if there is a change in resource names.
	con.proxy.Lock()
	previousResources := con.proxy.WatchedResources[request.TypeUrl].ResourceNames
	con.proxy.WatchedResources[request.TypeUrl].VersionAcked = ""
	con.proxy.WatchedResources[request.TypeUrl].NonceAcked = request.ResponseNonce
	con.proxy.WatchedResources[request.TypeUrl].NonceNacked = ""
	con.proxy.WatchedResources[request.TypeUrl].ResourceNames = deltaWatchedResources(previousResources, request)
	con.proxy.WatchedResources[request.TypeUrl].LastRequest = deltaToSotwRequest(request)
	con.proxy.Unlock()

	oldAck := listEqualUnordered(previousResources, con.proxy.WatchedResources[request.TypeUrl].ResourceNames)
	newAck := request.ResponseNonce != ""
	if newAck != oldAck {
		// Not sure which is better, lets just log if they don't match for now and compare.
		log.Errorf("dADS:%s: New ACK and old ACK check mismatch: %v vs %v", stype, oldAck, newAck)
		if features.EnableUnsafeAssertions {
			panic(fmt.Sprintf("dADS:%s: New ACK and old ACK check mismatch: %v vs %v", stype, oldAck, newAck))
		}
	}
	// Envoy can send two DiscoveryRequests with same version and nonce
	// when it detects a new resource. We should respond if they change.
	if oldAck {
		log.Debugf("dADS:%s: ACK %s %s", stype, con.ConID, request.ResponseNonce)
		return false
	}
	log.Debugf("dADS:%s: RESOURCE CHANGE previous resources: %v, new resources: %v %s %s", stype,
		previousResources, con.proxy.WatchedResources[request.TypeUrl].ResourceNames, con.ConID, request.ResponseNonce)

	return true
}

// Push an XDS resource for the given connection. Configuration will be generated
// based on the passed in generator. Based on the updates field, generators may
// choose to send partial or even no response if there are no changes.
func (s *DiscoveryServer) pushDeltaXds(con *Connection, push *model.PushContext, currentVersion string,
	w *model.WatchedResource, subscribe []string, req *model.PushRequest) error {
	if w == nil {
		return nil
	}
	gen := s.findGenerator(w.TypeUrl, con)
	if gen == nil {
		return nil
	}

	t0 := time.Now()

	res, err := gen.Generate(con.proxy, push, w, req)
	if err != nil || res == nil {
		// If we have nothing to send, report that we got an ACK for this version.
		if s.StatusReporter != nil {
			s.StatusReporter.RegisterEvent(con.ConID, w.TypeUrl, push.LedgerVersion)
		}
		return err
	}
	defer func() { recordPushTime(w.TypeUrl, time.Since(t0)) }()

	deltaResponse := convertResponseToDelta(currentVersion, res)
	originalResponse := deltaResponse
	if subscribe != nil {
		// If subscribe is set, client is requesting specific resources. We should just give it the
		// new resources it needs, rather than the entire set of known resources.
		subres := sets.NewSet(subscribe...)
		filteredResponse := []*discovery.Resource{}
		for _, r := range deltaResponse {
			if subres.Contains(r.Name) {
				filteredResponse = append(filteredResponse, r)
			} else {
				log.Debugf("ADS:%v SKIP %v", v3.GetShortType(w.TypeUrl), r.Name)
			}
		}
		deltaResponse = filteredResponse
	}
	resp := &discovery.DeltaDiscoveryResponse{
		TypeUrl:           w.TypeUrl,
		SystemVersionInfo: currentVersion,
		Nonce:             nonce(push.LedgerVersion),
		Resources:         deltaResponse,
	}
	// We take the set of watched resources and anything not in the response is sent as RemovedResources
	// This is similar to SotW, but done on the server side instead of the client.
	cur := sets.NewSet(w.ResourceNames...)
	cur.Delete(extractNames(originalResponse)...)
	resp.RemovedResources = cur.SortedList()
	if len(resp.RemovedResources) > 0 {
		log.Infof("ADS:%v REMOVE %v", v3.GetShortType(w.TypeUrl), resp.RemovedResources)
	}
	if isWildcardTypeURL(w.TypeUrl) {
		// this is probably a bad idea...
		con.proxy.Lock()
		w.ResourceNames = extractNames(originalResponse)
		con.proxy.Unlock()
	}

	if err := con.sendDelta(resp); err != nil {
		recordSendError(w.TypeUrl, con.ConID, err)
		return err
	}

	// Some types handle logs inside Generate, skip them here
	// TODO because we filter out after the fact, SkipLogTypes report wrong info
	// We should have them return up some metadata that we can transparently log
	if _, f := SkipLogTypes[w.TypeUrl]; !f {
		if log.DebugEnabled() {
			// Add additional information to logs when debug mode enabled
			log.Infof("%s: PUSH for node:%s resources:%d size:%s nonce:%v version:%v",
				v3.GetShortType(w.TypeUrl), con.proxy.ID, len(res), util.ByteCount(ResourceSize(res)), resp.Nonce, resp.SystemVersionInfo)
		} else {
			log.Infof("%s: PUSH for node:%s resources:%d size:%s",
				v3.GetShortType(w.TypeUrl), con.proxy.ID, len(res), util.ByteCount(ResourceSize(res)))
		}
	}
	return nil
}

func newDeltaConnection(peerAddr string, stream DeltaDiscoveryStream) *Connection {
	return &Connection{
		pushChannel:   make(chan *Event),
		initialized:   make(chan struct{}),
		stop:          make(chan struct{}),
		PeerAddr:      peerAddr,
		Connect:       time.Now(),
		deltaStream:   stream,
		blockedPushes: map[string]*model.PushRequest{},
	}
}

// just for experimentation
// TODO: make generator return discovery.Resource; then we don't need to introspect the name
func convertResponseToDelta(ver string, resources model.Resources) []*discovery.Resource {
	convert := []*discovery.Resource{}
	for _, r := range resources {
		var name string
		switch r.TypeUrl {
		case v3.ClusterType:
			aa := &cluster.Cluster{}
			_ = r.UnmarshalTo(aa)
			name = aa.Name
		case v3.ListenerType:
			aa := &listener.Listener{}
			_ = r.UnmarshalTo(aa)
			name = aa.Name
		case v3.EndpointType:
			aa := &endpoint.ClusterLoadAssignment{}
			_ = r.UnmarshalTo(aa)
			name = aa.ClusterName
		case v3.RouteType:
			aa := &route.RouteConfiguration{}
			_ = r.UnmarshalTo(aa)
			name = aa.Name
		case v3.SecretType:
			aa := &tls.Secret{}
			_ = r.UnmarshalTo(aa)
			name = aa.Name
		case v3.ExtensionConfigurationType:
			aa := &core.TypedExtensionConfig{}
			_ = r.UnmarshalTo(aa)
			name = aa.Name
		}
		c := &discovery.Resource{
			Name:     name,
			Version:  ver,
			Resource: r,
		}
		convert = append(convert, c)
	}
	return convert
}

// To satisfy methods that need DiscoveryRequest. Not suitable for real usage
func deltaToSotwRequest(request *discovery.DeltaDiscoveryRequest) *discovery.DiscoveryRequest {
	return &discovery.DiscoveryRequest{
		Node:          request.Node,
		ResourceNames: request.ResourceNamesSubscribe,
		TypeUrl:       request.TypeUrl,
		ResponseNonce: request.ResponseNonce,
		ErrorDetail:   request.ErrorDetail,
	}
}

func deltaWatchedResources(existing []string, request *discovery.DeltaDiscoveryRequest) []string {
	res := sets.NewSet(existing...)
	res.Insert(request.ResourceNamesSubscribe...)
	res.Delete(request.ResourceNamesUnsubscribe...)
	// TODO initial request?
	return res.SortedList()
}

func ConvertDeltaToResponse(response []*discovery.Resource) model.Resources {
	convert := model.Resources{}
	for _, r := range response {
		convert = append(convert, r.Resource)
	}
	return convert
}

func extractNames(res []*discovery.Resource) []string {
	names := []string{}
	for _, r := range res {
		names = append(names, r.Name)
	}
	return names
}

// TODO: remove, just for development
func debugRequest(req *discovery.DeltaDiscoveryRequest) {
	debug, _ := (&jsonpb.Marshaler{Indent: " "}).MarshalToString(&discovery.DeltaDiscoveryRequest{
		TypeUrl:                  req.TypeUrl,
		ResourceNamesSubscribe:   req.ResourceNamesSubscribe,
		ResourceNamesUnsubscribe: req.ResourceNamesUnsubscribe,
		InitialResourceVersions:  req.InitialResourceVersions,
		ResponseNonce:            req.ResponseNonce,
		ErrorDetail:              req.ErrorDetail,
	})
	log.Debugf("delta request: %s", debug)
}
