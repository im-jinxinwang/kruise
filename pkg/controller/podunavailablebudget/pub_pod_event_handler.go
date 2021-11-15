/*
Copyright 2021 The Kruise Authors.

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

package podunavailablebudget

import (
	"context"
	"reflect"
	"time"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	appsv1beta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	"github.com/openkruise/kruise/pkg/control/pubcontrol"
	"github.com/openkruise/kruise/pkg/util"
	"github.com/openkruise/kruise/pkg/util/controllerfinder"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ handler.EventHandler = &enqueueRequestForPod{}

type enqueueRequestForPod struct {
	client           client.Client
	controllerFinder *controllerfinder.ControllerFinder
}

func (p *enqueueRequestForPod) Create(evt event.CreateEvent, q workqueue.RateLimitingInterface) {
	p.addPod(q, evt.Object)
}

func (p *enqueueRequestForPod) Delete(evt event.DeleteEvent, q workqueue.RateLimitingInterface) {
}

func (p *enqueueRequestForPod) Generic(evt event.GenericEvent, q workqueue.RateLimitingInterface) {}

func (p *enqueueRequestForPod) Update(evt event.UpdateEvent, q workqueue.RateLimitingInterface) {
	p.updatePod(q, evt.ObjectOld, evt.ObjectNew)
}

func (p *enqueueRequestForPod) addPod(q workqueue.RateLimitingInterface, obj runtime.Object) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	pub, _ := pubcontrol.GetPodUnavailableBudgetForPod(p.client, p.controllerFinder, pod)
	if pub == nil {
		return
	}

	klog.V(3).Infof("add pod(%s.%s) reconcile pub(%s.%s)", pod.Namespace, pod.Name, pub.Namespace, pub.Name)
	q.Add(reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      pub.Name,
			Namespace: pub.Namespace,
		},
	})
}

func (p *enqueueRequestForPod) updatePod(q workqueue.RateLimitingInterface, old, cur runtime.Object) {
	newPod := cur.(*corev1.Pod)
	oldPod := old.(*corev1.Pod)
	if newPod.ResourceVersion == oldPod.ResourceVersion {
		return
	}

	//labels changed, and reconcile union pubs
	if !reflect.DeepEqual(newPod.Labels, oldPod.Labels) {
		oldPub, _ := pubcontrol.GetPodUnavailableBudgetForPod(p.client, p.controllerFinder, oldPod)
		newPub, _ := pubcontrol.GetPodUnavailableBudgetForPod(p.client, p.controllerFinder, newPod)
		if oldPub != nil && newPub != nil && oldPub.Name == newPub.Name {
			control := pubcontrol.NewPubControl(newPub, p.controllerFinder, p.client)
			if isReconcile, enqueueDelayTime := isPodAvailableChanged(oldPod, newPod, newPub, control); isReconcile {
				q.AddAfter(reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      newPub.Name,
						Namespace: newPub.Namespace,
					},
				}, enqueueDelayTime)
			}
			return
		}
		if oldPub != nil {
			klog.V(3).Infof("pod(%s.%s) labels changed, and reconcile pub(%s.%s)", oldPod.Namespace, oldPod.Name, oldPub.Namespace, oldPub.Name)
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      oldPub.Name,
					Namespace: oldPub.Namespace,
				},
			})
		}
		if newPub != nil {
			klog.V(3).Infof("pod(%s.%s) labels changed, and reconcile pub(%s.%s)", newPod.Namespace, newPod.Name, newPub.Namespace, newPub.Name)
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      newPub.Name,
					Namespace: newPub.Namespace,
				},
			})
		}

		return
	}

	pub, _ := pubcontrol.GetPodUnavailableBudgetForPod(p.client, p.controllerFinder, newPod)
	if pub == nil {
		return
	}
	control := pubcontrol.NewPubControl(pub, p.controllerFinder, p.client)
	if isReconcile, enqueueDelayTime := isPodAvailableChanged(oldPod, newPod, pub, control); isReconcile {
		q.AddAfter(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      pub.Name,
				Namespace: pub.Namespace,
			},
		}, enqueueDelayTime)
	}

}

func isPodAvailableChanged(oldPod, newPod *corev1.Pod, pub *policyv1alpha1.PodUnavailableBudget, control pubcontrol.PubControl) (bool, time.Duration) {
	var enqueueDelayTime time.Duration
	// If the pod's deletion timestamp is set, remove endpoint from ready address.
	if oldPod.DeletionTimestamp.IsZero() && !newPod.DeletionTimestamp.IsZero() {
		enqueueDelayTime = time.Second * 5
		klog.V(3).Infof("pod(%s.%s) DeletionTimestamp changed, and reconcile pub(%s.%s) delayTime(5s)", newPod.Namespace, newPod.Name, pub.Namespace, pub.Name)
		return true, enqueueDelayTime
		// oldPod Deletion is set, then no reconcile
	} else if !oldPod.DeletionTimestamp.IsZero() {
		return false, enqueueDelayTime
	}

	// If the pod's readiness has changed, the associated endpoint address
	// will move from the unready endpoints set to the ready endpoints.
	// So for the purposes of an endpoint, a readiness change on a pod
	// means we have a changed pod.
	oldReady := control.IsPodReady(oldPod) && control.IsPodStateConsistent(oldPod)
	newReady := control.IsPodReady(newPod) && control.IsPodStateConsistent(newPod)
	if oldReady != newReady {
		klog.V(3).Infof("pod(%s.%s) ConsistentAndReady changed(from %v to %v), and reconcile pub(%s.%s)",
			newPod.Namespace, newPod.Name, oldReady, newReady, pub.Namespace, pub.Name)
		return true, enqueueDelayTime
	}

	return false, enqueueDelayTime
}

var _ handler.EventHandler = &SetEnqueueRequestForPUB{}

type SetEnqueueRequestForPUB struct {
	mgr manager.Manager
}

// Create implements EventHandler
func (e *SetEnqueueRequestForPUB) Create(evt event.CreateEvent, q workqueue.RateLimitingInterface) {
	e.addSetRequest(evt.Object, q)
}

// Update implements EventHandler
func (e *SetEnqueueRequestForPUB) Update(evt event.UpdateEvent, q workqueue.RateLimitingInterface) {
	e.addSetRequest(evt.ObjectNew, q)
}

// Delete implements EventHandler
func (e *SetEnqueueRequestForPUB) Delete(evt event.DeleteEvent, q workqueue.RateLimitingInterface) {
}

// Generic implements EventHandler
func (e *SetEnqueueRequestForPUB) Generic(evt event.GenericEvent, q workqueue.RateLimitingInterface) {
}

func (e *SetEnqueueRequestForPUB) addSetRequest(object client.Object, q workqueue.RateLimitingInterface) {
	gvk, _ := apiutil.GVKForObject(object, e.mgr.GetScheme())
	targetRef := &policyv1alpha1.TargetReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
	}
	var namespace string
	var temLabels map[string]string
	switch gvk.Kind {
	// cloneSet
	case controllerfinder.ControllerKruiseKindCS.Kind:
		obj := object.(*appsv1alpha1.CloneSet)
		targetRef.Name, namespace = obj.Name, obj.Namespace
		temLabels = obj.Spec.Template.Labels
	// deployment
	case controllerfinder.ControllerKindDep.Kind:
		obj := object.(*apps.Deployment)
		targetRef.Name, namespace = obj.Name, obj.Namespace
		temLabels = obj.Spec.Template.Labels

	// statefulSet
	case controllerfinder.ControllerKindSS.Kind:
		// kruise advanced statefulSet
		if gvk.Group == controllerfinder.ControllerKruiseKindSS.Group {
			obj := object.(*appsv1beta1.StatefulSet)
			targetRef.Name, namespace = obj.Name, obj.Namespace
			temLabels = obj.Spec.Template.Labels
		} else {
			obj := object.(*apps.StatefulSet)
			targetRef.Name, namespace = obj.Name, obj.Namespace
			temLabels = obj.Spec.Template.Labels
		}
	default:
		return
	}

	// fetch matched pub
	pubList := &policyv1alpha1.PodUnavailableBudgetList{}
	if err := e.mgr.GetClient().List(context.TODO(), pubList, &client.ListOptions{Namespace: namespace}); err != nil {
		klog.Errorf("SetEnqueueRequestForPUB list pub failed: %s", err.Error())
		return
	}
	var matchedPubs []policyv1alpha1.PodUnavailableBudget
	for _, pub := range pubList.Items {
		// if targetReference isn't nil, priority to take effect
		if pub.Spec.TargetReference != nil {
			// belongs the same workload
			if isReferenceEqual(targetRef, pub.Spec.TargetReference) {
				matchedPubs = append(matchedPubs, pub)
			}
		} else {
			// This error is irreversible, so continue
			labelSelector, err := util.GetFastLabelSelector(pub.Spec.Selector)
			if err != nil {
				continue
			}
			// If a PUB with a nil or empty selector creeps in, it should match nothing, not everything.
			if labelSelector.Empty() || !labelSelector.Matches(labels.Set(temLabels)) {
				continue
			}
			matchedPubs = append(matchedPubs, pub)
		}
	}

	for _, pub := range matchedPubs {
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      pub.Name,
				Namespace: pub.Namespace,
			},
		})
		klog.V(3).Infof("workload(%s/%s) replicas changed, and reconcile pub(%s/%s)",
			namespace, targetRef.Name, pub.Namespace, pub.Name)
	}
}

// check APIVersion, Kind, Name
func isReferenceEqual(ref1, ref2 *policyv1alpha1.TargetReference) bool {
	gv1, err := schema.ParseGroupVersion(ref1.APIVersion)
	if err != nil {
		return false
	}
	gv2, err := schema.ParseGroupVersion(ref2.APIVersion)
	if err != nil {
		return false
	}
	return gv1.Group == gv2.Group && ref1.Kind == ref2.Kind && ref1.Name == ref2.Name
}