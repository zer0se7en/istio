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

package workloadentry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gogo/protobuf/types"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"

	"istio.io/api/meta/v1alpha1"
	"istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/model/status"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/queue"
	istiolog "istio.io/pkg/log"
)

const (
	// TODO use status or another proper API instead of annotations

	// AutoRegistrationGroupAnnotation on a WorkloadEntry stores the associated WorkloadGroup.
	AutoRegistrationGroupAnnotation = "istio.io/autoRegistrationGroup"
	// WorkloadControllerAnnotation on a WorkloadEntry should store the current/last pilot instance connected to the workload for XDS.
	WorkloadControllerAnnotation = "istio.io/workloadController"
	// ConnectedAtAnnotation on a WorkloadEntry stores the time in nanoseconds when the associated workload connected to a Pilot instance.
	ConnectedAtAnnotation = "istio.io/connectedAt"
	// DisconnectedAtAnnotation on a WorkloadEntry stores the time in nanoseconds when the associated workload disconnected from a Pilot instance.
	DisconnectedAtAnnotation = "istio.io/disconnectedAt"

	timeFormat = time.RFC3339Nano
)

type HealthEvent struct {
	// whether or not the agent thought the target was empty
	Healthy bool `json:"healthy,omitempty"`
	// error message propagated
	Message string `json:"err_message,omitempty"`
}

var log = istiolog.RegisterScope("wle", "wle controller debugging", 0)

type Controller struct {
	instanceID string
	// TODO move WorkloadEntry related tasks into their own object and give InternalGen a reference.
	// store should either be k8s (for running pilot) or in-memory (for tests). MCP and other config store implementations
	// do not support writing. We only use it here for reading WorkloadEntry/WorkloadGroup.
	store model.ConfigStoreCache
	// cleanupLimit rate limit's autoregistered WorkloadEntry cleanup calls to k8s
	cleanupLimit *rate.Limiter
	// cleanupQueue delays the cleanup of autoregsitered WorkloadEntries to allow for grace period
	cleanupQueue queue.Delayed
}

func NewController(store model.ConfigStoreCache, instanceID string) *Controller {
	if features.WorkloadEntryAutoRegistration || features.WorkloadEntryHealthChecks {
		return &Controller{
			instanceID:   instanceID,
			store:        store,
			cleanupLimit: rate.NewLimiter(rate.Limit(20), 1),
			cleanupQueue: queue.NewDelayed(),
		}
	}
	return nil
}

func (c *Controller) Run(stop <-chan struct{}) {
	if c == nil {
		return
	}
	if c.store != nil && c.cleanupQueue != nil {
		go c.periodicWorkloadEntryCleanup(stop)
		go c.cleanupQueue.Run(stop)
	}
}

func setConnectMeta(c *config.Config, controller string, conTime time.Time) {
	c.Annotations[WorkloadControllerAnnotation] = controller
	c.Annotations[ConnectedAtAnnotation] = conTime.Format(timeFormat)
}

func (c *Controller) RegisterWorkload(proxy *model.Proxy, conTime time.Time) error {
	if !features.WorkloadEntryAutoRegistration || c == nil {
		return nil
	}
	// check if the WE already exists, update the status
	entryName := autoregisteredWorkloadEntryName(proxy)
	if entryName == "" {
		return nil
	}

	// Try to patch, if it fails then try to create
	_, err := c.store.Patch(gvk.WorkloadEntry, entryName, proxy.Metadata.Namespace, func(cfg config.Config) config.Config {
		setConnectMeta(&cfg, c.instanceID, conTime)
		return cfg
	})
	// TODO return err from Patch through Get
	if err == nil {
		log.Infof("updated auto-registered WorkloadEntry %s/%s", proxy.Metadata.Namespace, entryName)
		return nil
	} else if !errors.IsNotFound(err) && err.Error() != "item not found" {
		log.Errorf("updating auto-registered WorkloadEntry %s/%s: %v", proxy.Metadata.Namespace, entryName, err)
		return fmt.Errorf("updating auto-registered WorkloadEntry %s/%s err: %v", proxy.Metadata.Namespace, entryName, err)
	}

	// No WorkloadEntry, create one using fields from the associated WorkloadGroup
	groupCfg := c.store.Get(gvk.WorkloadGroup, proxy.Metadata.AutoRegisterGroup, proxy.Metadata.Namespace)
	if groupCfg == nil {
		log.Errorf("auto-registration of %v failed: cannot find WorkloadGroup %s/%s",
			proxy.ID, proxy.Metadata.Namespace, proxy.Metadata.AutoRegisterGroup)
		return fmt.Errorf("auto-registration of %v failed: cannot find WorkloadGroup %s/%s",
			proxy.ID, proxy.Metadata.Namespace, proxy.Metadata.AutoRegisterGroup)
	}
	entry := workloadEntryFromGroup(entryName, proxy, groupCfg)
	setConnectMeta(entry, c.instanceID, conTime)
	_, err = c.store.Create(*entry)
	if err != nil {
		log.Errorf("auto-registration of %v failed: error creating WorkloadEntry: %v", proxy.ID, err)
		return fmt.Errorf("auto-registration of %v failed: error creating WorkloadEntry: %v", proxy.ID, err)
	}
	hcMessage := ""
	if _, f := entry.Annotations[status.WorkloadEntryHealthCheckAnnotation]; f {
		hcMessage = " with health checking enabled"
	}
	log.Infof("auto-registered WorkloadEntry %s/%s%s", proxy.Metadata.Namespace, entryName, hcMessage)
	return nil
}

