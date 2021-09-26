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
	"reflect"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/history"
	"k8s.io/kubernetes/pkg/features"

	"k8s.io/klog/v2"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = apps.SchemeGroupVersion.WithKind("StatefulSet")

// StatefulSetController 控制 StatefulSet
//
// StatefulSetController controls statefulsets.
type StatefulSetController struct {
	// client interface
	kubeClient clientset.Interface
	// control returns an interface capable of syncing a stateful set.
	// Abstracted out for testing.
	control StatefulSetControlInterface
	// podControl is used for patching pods.
	podControl controller.PodControlInterface
	// podLister is able to list/get pods from a shared informer's store
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod shared informer has synced at least once
	podListerSynced cache.InformerSynced
	// setLister is able to list/get stateful sets from a shared informer's store
	setLister appslisters.StatefulSetLister
	// setListerSynced returns true if the stateful set shared informer has synced at least once
	setListerSynced cache.InformerSynced
	// pvcListerSynced returns true if the pvc shared informer has synced at least once
	pvcListerSynced cache.InformerSynced
	// revListerSynced returns true if the rev shared informer has synced at least once
	revListerSynced cache.InformerSynced
	// StatefulSets that need to be synced.
	queue workqueue.RateLimitingInterface
}

// NewStatefulSetController 创建一个 StatefulSetController
//
// 从其中可以看到其需要操作的资源
//	+ Pod
//	+ StatefulSet
//	+ PVC
//	+ ControllerRevision
//
// NewStatefulSetController creates a new statefulset controller.
func NewStatefulSetController(
	podInformer coreinformers.PodInformer,
	setInformer appsinformers.StatefulSetInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	revInformer appsinformers.ControllerRevisionInformer,
	kubeClient clientset.Interface,
) *StatefulSetController {
	// 创建 Event Recorder
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "statefulset-controller"})

	// 构建 StatefulSetController
	ssc := &StatefulSetController{
		kubeClient: kubeClient,
		control: NewDefaultStatefulSetControl(
			NewRealStatefulPodControl(
				kubeClient,
				setInformer.Lister(),
				podInformer.Lister(),
				pvcInformer.Lister(),
				recorder),
			NewRealStatefulSetStatusUpdater(kubeClient, setInformer.Lister()),
			history.NewHistory(kubeClient, revInformer.Lister()),
			recorder,
		),
		pvcListerSynced: pvcInformer.Informer().HasSynced,
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "statefulset"),
		podControl:      controller.RealPodControl{KubeClient: kubeClient, Recorder: recorder},

		revListerSynced: revInformer.Informer().HasSynced,
	}

	// 注册 Pod 资源对象的 Event Callback
	// NOTE: 所有的 Pod 相关的回调处理中，都是努力得到 Pod 所属的 StatefulSet，然后调用 enqueueStatefulSet()。
	// ** 其思想是，任何有关的资源最终都是应该走唯一的一个协调流程 **
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		// lookup the statefulset and enqueue
		AddFunc: ssc.addPod,
		// lookup current and old statefulset if labels changed
		UpdateFunc: ssc.updatePod,
		// lookup statefulset accounting for deletion tombstones
		DeleteFunc: ssc.deletePod,
	})
	ssc.podLister = podInformer.Lister()
	ssc.podListerSynced = podInformer.Informer().HasSynced

	// 注册 StatefulSet 资源对象的 Event Callback
	// Add/Update/Delete 都走 enqueueStatefulSet() 方法
	setInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: ssc.enqueueStatefulSet,
			UpdateFunc: func(old, cur interface{}) {
				oldPS := old.(*apps.StatefulSet)
				curPS := cur.(*apps.StatefulSet)
				if oldPS.Status.Replicas != curPS.Status.Replicas {
					klog.V(4).Infof("Observed updated replica count for StatefulSet: %v, %d->%d", curPS.Name, oldPS.Status.Replicas, curPS.Status.Replicas)
				}
				ssc.enqueueStatefulSet(cur)
			},
			DeleteFunc: ssc.enqueueStatefulSet,
		},
	)
	ssc.setLister = setInformer.Lister()
	ssc.setListerSynced = setInformer.Informer().HasSynced

	// TODO: Watch volumes
	return ssc
}

// Run 启动 Controller
//
// Run runs the statefulset controller.
func (ssc *StatefulSetController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ssc.queue.ShutDown()

	klog.Infof("Starting stateful set controller")
	defer klog.Infof("Shutting down statefulset controller")

	// 等待需要查询资源的 Informer 缓存同步
	if !cache.WaitForNamedCacheSync("stateful set", stopCh, ssc.podListerSynced, ssc.setListerSynced, ssc.pvcListerSynced, ssc.revListerSynced) {
		return
	}

	// 启动多个 worker
	// 每个 worker 会对 queue 消费并处理
	for i := 0; i < workers; i++ {
		go wait.Until(ssc.worker, time.Second, stopCh)
	}

	<-stopCh
}

