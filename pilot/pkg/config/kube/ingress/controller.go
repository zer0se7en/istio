// Copyright 2017 Istio Authors
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

// Package ingress provides a read-only view of Kubernetes ingress resources
// as an ingress rule configuration type store
package ingress

import (
	"errors"
	"reflect"
	"time"

	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/informers/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"istio.io/istio/galley/pkg/config/schema/resource"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/pkg/env"
	"istio.io/pkg/log"

	"istio.io/istio/galley/pkg/config/schema/collection"
	"istio.io/istio/galley/pkg/config/schema/collections"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/queue"
)

// In 1.0, the Gateway is defined in the namespace where the actual controller runs, and needs to be managed by
// user.
// The gateway is named by appending "-istio-autogenerated-k8s-ingress" to the name of the ingress.
//
// Currently the gateway namespace is hardcoded to istio-system (model.IstioIngressNamespace)
//
// VirtualServices are also auto-generated in the model.IstioIngressNamespace.
//
// The sync of Ingress objects to IP is done by status.go
// the 'ingress service' name is used to get the IP of the Service
// If ingress service is empty, it falls back to NodeExternalIP list, selected using the labels.
// This is using 'namespace' of pilot - but seems to be broken (never worked), since it uses Pilot's pod labels
// instead of the ingress labels.

// Follows mesh.IngressControllerMode setting to enable - OFF|STRICT|DEFAULT.
// STRICT requires "kubernetes.io/ingress.class" == mesh.IngressClass
// DEFAULT allows Ingress without explicit class.

// In 1.1:
// - K8S_INGRESS_NS - namespace of the Gateway that will act as ingress.
// - labels of the gateway set to "app=ingressgateway" for node_port, service set to 'ingressgateway' (matching default install)
//   If we need more flexibility - we can add it (but likely we'll deprecate ingress support first)
// -

var (
	schemas = collection.SchemasFor(
		collections.IstioNetworkingV1Alpha3Virtualservices,
		collections.IstioNetworkingV1Alpha3Gateways)

	virtualServiceGvk = collections.IstioNetworkingV1Alpha3Virtualservices.Resource().GroupVersionKind()
	gatewayGvk        = collections.IstioNetworkingV1Alpha3Gateways.Resource().GroupVersionKind()
)

// Control needs RBAC permissions to write to Pods.

type controller struct {
	mesh         *meshconfig.MeshConfig
	domainSuffix string

	client                 kubernetes.Interface
	queue                  queue.Instance
	informer               cache.SharedIndexInformer
	virtualServiceHandlers []func(model.Config, model.Config, model.Event)
}

var (
	// TODO: move to features ( and remove in 1.2 )
	ingressNamespace = env.RegisterStringVar("K8S_INGRESS_NS", "", "").Get()
)

var (
	errUnsupportedOp = errors.New("unsupported operation: the ingress config store is a read-only view")
)

// NewController creates a new Kubernetes controller
func NewController(client kubernetes.Interface, mesh *meshconfig.MeshConfig,
	options kubecontroller.Options) model.ConfigStoreCache {

	// queue requires a time duration for a retry delay after a handler error
	q := queue.NewQueue(1 * time.Second)

	if ingressNamespace == "" {
		ingressNamespace = constants.IstioIngressNamespace
	}

	log.Infof("Ingress controller watching namespaces %q", options.WatchedNamespace)
	informer := v1beta1.NewFilteredIngressInformer(client, options.WatchedNamespace, options.ResyncPeriod, cache.Indexers{}, nil)

	c := &controller{
		mesh:         mesh,
		domainSuffix: options.DomainSuffix,
		client:       client,
		queue:        q,
		informer:     informer,
	}

	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				q.Push(func() error {
					return c.onEvent(obj, model.EventAdd)
				})
			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					q.Push(func() error {
						return c.onEvent(cur, model.EventUpdate)
					})
				}
			},
			DeleteFunc: func(obj interface{}) {
				q.Push(func() error {
					return c.onEvent(obj, model.EventDelete)
				})
			},
		})

	return c
}