func (c *Controller) QueueUnregisterWorkload(proxy *model.Proxy) {
	if !features.WorkloadEntryAutoRegistration || c == nil {
		return
	}
	// check if the WE already exists, update the status
	entryName := autoregisteredWorkloadEntryName(proxy)
	if entryName == "" {
		return
	}

	// unset controller, set disconnect time
	cfg := c.store.Get(gvk.WorkloadEntry, entryName, proxy.Metadata.Namespace)
	if cfg == nil {
		// we failed to create the workload entry in the first place or it is not propagated
		return
	}

	// The wle has reconnected to another istiod and controlled by it.
	if cfg.Annotations[WorkloadControllerAnnotation] != c.instanceID {
		return
	}
	wle := cfg.DeepCopy()
	delete(wle.Annotations, WorkloadControllerAnnotation)
	wle.Annotations[DisconnectedAtAnnotation] = time.Now().Format(timeFormat)
	// use update instead of patch to prevent race condition
	_, err := c.store.Update(wle)
	if err != nil && !errors.IsConflict(err) {
		log.Warnf("disconnect: failed updating WorkloadEntry %s/%s: %v", proxy.Metadata.Namespace, entryName, err)
		return
	}

	// after grace period, check if the workload ever reconnected
	ns := proxy.Metadata.Namespace
	c.cleanupQueue.PushDelayed(func() error {
		wle := c.store.Get(gvk.WorkloadEntry, entryName, ns)
		if wle == nil {
			return nil
		}
		if shouldCleanupEntry(*wle) {
			c.cleanupEntry(*wle)
		}
		return nil
	}, features.WorkloadEntryCleanupGracePeriod)
}

// UpdateWorkloadEntryHealth updates the associated WorkloadEntries health status
// based on the corresponding health check performed by istio-agent.
func (c *Controller) UpdateWorkloadEntryHealth(proxy *model.Proxy, event HealthEvent) {
	// we assume that the workload entry exists
	// if auto registration does not exist, try looking
	// up in NodeMetadata
	entryName := autoregisteredWorkloadEntryName(proxy)
	if entryName == "" {
		log.Errorf("unable to derive WorkloadEntry for health update for %v", proxy.ID)
		return
	}

	// get previous status
	cfg := c.store.Get(gvk.WorkloadEntry, entryName, proxy.Metadata.Namespace)
	if cfg == nil {
		log.Errorf("config was nil when getting WorkloadEntry %v for %v", entryName, proxy.ID)
		return
	}

	// replace the updated status
	wle := status.UpdateConfigCondition(*cfg, transformHealthEvent(event))
	// update the status
	_, err := c.store.UpdateStatus(wle)
	if err != nil {
		log.Errorf("error while updating WorkloadEntry status: %v for %v", err, proxy.ID)
	}
	log.Debugf("updated health status of %v to %v", proxy.ID, event.Healthy)
}

// periodicWorkloadEntryCleanup checks lists all WorkloadEntry
func (c *Controller) periodicWorkloadEntryCleanup(stopCh <-chan struct{}) {
	if !features.WorkloadEntryAutoRegistration {
		return
	}
	ticker := time.NewTicker(10 * features.WorkloadEntryCleanupGracePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			wles, err := c.store.List(gvk.WorkloadEntry, metav1.NamespaceAll)
			if err != nil {
				log.Warnf("error listing WorkloadEntry for cleanup: %v", err)
				continue
			}
			for _, wle := range wles {
				wle := wle
				if shouldCleanupEntry(wle) {
					c.cleanupQueue.Push(func() error {
						c.cleanupEntry(wle)
						return nil
					})
				}
			}
		case <-stopCh:
			return
		}
	}
}

