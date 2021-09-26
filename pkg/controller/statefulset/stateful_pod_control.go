/*
Copyright 2016 The Kubernetes Authors.

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

package statefulset

import (
	"context"
	"fmt"
	"strings"

	apps "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	errorutils "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientset "k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
)

// StatefulPodControlInterface 抽象出 StatefulSet 对于 Pod 的操作
//
// StatefulPodControlInterface defines the interface that StatefulSetController uses to create, update, and delete Pods,
// and to update the Status of a StatefulSet. It follows the design paradigms used for PodControl, but its
// implementation provides for PVC creation, ordered Pod creation, ordered Pod termination, and Pod identity enforcement.
// Like controller.PodControlInterface, it is implemented as an interface to provide for testing fakes.
type StatefulPodControlInterface interface {
	// CreateStatefulPod 新创建一个 Pod，也会创建 PVC
	//
	// CreateStatefulPod create a Pod in a StatefulSet. Any PVCs necessary for the Pod are created prior to creating
	// the Pod. If the returned error is nil the Pod and its PVCs have been created.
	CreateStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error
	// UpdateStatefulPod 更新一个 Pod
	//
	// UpdateStatefulPod Updates a Pod in a StatefulSet. If the Pod already has the correct identity and stable
	// storage this method is a no-op. If the Pod must be mutated to conform to the Set, it is mutated and updated.
	// pod is an in-out parameter, and any updates made to the pod are reflected as mutations to this parameter. If
	// the create is successful, the returned error is nil.
	UpdateStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error
	// DeleteStatefulPod 删除一个 Pod，但是不会删除 PVC
	//
	// DeleteStatefulPod deletes a Pod in a StatefulSet. The pods PVCs are not deleted. If the delete is successful,
	// the returned error is nil.
	DeleteStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error
}

func NewRealStatefulPodControl(
	client clientset.Interface,
	setLister appslisters.StatefulSetLister,
	podLister corelisters.PodLister,
	pvcLister corelisters.PersistentVolumeClaimLister,
	recorder record.EventRecorder,
) StatefulPodControlInterface {
	return &realStatefulPodControl{client, setLister, podLister, pvcLister, recorder}
}

// realStatefulPodControl 实现了 StatefulPodControlInterface
//
// realStatefulPodControl implements StatefulPodControlInterface using a clientset.Interface to communicate with the
// API server. The struct is package private as the internal details are irrelevant to importing packages.
type realStatefulPodControl struct {
	client    clientset.Interface
	setLister appslisters.StatefulSetLister
	podLister corelisters.PodLister
	pvcLister corelisters.PersistentVolumeClaimLister
	recorder  record.EventRecorder
}

// CreateStatefulPod 创建一个 Pod
//
//	1. 创建 PVC
//	2. 创建 Pod
//	3. Record Event
func (spc *realStatefulPodControl) CreateStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error {
	// 创建 Pod 对应所有 PVC
	// 任一 PVC 失败算作失败
	// Create the Pod's PVCs prior to creating the Pod
	if err := spc.createPersistentVolumeClaims(set, pod); err != nil {
		spc.recordPodEvent("create", set, pod, err)
		return err
	}

	// 创建 Pod
	// If we created the PVCs attempt to create the Pod
	_, err := spc.client.CoreV1().Pods(set.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	// sink already exists errors
	if apierrors.IsAlreadyExists(err) {
		return err
	}

	// Record Event
	spc.recordPodEvent("create", set, pod, err)
	return err
}

// UpdateStatefulPod 更新一个 Pod
func (spc *realStatefulPodControl) UpdateStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error {
	attemptedUpdate := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// assume the Pod is consistent
		consistent := true

		// 检查 Pod 是否符合 StatefulSet
		// if the Pod does not conform to its identity, update the identity and dirty the Pod
		if !identityMatches(set, pod) {
			updateIdentity(set, pod)
			consistent = false
		}

		// 检查 Pod PVC 是否符合 StatefulSet
		// if the Pod does not conform to the StatefulSet's storage requirements, update the Pod's PVC's,
		// dirty the Pod, and create any missing PVCs
		if !storageMatches(set, pod) {
			updateStorage(set, pod)
			consistent = false
			if err := spc.createPersistentVolumeClaims(set, pod); err != nil {
				spc.recordPodEvent("update", set, pod, err)
				return err
			}
		}

		// Pod 完美属于 StatefulSet，不需要更新
		// if the Pod is not dirty, do nothing
		if consistent {
			return nil
		}

		// 调用 Update 更新 Pod
		attemptedUpdate = true
		// commit the update, retrying on conflicts
		_, updateErr := spc.client.CoreV1().Pods(set.Namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
		if updateErr == nil {
			return nil
		}

		// Get 一次 Pod，更新入参 pod
		if updated, err := spc.podLister.Pods(set.Namespace).Get(pod.Name); err == nil {
			// make a copy so we don't mutate the shared cache
			pod = updated.DeepCopy()
		} else {
			utilruntime.HandleError(fmt.Errorf("error getting updated Pod %s/%s from lister: %v", set.Namespace, pod.Name, err))
		}

		return updateErr
	})
	if attemptedUpdate {
		spc.recordPodEvent("update", set, pod, err)
	}
	return err
}

// DeleteStatefulPod 删除一个 Pod
func (spc *realStatefulPodControl) DeleteStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error {
	// 调用 Delete 删除
	err := spc.client.CoreV1().Pods(set.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
	spc.recordPodEvent("delete", set, pod, err)
	return err
}

// recordPodEvent records an event for verb applied to a Pod in a StatefulSet. If err is nil the generated event will
// have a reason of v1.EventTypeNormal. If err is not nil the generated event will have a reason of v1.EventTypeWarning.
func (spc *realStatefulPodControl) recordPodEvent(verb string, set *apps.StatefulSet, pod *v1.Pod, err error) {
	if err == nil {
		reason := fmt.Sprintf("Successful%s", strings.Title(verb))
		message := fmt.Sprintf("%s Pod %s in StatefulSet %s successful",
			strings.ToLower(verb), pod.Name, set.Name)
		spc.recorder.Event(set, v1.EventTypeNormal, reason, message)
	} else {
		reason := fmt.Sprintf("Failed%s", strings.Title(verb))
		message := fmt.Sprintf("%s Pod %s in StatefulSet %s failed error: %s",
			strings.ToLower(verb), pod.Name, set.Name, err)
		spc.recorder.Event(set, v1.EventTypeWarning, reason, message)
	}
}

// recordClaimEvent records an event for verb applied to the PersistentVolumeClaim of a Pod in a StatefulSet. If err is
// nil the generated event will have a reason of v1.EventTypeNormal. If err is not nil the generated event will have a
// reason of v1.EventTypeWarning.
func (spc *realStatefulPodControl) recordClaimEvent(verb string, set *apps.StatefulSet, pod *v1.Pod, claim *v1.PersistentVolumeClaim, err error) {
	if err == nil {
		reason := fmt.Sprintf("Successful%s", strings.Title(verb))
		message := fmt.Sprintf("%s Claim %s Pod %s in StatefulSet %s success",
			strings.ToLower(verb), claim.Name, pod.Name, set.Name)
		spc.recorder.Event(set, v1.EventTypeNormal, reason, message)
	} else {
		reason := fmt.Sprintf("Failed%s", strings.Title(verb))
		message := fmt.Sprintf("%s Claim %s for Pod %s in StatefulSet %s failed error: %s",
			strings.ToLower(verb), claim.Name, pod.Name, set.Name, err)
		spc.recorder.Event(set, v1.EventTypeWarning, reason, message)
	}
}

// createPersistentVolumeClaims creates all of the required PersistentVolumeClaims for pod, which must be a member of
// set. If all of the claims for Pod are successfully created, the returned error is nil. If creation fails, this method
// may be called again until no error is returned, indicating the PersistentVolumeClaims for pod are consistent with
// set's Spec.
func (spc *realStatefulPodControl) createPersistentVolumeClaims(set *apps.StatefulSet, pod *v1.Pod) error {
	var errs []error
	// 遍历 Pod 所需要创建的 PVC
	for _, claim := range getPersistentVolumeClaims(set, pod) {
		// 查询是否存在
		pvc, err := spc.pvcLister.PersistentVolumeClaims(claim.Namespace).Get(claim.Name)
		switch {
		case apierrors.IsNotFound(err):
			// 不存在，则创建
			_, err := spc.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(context.TODO(), &claim, metav1.CreateOptions{})
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to create PVC %s: %s", claim.Name, err))
			}
			if err == nil || !apierrors.IsAlreadyExists(err) {
				spc.recordClaimEvent("create", set, pod, &claim, err)
			}
		case err != nil:
			// 失败，record event
			errs = append(errs, fmt.Errorf("failed to retrieve PVC %s: %s", claim.Name, err))
			spc.recordClaimEvent("create", set, pod, &claim, err)
		case err == nil:
			// PVC 删除中，算作错误
			if pvc.DeletionTimestamp != nil {
				errs = append(errs, fmt.Errorf("pvc %s is being deleted", claim.Name))
			}
		}
		// TODO: Check resource requirements and accessmodes, update if necessary
	}
	return errorutils.NewAggregate(errs)
}

var _ StatefulPodControlInterface = &realStatefulPodControl{}
