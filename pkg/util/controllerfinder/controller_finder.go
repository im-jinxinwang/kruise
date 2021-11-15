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

package controllerfinder

import (
	"context"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	appsv1beta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
)

// ScaleAndSelector is used to return (controller, scale, selector) fields from the
// controller finder functions.
type ScaleAndSelector struct {
	ControllerReference
	// controller.spec.Replicas
	Scale int32
	// controller.spec.Selector
	Selector *metav1.LabelSelector
	// metadata
	Metadata metav1.ObjectMeta
}

type ControllerReference struct {
	// API version of the referent.
	APIVersion string `json:"apiVersion" protobuf:"bytes,5,opt,name=apiVersion"`
	// Kind of the referent.
	Kind string `json:"kind" protobuf:"bytes,1,opt,name=kind"`
	// Name of the referent.
	Name string `json:"name" protobuf:"bytes,3,opt,name=name"`
	// UID of the referent.
	UID types.UID `json:"uid" protobuf:"bytes,4,opt,name=uid,casttype=k8s.io/apimachinery/pkg/types.UID"`
}

// PodControllerFinder is a function type that maps a pod to a list of
// controllers and their scale.
type PodControllerFinder func(ref ControllerReference, namespace string) (*ScaleAndSelector, error)

type ControllerFinder struct {
	client.Client
}

func NewControllerFinder(c client.Client) *ControllerFinder {
	return &ControllerFinder{
		Client: c,
	}
}

func (r *ControllerFinder) GetExpectedScaleForPods(pods []*corev1.Pod) (int32, error) {
	// 1. Find the controller for each pod.  If any pod has 0 controllers,
	// that's an error. With ControllerRef, a pod can only have 1 controller.
	// A mapping from controllers to their scale.
	controllerScale := map[types.UID]int32{}
	for _, pod := range pods {
		ref := metav1.GetControllerOf(pod)
		if ref == nil {
			continue
		}
		// If we already know the scale of the controller there is no need to do anything.
		if _, found := controllerScale[ref.UID]; found {
			continue
		}
		// Check all the supported controllers to find the desired scale.
		workload, err := r.GetScaleAndSelectorForRef(ref.APIVersion, ref.Kind, pod.Namespace, ref.Name, ref.UID)
		if err != nil && !errors.IsNotFound(err) {
			return 0, err
		}
		if workload != nil && workload.Metadata.DeletionTimestamp.IsZero() {
			controllerScale[workload.UID] = workload.Scale
		}
	}

	// 2. Add up all the controllers.
	var expectedCount int32
	for _, count := range controllerScale {
		expectedCount += count
	}

	return expectedCount, nil
}

func (r *ControllerFinder) GetScaleAndSelectorForRef(apiVersion, kind, ns, name string, uid types.UID) (*ScaleAndSelector, error) {
	targetRef := ControllerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        uid,
	}

	for _, finder := range r.Finders() {
		scale, err := finder(targetRef, ns)
		if scale != nil || err != nil {
			return scale, err
		}
	}
	return nil, nil
}

func (r *ControllerFinder) Finders() []PodControllerFinder {
	return []PodControllerFinder{r.getPodReplicationController, r.getPodDeployment, r.getPodReplicaSet,
		r.getPodStatefulSet, r.getPodKruiseCloneSet, r.getPodKruiseStatefulSet}
}

var (
	ControllerKindRS       = apps.SchemeGroupVersion.WithKind("ReplicaSet")
	ControllerKindSS       = apps.SchemeGroupVersion.WithKind("StatefulSet")
	ControllerKindRC       = corev1.SchemeGroupVersion.WithKind("ReplicationController")
	ControllerKindDep      = apps.SchemeGroupVersion.WithKind("Deployment")
	ControllerKruiseKindCS = appsv1alpha1.SchemeGroupVersion.WithKind("CloneSet")
	ControllerKruiseKindSS = appsv1beta1.SchemeGroupVersion.WithKind("StatefulSet")
)

// getPodReplicaSet finds a replicaset which has no matching deployments.
func (r *ControllerFinder) getPodReplicaSet(ref ControllerReference, namespace string) (*ScaleAndSelector, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKindRS.Kind, []string{ControllerKindRS.Group})
	if !ok {
		return nil, nil
	}
	replicaSet, err := r.getReplicaSet(ref, namespace)
	if err != nil {
		return nil, err
	}
	if replicaSet == nil {
		return nil, nil
	}
	controllerRef := metav1.GetControllerOf(replicaSet)
	if controllerRef != nil && controllerRef.Kind == ControllerKindDep.Kind {
		refSs := ControllerReference{
			APIVersion: controllerRef.APIVersion,
			Kind:       controllerRef.Kind,
			Name:       controllerRef.Name,
			UID:        controllerRef.UID,
		}
		return r.getPodDeployment(refSs, namespace)
	}
	return &ScaleAndSelector{
		Scale:    *(replicaSet.Spec.Replicas),
		Selector: replicaSet.Spec.Selector,
		ControllerReference: ControllerReference{
			APIVersion: replicaSet.APIVersion,
			Kind:       replicaSet.Kind,
			Name:       replicaSet.Name,
			UID:        replicaSet.UID,
		},
		Metadata: replicaSet.ObjectMeta,
	}, nil
}