// addPod 处理 Pod 资源对象的 Add Event
//
// addPod adds the statefulset for the pod to the sync queue
func (ssc *StatefulSetController) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)

	// 如果 Pod 的 DeletionTimestamp 不为 nil，表明 Pod 已经被请求删除
	// 那么走 Delete Event Callback
	if pod.DeletionTimestamp != nil {
		// on a restart of the controller manager, it's possible a new pod shows up in a state that
		// is already pending deletion. Prevent the pod from being a creation observation.
		ssc.deletePod(pod)
		return
	}

	// 读取 ControllerRef，即读取 `.metadata.ownerReferences` 字段
	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(pod); controllerRef != nil {
		// 解析 Pod 所属的 StatefulSet
		set := ssc.resolveControllerRef(pod.Namespace, controllerRef)
		if set == nil {
			return // 不属于 StatefulSet 管，直接返回
		}
		klog.V(4).Infof("Pod %s created, labels: %+v", pod.Name, pod.Labels)

		// 转化为 Sync StatefulSet
		ssc.enqueueStatefulSet(set)
		return
	}

	// 如果 Pod 没有 Controller 管理，通过 StatefulSet 来查找 Pod
	// Otherwise, it's an orphan. Get a list of all matching controllers and sync
	// them to see if anyone wants to adopt it.
	sets := ssc.getStatefulSetsForPod(pod)
	if len(sets) == 0 {
		return
	}

	// 还能查到对应的 StatefulSet，那么就处理这个 Orphan Pod
	klog.V(4).Infof("Orphan Pod %s created, labels: %+v", pod.Name, pod.Labels)
	for _, set := range sets {
		ssc.enqueueStatefulSet(set)
	}
}

// updatePod 处理 Pod 资源对象的 Update Event
//
// updatePod adds the statefulset for the current and old pods to the sync queue.
func (ssc *StatefulSetController) updatePod(old, cur interface{}) {
	curPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)

	// 通过 `metadata.resourceVersion` 判断 Pod 是否真正 Update
	// 这里过滤了周期性 Resync 调用 Update 的情况（因为 StatefulSet 只需要周期性 Resync StatefulSet 对象，而不需要处理 Pod 对象）
	if curPod.ResourceVersion == oldPod.ResourceVersion {
		// In the event of a re-list we may receive update events for all known pods.
		// Two different versions of the same pod will always have different RVs.
		return
	}

	// 判断 Label 是否变更
	labelChanged := !reflect.DeepEqual(curPod.Labels, oldPod.Labels)

	// 判断 ControllerRef 是否变更
	curControllerRef := metav1.GetControllerOf(curPod)
	oldControllerRef := metav1.GetControllerOf(oldPod)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// 如果 ControllerRef 变更了，那么先进行 Sync 老的 StatefulSet
		// The ControllerRef was changed. Sync the old controller, if any.
		if set := ssc.resolveControllerRef(oldPod.Namespace, oldControllerRef); set != nil {
			ssc.enqueueStatefulSet(set)
		}
	}

	// 能够解析出 ControllerRef，如果是 StatefulSet，那么 Sync
	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		set := ssc.resolveControllerRef(curPod.Namespace, curControllerRef)
		if set == nil {
			return
		}
		klog.V(4).Infof("Pod %s updated, objectMeta %+v -> %+v.", curPod.Name, oldPod.ObjectMeta, curPod.ObjectMeta)
		ssc.enqueueStatefulSet(set)
		// TODO: MinReadySeconds in the Pod will generate an Available condition to be added in
		// the Pod status which in turn will trigger a requeue of the owning replica set thus
		// having its status updated with the newly available replica.
		if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetMinReadySeconds) && !podutil.IsPodReady(oldPod) && podutil.IsPodReady(curPod) && set.Spec.MinReadySeconds > 0 {
			klog.V(2).Infof("StatefulSet %s will be enqueued after %ds for availability check", set.Name, set.Spec.MinReadySeconds)
			// Add a second to avoid milliseconds skew in AddAfter.
			// See https://github.com/kubernetes/kubernetes/issues/39785#issuecomment-279959133 for more info.
			ssc.enqueueSSAfter(set, (time.Duration(set.Spec.MinReadySeconds)*time.Second)+time.Second)
		}
		return
	}

	// 到这里，不能解析出 ControllerRef，那么从 StatefulSet 尝试查找该 Pod
	// Otherwise, it's an orphan. If anything changed, sync matching controllers
	// to see if anyone wants to adopt it now.
	if labelChanged || controllerRefChanged {
		sets := ssc.getStatefulSetsForPod(curPod)
		if len(sets) == 0 {
			return
		}
		klog.V(4).Infof("Orphan Pod %s updated, objectMeta %+v -> %+v.", curPod.Name, oldPod.ObjectMeta, curPod.ObjectMeta)
		for _, set := range sets {
			ssc.enqueueStatefulSet(set)
		}
	}
}

