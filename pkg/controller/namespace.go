/*
Copyright 2021 The cert-manager Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	IstioConfigLabelKey = "istio.io/config"
)

type caGetter func() []byte

// CARoot manages reconciles a configmap in each namespace with the root CA
// data
type CARoot struct {
	log logr.Logger
}

type namespace struct {
	log    logr.Logger
	client client.Client
	*enforcer
}

type configmap struct {
	log    logr.Logger
	client client.Client
	*enforcer
}

type enforcer struct {
	client        client.Client
	rootCA        caGetter
	configMapName string
}

func AddCARootController(log logr.Logger,
	mgr manager.Manager,
	rootCA caGetter,
	configMapName string,
) error {
	log = log.WithName("ca-root-controller")

	enforcer := &enforcer{
		client:        mgr.GetClient(),
		rootCA:        rootCA,
		configMapName: configMapName,
	}

	namespace := &namespace{
		log:      log,
		client:   mgr.GetClient(),
		enforcer: enforcer,
	}
	configmap := &configmap{
		log:      log,
		client:   mgr.GetClient(),
		enforcer: enforcer,
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(new(corev1.Namespace)).
		Complete(namespace); err != nil {
		return fmt.Errorf("failed to create namespace controller: %s", err)
	}

	// Only reconcile config maps that match the well known name
	if err := ctrl.NewControllerManagedBy(mgr).
		For(new(corev1.ConfigMap)).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			if obj.GetName() != configMapName {
				return false
			}
			return true
		})).
		Complete(configmap); err != nil {
		return fmt.Errorf("failed to create configmap controller: %s", err)
	}

	return nil
}

// Reconcile is called when a ConfigMap event occurs where the resource has the
// well known name in the target Kubernetes cluster. Reconcile will ensure that
// the ConfigMap exists, and the CA root bundle is present.
func (c *configmap) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if err := c.configmap(ctx, c.log, req.NamespacedName.Namespace); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// Reconcile is called when any Namespace event occurs in the target Kubernetes
// cluster. If the resource exists, Reconcile will ensure that the ConfigMap
// exists, CA root bundle is present.
func (n *namespace) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := n.log.WithValues("namespace", req.NamespacedName.Namespace)
	ns := new(corev1.Namespace)

	// Attempt to get the synced Namespace. If the resource no longer
	// exists, we can ignore it.
	err := n.client.Get(ctx, req.NamespacedName, ns)
	if apierrors.IsNotFound(err) {
		log.V(2).Info("namespace doesn't exist, ignoring")
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get %q: %s", req.NamespacedName, err)
	}

	// If the namespace is terminating, we should reconcile configmap
	if ns.Status.Phase == corev1.NamespaceTerminating {
		log.V(2).Info("namespace is terminating, ignoring")
		return ctrl.Result{}, nil
	}

	if err := n.configmap(ctx, log, req.Name); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// configmap will ensure that the provided namespace has the correct ConfigMap,
// with the correct CA and label.
func (e *enforcer) configmap(ctx context.Context, log logr.Logger, namespace string) error {
	var (
		namespacedName = types.NamespacedName{
			Name:      e.configMapName,
			Namespace: namespace,
		}
		cm = new(corev1.ConfigMap)
	)

	rootCA := fmt.Sprintf("%s", e.rootCA())

	// Build the data which should be present in the well-known configmap in
	// all namespaces.
	rootCAConfigData := map[string]string{
		"root-cert.pem": rootCA,
	}

	log = log.WithValues("configmap", namespacedName.String())
	err := e.client.Get(ctx, namespacedName, cm)
	if apierrors.IsNotFound(err) {
		log.V(3).Info("configmap doesn't exist, creating")

		return e.client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      e.configMapName,
				Namespace: namespace,
				Labels: map[string]string{
					IstioConfigLabelKey: "true",
				},
			},
			Data: rootCAConfigData,
		})
	}

	if err != nil {
		return fmt.Errorf("failed to get %q: %s", namespacedName, err)
	}

	var notMatch bool
	if data, ok := cm.Data["root-cert.pem"]; !ok || data != rootCA {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}

		cm.Data["root-cert.pem"] = rootCA
		notMatch = true
	}

	if val, ok := cm.Labels[IstioConfigLabelKey]; !ok || val != "true" {
		notMatch = true
	}

	if notMatch {
		if cm.Labels == nil {
			cm.Labels = make(map[string]string)
		}

		cm.Labels[IstioConfigLabelKey] = "true"

		log.V(3).Info("updating configmap")
		if err := e.client.Update(ctx, cm); err != nil {
			return err
		}
	}

	return nil
}