func shouldCleanupEntry(wle config.Config) bool {
	// don't clean-up if connected or non-autoregistered WorkloadEntries
	if wle.Annotations[AutoRegistrationGroupAnnotation] == "" ||
		wle.Annotations[WorkloadControllerAnnotation] != "" {
		return false
	}

	disconnTime := wle.Annotations[DisconnectedAtAnnotation]
	if disconnTime == "" {
		return false
	}

	disconnAt, err := time.Parse(timeFormat, disconnTime)
	// if we haven't passed the grace period, don't cleanup
	if err == nil && time.Since(disconnAt) < features.WorkloadEntryCleanupGracePeriod {
		return false
	}

	return true
}

func (c *Controller) cleanupEntry(wle config.Config) {
	if err := c.cleanupLimit.Wait(context.TODO()); err != nil {
		log.Errorf("error in WorkloadEntry cleanup rate limiter: %v", err)
		return
	}
	if err := c.store.Delete(gvk.WorkloadEntry, wle.Name, wle.Namespace); err != nil {
		log.Warnf("failed cleaning up auto-registered WorkloadEntry %s/%s: %v", wle.Namespace, wle.Name, err)
		return
	}
	log.Infof("cleaned up auto-registered WorkloadEntry %s/%s", wle.Namespace, wle.Name)
}

func autoregisteredWorkloadEntryName(proxy *model.Proxy) string {
	if proxy.Metadata.AutoRegisterGroup == "" {
		return ""
	}
	if len(proxy.IPAddresses) == 0 {
		log.Errorf("auto-registration of %v failed: missing IP addresses", proxy.ID)
		return ""
	}
	if len(proxy.Metadata.Namespace) == 0 {
		log.Errorf("auto-registration of %v failed: missing namespace", proxy.ID)
		return ""
	}
	p := []string{proxy.Metadata.AutoRegisterGroup, proxy.IPAddresses[0]}
	if proxy.Metadata.Network != "" {
		p = append(p, proxy.Metadata.Network)
	}

	name := strings.Join(p, "-")
	if len(name) > 253 {
		name = name[len(name)-253:]
		log.Warnf("generated WorkloadEntry name is too long, consider making the WorkloadGroup name shorter. Shortening from beginning to: %s", name)
	}
	return name
}

func transformHealthEvent(event HealthEvent) *v1alpha1.IstioCondition {
	cond := &v1alpha1.IstioCondition{
		Type: status.ConditionHealthy,
		// last probe and transition are the same because
		// we only send on transition in the agent
		LastProbeTime:      types.TimestampNow(),
		LastTransitionTime: types.TimestampNow(),
	}
	if event.Healthy {
		cond.Status = status.StatusTrue
		return cond
	}
	cond.Status = status.StatusFalse
	cond.Message = event.Message
	return cond
}

func mergeLabels(labels ...map[string]string) map[string]string {
	if len(labels) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(labels)*len(labels[0]))
	for _, lm := range labels {
		for k, v := range lm {
			out[k] = v
		}
	}
	return out
}

var workloadGroupIsController = true

func workloadEntryFromGroup(name string, proxy *model.Proxy, groupCfg *config.Config) *config.Config {
	group := groupCfg.Spec.(*v1alpha3.WorkloadGroup)
	entry := group.Template.DeepCopy()
	entry.Address = proxy.IPAddresses[0]
	// TODO move labels out of entry
	// node metadata > WorkloadGroup.Metadata > WorkloadGroup.Template
	if group.Metadata != nil && group.Metadata.Labels != nil {
		entry.Labels = mergeLabels(entry.Labels, group.Metadata.Labels)
	}
	if proxy.Metadata != nil && proxy.Metadata.Labels != nil {
		entry.Labels = mergeLabels(entry.Labels, proxy.Metadata.Labels)
	}

	annotations := map[string]string{AutoRegistrationGroupAnnotation: groupCfg.Name}
	if group.Metadata != nil && group.Metadata.Annotations != nil {
		annotations = mergeLabels(annotations, group.Metadata.Annotations)
	}

	if proxy.Metadata.Network != "" {
		entry.Network = proxy.Metadata.Network
	}
	if proxy.Locality != nil {
		entry.Locality = util.LocalityToString(proxy.Locality)
	}
	if proxy.Metadata.ProxyConfig != nil && proxy.Metadata.ProxyConfig.ReadinessProbe != nil {
		annotations[status.WorkloadEntryHealthCheckAnnotation] = "true"
	}
	return &config.Config{
		Meta: config.Meta{
			GroupVersionKind: gvk.WorkloadEntry,
			Name:             name,
			Namespace:        proxy.Metadata.Namespace,
			Labels:           entry.Labels,
			Annotations:      annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: groupCfg.GroupVersionKind.GroupVersion(),
				Kind:       groupCfg.GroupVersionKind.Kind,
				Name:       groupCfg.Name,
				UID:        kubetypes.UID(groupCfg.UID),
				Controller: &workloadGroupIsController,
			}},
		},
		Spec: entry,
		// TODO status fields used for garbage collection
		Status: nil,
	}
}