// deletePod 处理 Pod Delete Event
// deletePod enqueues the statefulset for the pod accounting for deletion tombstones.
func (ssc *StatefulSetController) deletePod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)

	// 如果 obj 不是 Pod 类型，那么可能是一个 delete event 丢弃了，然后 relist 后重新触发了。
	// 这种情况下，会以 tombstone object 方式调用回调，因此需要进一步转换判断
	//
	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %+v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a pod %+v", obj))
			return
		}
	}

	// 解析 Controller，转化为 StatefulSet
	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	set := ssc.resolveControllerRef(pod.Namespace, controllerRef)
	if set == nil {
		return
	}
	klog.V(4).Infof("Pod %s/%s deleted through %v.", pod.Namespace, pod.Name, utilruntime.GetCaller())

	// 依旧入队
	ssc.enqueueStatefulSet(set)
}

// getPodsForStatefulSet 获取 StatefulSet 所管理的 Pod
//
// getPodsForStatefulSet returns the Pods that a given StatefulSet should manage.
// It also reconciles ControllerRef by adopting/orphaning.
//
// NOTE: Returned Pods are pointers to objects from the cache.
//       If you need to modify one, you need to copy it first.
func (ssc *StatefulSetController) getPodsForStatefulSet(set *apps.StatefulSet, selector labels.Selector) ([]*v1.Pod, error) {
	// 列出所有 Pod
	// 因为还要判断可能 adopting/orphaning Pod
	// List all pods to include the pods that don't match the selector anymore but
	// has a ControllerRef pointing to this StatefulSet.
	pods, err := ssc.podLister.Pods(set.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	// filter 用于过滤 Pod
	filter := func(pod *v1.Pod) bool {
		// Only claim if it matches our StatefulSet name. Otherwise release/ignore.
		return isMemberOf(set, pod)
	}

	cm := controller.NewPodControllerRefManager(ssc.podControl, set, selector, controllerKind, ssc.canAdoptFunc(set))
	return cm.ClaimPods(pods, filter)
}

// If any adoptions are attempted, we should first recheck for deletion with
// an uncached quorum read sometime after listing Pods/ControllerRevisions (see #42639).
func (ssc *StatefulSetController) canAdoptFunc(set *apps.StatefulSet) func() error {
	return controller.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		fresh, err := ssc.kubeClient.AppsV1().StatefulSets(set.Namespace).Get(context.TODO(), set.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != set.UID {
			return nil, fmt.Errorf("original StatefulSet %v/%v is gone: got uid %v, wanted %v", set.Namespace, set.Name, fresh.UID, set.UID)
		}
		return fresh, nil
	})
}

// adoptOrphanRevisions 尝试处理 StatefulSet 对象对应的 Orphan Pod
// 
// adoptOrphanRevisions adopts any orphaned ControllerRevisions matched by set's Selector.
func (ssc *StatefulSetController) adoptOrphanRevisions(set *apps.StatefulSet) error {
	revisions, err := ssc.control.ListRevisions(set)
	if err != nil {
		return err
	}
	orphanRevisions := make([]*apps.ControllerRevision, 0)
	for i := range revisions {
		if metav1.GetControllerOf(revisions[i]) == nil {
			orphanRevisions = append(orphanRevisions, revisions[i])
		}
	}
	if len(orphanRevisions) > 0 {
		canAdoptErr := ssc.canAdoptFunc(set)()
		if canAdoptErr != nil {
			return fmt.Errorf("can't adopt ControllerRevisions: %v", canAdoptErr)
		}
		return ssc.control.AdoptOrphanRevisions(set, orphanRevisions)
	}
	return nil
}

// getStatefulSetsForPod 通过 StatefulSet 来判断一个 Pod 所属于的 StatefulSet
// getStatefulSetsForPod returns a list of StatefulSets that potentially match
// a given pod.
func (ssc *StatefulSetController) getStatefulSetsForPod(pod *v1.Pod) []*apps.StatefulSet {
	sets, err := ssc.setLister.GetPodStatefulSets(pod)
	if err != nil {
		return nil
	}
	// More than one set is selecting the same Pod
	if len(sets) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than one
		// item in this list nevertheless constitutes user error.
		utilruntime.HandleError(
			fmt.Errorf(
				"user error: more than one StatefulSet is selecting pods with labels: %+v",
				pod.Labels))
	}
	return sets
}