func (c *controller) onEvent(obj interface{}, event model.Event) error {
	if !c.informer.HasSynced() {
		return errors.New("waiting till full synchronization")
	}

	ingress, ok := obj.(*extensionsv1beta1.Ingress)
	if !ok || !shouldProcessIngress(c.mesh, ingress) {
		return nil
	}
	log.Infof("ingress event %s for %s/%s", event, ingress.Namespace, ingress.Name)

	// In 1.0, Pilot has a single function, clearCache, which ignores
	// the inputs.
	// In future we may do smarter processing - but first we'll do
	// major refactoring. No need to recompute everything and generate
	// multiple events.

	// TODO: This works well for Add and Delete events, but not so for Update:
	// An updated ingress may also trigger an Add or Delete for one of its constituent sub-rules.
	for _, f := range c.virtualServiceHandlers {
		f(model.Config{}, model.Config{
			ConfigMeta: model.ConfigMeta{
				Type: gatewayGvk.Kind,
			},
		}, event)
	}

	return nil
}

func (c *controller) RegisterEventHandler(kind resource.GroupVersionKind, f func(model.Config, model.Config, model.Event)) {
	switch kind {
	case gatewayGvk:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	}
}

func (c *controller) Version() string {
	panic("implement me")
}

func (c *controller) GetResourceAtVersion(string, string) (resourceVersion string, err error) {
	panic("implement me")
}

func (c *controller) HasSynced() bool {
	return c.informer.HasSynced()
}

func (c *controller) Run(stop <-chan struct{}) {
	go func() {
		cache.WaitForCacheSync(stop, c.HasSynced)
		c.queue.Run(stop)
	}()
	go c.informer.Run(stop)
	<-stop
}

func (c *controller) Schemas() collection.Schemas {
	//TODO: are these two config descriptors right?
	return schemas
}

//TODO: we don't return out of this function now
func (c *controller) Get(typ resource.GroupVersionKind, name, namespace string) *model.Config {
	if typ != collections.IstioNetworkingV1Alpha3Gateways.Resource().GroupVersionKind() &&
		typ != collections.IstioNetworkingV1Alpha3Virtualservices.Resource().GroupVersionKind() {
		return nil
	}

	ingressName, _, _, err := decodeIngressRuleName(name)
	if err != nil {
		return nil
	}

	storeKey := kube.KeyFunc(ingressName, namespace)
	obj, exists, err := c.informer.GetStore().GetByKey(storeKey)
	if err != nil || !exists {
		return nil
	}

	ingress := obj.(*extensionsv1beta1.Ingress)
	if !shouldProcessIngress(c.mesh, ingress) {
		return nil
	}

	return nil
}

func (c *controller) List(typ resource.GroupVersionKind, namespace string) ([]model.Config, error) {
	if typ != collections.IstioNetworkingV1Alpha3Gateways.Resource().GroupVersionKind() &&
		typ != collections.IstioNetworkingV1Alpha3Virtualservices.Resource().GroupVersionKind() {
		return nil, errUnsupportedOp
	}

	out := make([]model.Config, 0)

	ingressByHost := map[string]*model.Config{}

	for _, obj := range c.informer.GetStore().List() {
		ingress := obj.(*extensionsv1beta1.Ingress)
		if namespace != "" && namespace != ingress.Namespace {
			continue
		}

		if !shouldProcessIngress(c.mesh, ingress) {
			continue
		}

		switch typ {
		case virtualServiceGvk:
			ConvertIngressVirtualService(*ingress, c.domainSuffix, ingressByHost)
		case gatewayGvk:
			gateways := ConvertIngressV1alpha3(*ingress, c.domainSuffix)
			out = append(out, gateways)
		}
	}

	if typ == virtualServiceGvk {
		for _, obj := range ingressByHost {
			out = append(out, *obj)
		}
	}

	return out, nil
}

func (c *controller) Create(_ model.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Update(_ model.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Delete(_ resource.GroupVersionKind, _, _ string) error {
	return errUnsupportedOp
}