// getPodReplicaSet finds a replicaset which has no matching deployments.
func (r *ControllerFinder) getReplicaSet(ref ControllerReference, namespace string) (*apps.ReplicaSet, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKindRS.Kind, []string{ControllerKindRS.Group})
	if !ok {
		return nil, nil
	}
	replicaSet := &apps.ReplicaSet{}
	err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, replicaSet)
	if err != nil {
		// when error is NotFound, it is ok here.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ref.UID != "" && replicaSet.UID != ref.UID {
		return nil, nil
	}
	return replicaSet, nil
}

// getPodStatefulSet returns the statefulset referenced by the provided controllerRef.
func (r *ControllerFinder) getPodStatefulSet(ref ControllerReference, namespace string) (*ScaleAndSelector, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKindSS.Kind, []string{ControllerKindSS.Group})
	if !ok {
		return nil, nil
	}
	statefulSet := &apps.StatefulSet{}
	err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, statefulSet)
	if err != nil {
		// when error is NotFound, it is ok here.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ref.UID != "" && statefulSet.UID != ref.UID {
		return nil, nil
	}

	return &ScaleAndSelector{
		Scale:    *(statefulSet.Spec.Replicas),
		Selector: statefulSet.Spec.Selector,
		ControllerReference: ControllerReference{
			APIVersion: statefulSet.APIVersion,
			Kind:       statefulSet.Kind,
			Name:       statefulSet.Name,
			UID:        statefulSet.UID,
		},
		Metadata: statefulSet.ObjectMeta,
	}, nil
}

// getPodDeployments finds deployments for any replicasets which are being managed by deployments.
func (r *ControllerFinder) getPodDeployment(ref ControllerReference, namespace string) (*ScaleAndSelector, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKindDep.Kind, []string{ControllerKindDep.Group})
	if !ok {
		return nil, nil
	}
	deployment := &apps.Deployment{}
	err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, deployment)
	if err != nil {
		// when error is NotFound, it is ok here.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ref.UID != "" && deployment.UID != ref.UID {
		return nil, nil
	}
	return &ScaleAndSelector{
		Scale:    *(deployment.Spec.Replicas),
		Selector: deployment.Spec.Selector,
		ControllerReference: ControllerReference{
			APIVersion: deployment.APIVersion,
			Kind:       deployment.Kind,
			Name:       deployment.Name,
			UID:        deployment.UID,
		},
		Metadata: deployment.ObjectMeta,
	}, nil
}

func (r *ControllerFinder) getPodReplicationController(ref ControllerReference, namespace string) (*ScaleAndSelector, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKindRC.Kind, []string{ControllerKindRC.Group})
	if !ok {
		return nil, nil
	}
	rc := &corev1.ReplicationController{}
	err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, rc)
	if err != nil {
		// when error is NotFound, it is ok here.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ref.UID != "" && rc.UID != ref.UID {
		return nil, nil
	}
	return &ScaleAndSelector{
		Scale:    *(rc.Spec.Replicas),
		Selector: &metav1.LabelSelector{MatchLabels: rc.Spec.Selector},
		ControllerReference: ControllerReference{
			APIVersion: rc.APIVersion,
			Kind:       rc.Kind,
			Name:       rc.Name,
			UID:        rc.UID,
		},
		Metadata: rc.ObjectMeta,
	}, nil
}

// getPodStatefulSet returns the kruise cloneSet referenced by the provided controllerRef.
func (r *ControllerFinder) getPodKruiseCloneSet(ref ControllerReference, namespace string) (*ScaleAndSelector, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKruiseKindCS.Kind, []string{ControllerKruiseKindCS.Group})
	if !ok {
		return nil, nil
	}
	cloneSet := &appsv1alpha1.CloneSet{}
	err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, cloneSet)
	if err != nil {
		// when error is NotFound, it is ok here.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ref.UID != "" && cloneSet.UID != ref.UID {
		return nil, nil
	}

	return &ScaleAndSelector{
		Scale:    *(cloneSet.Spec.Replicas),
		Selector: cloneSet.Spec.Selector,
		ControllerReference: ControllerReference{
			APIVersion: cloneSet.APIVersion,
			Kind:       cloneSet.Kind,
			Name:       cloneSet.Name,
			UID:        cloneSet.UID,
		},
		Metadata: cloneSet.ObjectMeta,
	}, nil
}

// getPodStatefulSet returns the kruise statefulset referenced by the provided controllerRef.
func (r *ControllerFinder) getPodKruiseStatefulSet(ref ControllerReference, namespace string) (*ScaleAndSelector, error) {
	// This error is irreversible, so there is no need to return error
	ok, _ := verifyGroupKind(ref, ControllerKruiseKindSS.Kind, []string{ControllerKruiseKindSS.Group})
	if !ok {
		return nil, nil
	}
	ss := &appsv1beta1.StatefulSet{}
	err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, ss)
	if err != nil {
		// when error is NotFound, it is ok here.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ref.UID != "" && ss.UID != ref.UID {
		return nil, nil
	}

	return &ScaleAndSelector{
		Scale:    *(ss.Spec.Replicas),
		Selector: ss.Spec.Selector,
		ControllerReference: ControllerReference{
			APIVersion: ss.APIVersion,
			Kind:       ss.Kind,
			Name:       ss.Name,
			UID:        ss.UID,
		},
		Metadata: ss.ObjectMeta,
	}, nil
}

func verifyGroupKind(ref ControllerReference, expectedKind string, expectedGroups []string) (bool, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false, err
	}

	if ref.Kind != expectedKind {
		return false, nil
	}

	for _, group := range expectedGroups {
		if group == gv.Group {
			return true, nil
		}
	}

	return false, nil
}