// resolveControllerRef 解析 metav1.OwnerReference，尝试将其变为 StatefulSet
//
// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (ssc *StatefulSetController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *apps.StatefulSet {
	// 对比 Kind
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}

	// 查找对应 Name StatefulSet 是否还存在
	set, err := ssc.setLister.StatefulSets(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}

	// 对比查到的 StatefulSet 还是否是同一个（通过 UID 判断）
	if set.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return set
}

// enqueueStatefulSet 将一个 StatefulSet 对象入队，异步对齐进行协调
//
// enqueueStatefulSet enqueues the given statefulset in the work queue.
func (ssc *StatefulSetController) enqueueStatefulSet(obj interface{}) {
	// 得到 obj 对应的 key
	// NOTE: 正常情况下，key 为 <namespace>/<name> 或者 <name>
	// 对应的，使用 cache.SplitMetaNamespaceKey(key) 可以解析出 namespace 与 name
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	ssc.queue.Add(key)
}

// enqueueStatefulSet enqueues the given statefulset in the work queue after given time
func (ssc *StatefulSetController) enqueueSSAfter(ss *apps.StatefulSet, duration time.Duration) {
	key, err := controller.KeyFunc(ss)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", ss, err))
		return
	}
	ssc.queue.AddAfter(key, duration)
}

// processNextWorkItem 从 queue 出队一个元素，并进行处理
//
// processNextWorkItem dequeues items, processes them, and marks them done. It enforces that the syncHandler is never
// invoked concurrently with the same key.
func (ssc *StatefulSetController) processNextWorkItem() bool {
	// 读取一个 key
	key, quit := ssc.queue.Get()
	if quit {
		return false
	}

	// 必须调用 queue.Done() 出队
	// 执行失败的情况下，会重新入队，因此 defer 里执行出队操作
	defer ssc.queue.Done(key)

	// 执行 sync
	if err := ssc.sync(key.(string)); err != nil {
		utilruntime.HandleError(fmt.Errorf("error syncing StatefulSet %v, requeuing: %v", key.(string), err))
		ssc.queue.AddRateLimited(key) // 执行失败的情况下，重新入队，但是 Rate Limit
	} else {
		ssc.queue.Forget(key) // 执行成功，queue.Forget() 重置 key 对应的限速值
	}
	return true
}

// worker 是每个 goroutine 执行的循环
//
// worker runs a worker goroutine that invokes processNextWorkItem until the controller's queue is closed
func (ssc *StatefulSetController) worker() {
	// 从 queue 中读取 key，并执行 sync
	// 永久循环
	for ssc.processNextWorkItem() {
	}
}

// sync 对指定 key 的 StatefulSet 执行 sync
//
// sync syncs the given statefulset.
func (ssc *StatefulSetController) sync(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing statefulset %q (%v)", key, time.Since(startTime))
	}()

	// 解析 key，得到 namespace 与 name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// 根据 namespace name 得到对应的 StatefulSet
	set, err := ssc.setLister.StatefulSets(namespace).Get(name)
	if errors.IsNotFound(err) {
		klog.Infof("StatefulSet has been deleted %v", key)
		return nil
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to retrieve StatefulSet %v from store: %v", key, err))
		return err
	}

	// 根据 ".spec.selector" 得到 Selector 对象
	// 因此，StatefulSet 的 spec.Selector 决定了其所管理的 Pod
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error converting StatefulSet %v selector: %v", key, err))
		// This is a non-transient error, so don't retry.
		return nil
	}

	if err := ssc.adoptOrphanRevisions(set); err != nil {
		return err
	}

	// 获取 StatefulSet 所管理的所有 Pod
	pods, err := ssc.getPodsForStatefulSet(set, selector)
	if err != nil {
		return err
	}

	// 核心 sync 逻辑
	return ssc.syncStatefulSet(set, pods)
}

// syncStatefulSet sync StatefulSet 与其所有管理的 Pod
//
// syncStatefulSet syncs a tuple of (statefulset, []*v1.Pod).
func (ssc *StatefulSetController) syncStatefulSet(set *apps.StatefulSet, pods []*v1.Pod) error {
	klog.V(4).Infof("Syncing StatefulSet %v/%v with %d pods", set.Namespace, set.Name, len(pods))
	var status *apps.StatefulSetStatus
	var err error
	// TODO: investigate where we mutate the set during the update as it is not obvious.
	status, err = ssc.control.UpdateStatefulSet(set.DeepCopy(), pods)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Successfully synced StatefulSet %s/%s successful", set.Namespace, set.Name)
	// One more sync to handle the clock skew. This is also helping in requeuing right after status update
	if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetMinReadySeconds) && set.Spec.MinReadySeconds > 0 && status != nil && status.AvailableReplicas != *set.Spec.Replicas {
		ssc.enqueueSSAfter(set, time.Duration(set.Spec.MinReadySeconds)*time.Second)
	}

	return nil
}
