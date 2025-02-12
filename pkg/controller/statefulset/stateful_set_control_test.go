/*
Copyright 2019 The Kruise Authors.
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
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/intstr"

	appsv1alpha1 "github.com/openkruise/kruise/pkg/apis/apps/v1alpha1"
	kruiseclientset "github.com/openkruise/kruise/pkg/client/clientset/versioned"
	kruisefake "github.com/openkruise/kruise/pkg/client/clientset/versioned/fake"
	kruiseinformers "github.com/openkruise/kruise/pkg/client/informers/externalversions"
	kruiseappsinformers "github.com/openkruise/kruise/pkg/client/informers/externalversions/apps/v1alpha1"
	kruiseappslisters "github.com/openkruise/kruise/pkg/client/listers/apps/v1alpha1"
	apps "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/history"
)

type invariantFunc func(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error

func setupController(client clientset.Interface, kruiseClient kruiseclientset.Interface) (*fakeStatefulPodControl, *fakeStatefulSetStatusUpdater, StatefulSetControlInterface, chan struct{}) {
	informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
	kruiseInformerFactory := kruiseinformers.NewSharedInformerFactory(kruiseClient, controller.NoResyncPeriodFunc())
	spc := newFakeStatefulPodControl(informerFactory.Core().V1().Pods(), kruiseInformerFactory.Apps().V1alpha1().StatefulSets())
	ssu := newFakeStatefulSetStatusUpdater(kruiseInformerFactory.Apps().V1alpha1().StatefulSets())
	recorder := record.NewFakeRecorder(10)
	ssc := NewDefaultStatefulSetControl(spc, ssu, history.NewFakeHistory(informerFactory.Apps().V1().ControllerRevisions()), recorder)

	stop := make(chan struct{})
	informerFactory.Start(stop)
	kruiseInformerFactory.Start(stop)
	cache.WaitForCacheSync(
		stop,
		kruiseInformerFactory.Apps().V1alpha1().StatefulSets().Informer().HasSynced,
		//informerFactory.Apps().V1().StatefulSets().Informer().HasSynced,
		informerFactory.Core().V1().Pods().Informer().HasSynced,
		informerFactory.Apps().V1().ControllerRevisions().Informer().HasSynced,
	)
	return spc, ssu, ssc, stop
}

func burst(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
	set.Spec.PodManagementPolicy = apps.ParallelPodManagement
	return set
}

func TestStatefulSetControl(t *testing.T) {
	simpleSetFn := func() *appsv1alpha1.StatefulSet { return newStatefulSet(3) }
	largeSetFn := func() *appsv1alpha1.StatefulSet { return newStatefulSet(5) }

	testCases := []struct {
		fn  func(*testing.T, *appsv1alpha1.StatefulSet, invariantFunc)
		obj func() *appsv1alpha1.StatefulSet
	}{
		{CreatesPods, simpleSetFn},
		{ScalesUp, simpleSetFn},
		{ScalesDown, simpleSetFn},
		{ReplacesPods, largeSetFn},
		{RecreatesFailedPod, simpleSetFn},
		{CreatePodFailure, simpleSetFn},
		{UpdatePodFailure, simpleSetFn},
		{UpdateSetStatusFailure, simpleSetFn},
		{PodRecreateDeleteFailure, simpleSetFn},
	}

	for _, testCase := range testCases {
		fnName := runtime.FuncForPC(reflect.ValueOf(testCase.fn).Pointer()).Name()
		if i := strings.LastIndex(fnName, "."); i != -1 {
			fnName = fnName[i+1:]
		}
		t.Run(
			fmt.Sprintf("%s/Monotonic", fnName),
			func(t *testing.T) {
				testCase.fn(t, testCase.obj(), assertMonotonicInvariants)
			},
		)
		t.Run(
			fmt.Sprintf("%s/Burst", fnName),
			func(t *testing.T) {
				set := burst(testCase.obj())
				testCase.fn(t, set, assertBurstInvariants)
			},
		)
	}
}

func CreatesPods(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 3 {
		t.Error("Failed to scale statefulset to 3 replicas")
	}
	if set.Status.ReadyReplicas != 3 {
		t.Error("Failed to set ReadyReplicas correctly")
	}
	if set.Status.UpdatedReplicas != 3 {
		t.Error("Failed to set UpdatedReplicas correctly")
	}
}

func ScalesUp(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	*set.Spec.Replicas = 4
	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to scale StatefulSet : %s", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 4 {
		t.Error("Failed to scale statefulset to 4 replicas")
	}
	if set.Status.ReadyReplicas != 4 {
		t.Error("Failed to set readyReplicas correctly")
	}
	if set.Status.UpdatedReplicas != 4 {
		t.Error("Failed to set updatedReplicas correctly")
	}
}

func ScalesDown(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	*set.Spec.Replicas = 0
	if err := scaleDownStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to scale StatefulSet : %s", err)
	}
	if set.Status.Replicas != 0 {
		t.Error("Failed to scale statefulset to 0 replicas")
	}
	if set.Status.ReadyReplicas != 0 {
		t.Error("Failed to set readyReplicas correctly")
	}
	if set.Status.UpdatedReplicas != 0 {
		t.Error("Failed to set updatedReplicas correctly")
	}
}

func ReplacesPods(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 5 {
		t.Error("Failed to scale statefulset to 5 replicas")
	}
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		t.Error(err)
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}

	sort.Sort(ascendingOrdinal(pods))
	spc.podsIndexer.Delete(pods[0])
	spc.podsIndexer.Delete(pods[2])
	spc.podsIndexer.Delete(pods[4])
	for i := 0; i < 5; i += 2 {
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Error(err)
		}
		if err = ssc.UpdateStatefulSet(set, pods); err != nil {
			t.Errorf("Failed to update StatefulSet : %s", err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("Error getting updated StatefulSet: %v", err)
		}
		if pods, err = spc.setPodRunning(set, i); err != nil {
			t.Error(err)
		}
		if err = ssc.UpdateStatefulSet(set, pods); err != nil {
			t.Errorf("Failed to update StatefulSet : %s", err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("Error getting updated StatefulSet: %v", err)
		}
		if pods, err = spc.setPodReady(set, i); err != nil {
			t.Error(err)
		}
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	if err := ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Errorf("Failed to update StatefulSet : %s", err)
	}
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if e, a := int32(5), set.Status.Replicas; e != a {
		t.Errorf("Expected to scale to %d, got %d", e, a)
	}
}

func RecreatesFailedPod(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset()
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		t.Error(err)
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	if err := ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Errorf("Error updating StatefulSet %s", err)
	}
	if err := invariants(set, spc); err != nil {
		t.Error(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	pods[0].Status.Phase = v1.PodFailed
	spc.podsIndexer.Update(pods[0])
	if err := ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Errorf("Error updating StatefulSet %s", err)
	}
	if err := invariants(set, spc); err != nil {
		t.Error(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	if isCreated(pods[0]) {
		t.Error("StatefulSet did not recreate failed Pod")
	}
}

func CreatePodFailure(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)
	spc.SetCreateStatefulPodError(apierrors.NewInternalError(errors.New("API server failed")), 2)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); !apierrors.IsInternalError(err) {
		t.Errorf("StatefulSetControl did not return InternalError found %s", err)
	}
	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 3 {
		t.Error("Failed to scale StatefulSet to 3 replicas")
	}
	if set.Status.ReadyReplicas != 3 {
		t.Error("Failed to set readyReplicas correctly")
	}
	if set.Status.UpdatedReplicas != 3 {
		t.Error("Failed to updatedReplicas correctly")
	}
}

func UpdatePodFailure(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)
	spc.SetUpdateStatefulPodError(apierrors.NewInternalError(errors.New("API server failed")), 0)

	// have to have 1 successful loop first
	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 3 {
		t.Error("Failed to scale StatefulSet to 3 replicas")
	}
	if set.Status.ReadyReplicas != 3 {
		t.Error("Failed to set readyReplicas correctly")
	}
	if set.Status.UpdatedReplicas != 3 {
		t.Error("Failed to set updatedReplicas correctly")
	}

	// now mutate a pod's identity
	pods, err := spc.podsLister.List(labels.Everything())
	if err != nil {
		t.Fatalf("Error listing pods: %v", err)
	}
	if len(pods) != 3 {
		t.Fatalf("Expected 3 pods, got %d", len(pods))
	}
	sort.Sort(ascendingOrdinal(pods))
	pods[0].Name = "goo-0"
	spc.podsIndexer.Update(pods[0])

	// now it should fail
	if err := ssc.UpdateStatefulSet(set, pods); !apierrors.IsInternalError(err) {
		t.Errorf("StatefulSetControl did not return InternalError found %s", err)
	}
}

func UpdateSetStatusFailure(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, ssu, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)
	ssu.SetUpdateStatefulSetStatusError(apierrors.NewInternalError(errors.New("API server failed")), 2)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); !apierrors.IsInternalError(err) {
		t.Errorf("StatefulSetControl did not return InternalError found %s", err)
	}
	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 3 {
		t.Error("Failed to scale StatefulSet to 3 replicas")
	}
	if set.Status.ReadyReplicas != 3 {
		t.Error("Failed to set readyReplicas to 3")
	}
	if set.Status.UpdatedReplicas != 3 {
		t.Error("Failed to set updatedReplicas to 3")
	}
}

func PodRecreateDeleteFailure(t *testing.T, set *appsv1alpha1.StatefulSet, invariants invariantFunc) {
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)

	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		t.Error(err)
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	if err := ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Errorf("Error updating StatefulSet %s", err)
	}
	if err := invariants(set, spc); err != nil {
		t.Error(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	pods[0].Status.Phase = v1.PodFailed
	spc.podsIndexer.Update(pods[0])
	spc.SetDeleteStatefulPodError(apierrors.NewInternalError(errors.New("API server failed")), 0)
	if err := ssc.UpdateStatefulSet(set, pods); !apierrors.IsInternalError(err) {
		t.Errorf("StatefulSet failed to %s", err)
	}
	if err := invariants(set, spc); err != nil {
		t.Error(err)
	}
	if err := ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Errorf("Error updating StatefulSet %s", err)
	}
	if err := invariants(set, spc); err != nil {
		t.Error(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Error(err)
	}
	if isCreated(pods[0]) {
		t.Error("StatefulSet did not recreate failed Pod")
	}
}

func TestStatefulSetControlScaleDownDeleteError(t *testing.T) {
	invariants := assertMonotonicInvariants
	set := newStatefulSet(3)
	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)

	if err := scaleUpStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn up StatefulSet : %s", err)
	}
	var err error
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	*set.Spec.Replicas = 0
	spc.SetDeleteStatefulPodError(apierrors.NewInternalError(errors.New("API server failed")), 2)
	if err := scaleDownStatefulSetControl(set, ssc, spc, invariants); !apierrors.IsInternalError(err) {
		t.Errorf("StatefulSetControl failed to throw error on delete %s", err)
	}
	if err := scaleDownStatefulSetControl(set, ssc, spc, invariants); err != nil {
		t.Errorf("Failed to turn down StatefulSet %s", err)
	}
	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatalf("Error getting updated StatefulSet: %v", err)
	}
	if set.Status.Replicas != 0 {
		t.Error("Failed to scale statefulset to 0 replicas")
	}
	if set.Status.ReadyReplicas != 0 {
		t.Error("Failed to set readyReplicas to 0")
	}
	if set.Status.UpdatedReplicas != 0 {
		t.Error("Failed to set updatedReplicas to 0")
	}
}

func TestStatefulSetControl_getSetRevisions(t *testing.T) {
	type testcase struct {
		name            string
		existing        []*apps.ControllerRevision
		set             *appsv1alpha1.StatefulSet
		expectedCount   int
		expectedCurrent *apps.ControllerRevision
		expectedUpdate  *apps.ControllerRevision
		err             bool
	}

	testFn := func(test *testcase, t *testing.T) {
		client := fake.NewSimpleClientset()
		kruiseClient := kruisefake.NewSimpleClientset()
		informerFactory := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
		kruiseInformerFactory := kruiseinformers.NewSharedInformerFactory(kruiseClient, controller.NoResyncPeriodFunc())
		spc := newFakeStatefulPodControl(informerFactory.Core().V1().Pods(), kruiseInformerFactory.Apps().V1alpha1().StatefulSets())
		ssu := newFakeStatefulSetStatusUpdater(kruiseInformerFactory.Apps().V1alpha1().StatefulSets())
		recorder := record.NewFakeRecorder(10)
		ssc := defaultStatefulSetControl{spc, ssu, history.NewFakeHistory(informerFactory.Apps().V1().ControllerRevisions()), recorder}

		stop := make(chan struct{})
		defer close(stop)
		informerFactory.Start(stop)
		kruiseInformerFactory.Start(stop)
		cache.WaitForCacheSync(
			stop,
			kruiseInformerFactory.Apps().V1alpha1().StatefulSets().Informer().HasSynced,
			//informerFactory.Apps().V1().StatefulSets().Informer().HasSynced,
			informerFactory.Core().V1().Pods().Informer().HasSynced,
			informerFactory.Apps().V1().ControllerRevisions().Informer().HasSynced,
		)
		test.set.Status.CollisionCount = new(int32)
		for i := range test.existing {
			ssc.controllerHistory.CreateControllerRevision(test.set, test.existing[i], test.set.Status.CollisionCount)
		}
		revisions, err := ssc.ListRevisions(test.set)
		if err != nil {
			t.Fatal(err)
		}
		current, update, _, err := ssc.getStatefulSetRevisions(test.set, revisions)
		if err != nil {
			t.Fatalf("error getting statefulset revisions:%v", err)
		}
		revisions, err = ssc.ListRevisions(test.set)
		if err != nil {
			t.Fatal(err)
		}
		if len(revisions) != test.expectedCount {
			t.Errorf("%s: want %d revisions got %d", test.name, test.expectedCount, len(revisions))
		}
		if test.err && err == nil {
			t.Errorf("%s: expected error", test.name)
		}
		if !test.err && !history.EqualRevision(current, test.expectedCurrent) {
			t.Errorf("%s: for current want %v got %v", test.name, test.expectedCurrent, current)
		}
		if !test.err && !history.EqualRevision(update, test.expectedUpdate) {
			t.Errorf("%s: for update want %v got %v", test.name, test.expectedUpdate, update)
		}
		if !test.err && test.expectedCurrent != nil && current != nil && test.expectedCurrent.Revision != current.Revision {
			t.Errorf("%s: for current revision want %d got %d", test.name, test.expectedCurrent.Revision, current.Revision)
		}
		if !test.err && test.expectedUpdate != nil && update != nil && test.expectedUpdate.Revision != update.Revision {
			t.Errorf("%s: for update revision want %d got %d", test.name, test.expectedUpdate.Revision, update.Revision)
		}
	}

	updateRevision := func(cr *apps.ControllerRevision, revision int64) *apps.ControllerRevision {
		clone := cr.DeepCopy()
		clone.Revision = revision
		return clone
	}

	set := newStatefulSet(3)
	set.Status.CollisionCount = new(int32)
	rev0 := newRevisionOrDie(set, 1)
	set1 := set.DeepCopy()
	set1.Spec.Template.Spec.Containers[0].Image = "foo"
	set1.Status.CurrentRevision = rev0.Name
	set1.Status.CollisionCount = new(int32)
	rev1 := newRevisionOrDie(set1, 2)
	set2 := set1.DeepCopy()
	set2.Spec.Template.Labels["new"] = "label"
	set2.Status.CurrentRevision = rev0.Name
	set2.Status.CollisionCount = new(int32)
	rev2 := newRevisionOrDie(set2, 3)
	tests := []testcase{
		{
			name:            "creates initial revision",
			existing:        nil,
			set:             set,
			expectedCount:   1,
			expectedCurrent: rev0,
			expectedUpdate:  rev0,
			err:             false,
		},
		{
			name:            "creates revision on update",
			existing:        []*apps.ControllerRevision{rev0},
			set:             set1,
			expectedCount:   2,
			expectedCurrent: rev0,
			expectedUpdate:  rev1,
			err:             false,
		},
		{
			name:            "must not recreate a new revision of same set",
			existing:        []*apps.ControllerRevision{rev0, rev1},
			set:             set1,
			expectedCount:   2,
			expectedCurrent: rev0,
			expectedUpdate:  rev1,
			err:             false,
		},
		{
			name:            "must rollback to a previous revision",
			existing:        []*apps.ControllerRevision{rev0, rev1, rev2},
			set:             set1,
			expectedCount:   3,
			expectedCurrent: rev0,
			expectedUpdate:  updateRevision(rev1, 4),
			err:             false,
		},
	}
	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestStatefulSetControlRollingUpdate(t *testing.T) {
	type testcase struct {
		name       string
		invariants func(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error
		initial    func() *appsv1alpha1.StatefulSet
		update     func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet
		validate   func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error
	}

	testFn := func(test *testcase, t *testing.T) {
		set := test.initial()
		client := fake.NewSimpleClientset()
		kruiseClient := kruisefake.NewSimpleClientset(set)
		spc, _, ssc, stop := setupController(client, kruiseClient)
		defer close(stop)
		if err := scaleUpStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set = test.update(set)
		if err := updateStatefulSetControl(set, ssc, spc, assertUpdateInvariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := test.validate(set, pods); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
	}

	tests := []testcase{
		{
			name:       "monotonic image update",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale up",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale down",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(5)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 3
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale up",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale down",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(5))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 3
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
	}
	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestStatefulSetControlOnDeleteUpdate(t *testing.T) {
	type testcase struct {
		name            string
		invariants      func(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error
		initial         func() *appsv1alpha1.StatefulSet
		update          func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet
		validateUpdate  func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error
		validateRestart func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error
	}

	originalImage := newStatefulSet(3).Spec.Template.Spec.Containers[0].Image

	testFn := func(test *testcase, t *testing.T) {
		set := test.initial()
		set.Spec.UpdateStrategy = appsv1alpha1.StatefulSetUpdateStrategy{Type: apps.OnDeleteStatefulSetStrategyType}
		client := fake.NewSimpleClientset()
		kruiseClient := kruisefake.NewSimpleClientset(set)
		spc, _, ssc, stop := setupController(client, kruiseClient)
		defer close(stop)
		if err := scaleUpStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set = test.update(set)
		if err := updateStatefulSetControl(set, ssc, spc, assertUpdateInvariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}

		selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := test.validateUpdate(set, pods); err != nil {
			for i := range pods {
				t.Log(pods[i].Name)
			}
			t.Fatalf("%s: %s", test.name, err)

		}
		replicas := *set.Spec.Replicas
		*set.Spec.Replicas = 0
		if err := scaleDownStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		*set.Spec.Replicas = replicas
		if err := scaleUpStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := test.validateRestart(set, pods); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
	}

	tests := []testcase{
		{
			name:       "monotonic image update",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRestart: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale up",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if i < 3 && pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
					if i >= 3 && pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRestart: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale down",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(5)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 3
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRestart: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRestart: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale up",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if i < 3 && pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
					if i >= 3 && pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRestart: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale down",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(5))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 3
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRestart: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
	}
	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestStatefulSetControlRollingUpdateWithPartition(t *testing.T) {
	type testcase struct {
		name       string
		partition  int32
		invariants func(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error
		initial    func() *appsv1alpha1.StatefulSet
		update     func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet
		validate   func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error
	}

	testFn := func(test *testcase, t *testing.T) {
		set := test.initial()
		set.Spec.UpdateStrategy = appsv1alpha1.StatefulSetUpdateStrategy{
			Type: apps.RollingUpdateStatefulSetStrategyType,
			RollingUpdate: func() *appsv1alpha1.RollingUpdateStatefulSetStrategy {
				return &appsv1alpha1.RollingUpdateStatefulSetStrategy{Partition: &test.partition}
			}(),
		}
		client := fake.NewSimpleClientset()
		kruiseClient := kruisefake.NewSimpleClientset(set)
		spc, _, ssc, stop := setupController(client, kruiseClient)
		defer close(stop)
		if err := scaleUpStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set = test.update(set)
		if err := updateStatefulSetControl(set, ssc, spc, assertUpdateInvariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := test.validate(set, pods); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
	}

	originalImage := newStatefulSet(3).Spec.Template.Spec.Containers[0].Image

	tests := []testcase{
		{
			name:       "monotonic image update",
			invariants: assertMonotonicInvariants,
			partition:  2,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if i < 2 && pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
					if i >= 2 && pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale up",
			partition:  2,
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if i < 2 && pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
					if i >= 2 && pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update",
			partition:  2,
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if i < 2 && pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
					if i >= 2 && pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale up",
			invariants: assertBurstInvariants,
			partition:  2,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if i < 2 && pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
					if i >= 2 && pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
	}
	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestStatefulSetControlRollingUpdateWithMaxUnavailable(t *testing.T) {
	set := burst(newStatefulSet(6))
	var partition int32 = 3
	var maxUnavailable = intstr.FromInt(2)
	set.Spec.UpdateStrategy = appsv1alpha1.StatefulSetUpdateStrategy{
		Type: apps.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: func() *appsv1alpha1.RollingUpdateStatefulSetStrategy {
			return &appsv1alpha1.RollingUpdateStatefulSetStrategy{
				Partition:      &partition,
				MaxUnavailable: &maxUnavailable,
			}
		}(),
	}

	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)
	if err := scaleUpStatefulSetControl(set, ssc, spc, assertBurstInvariants); err != nil {
		t.Fatal(err)
	}
	set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatal(err)
	}

	// start to update
	set.Spec.Template.Spec.Containers[0].Image = "foo"

	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		t.Fatal(err)
	}
	originalPods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(originalPods))

	// first update pods 4/5
	if err = ssc.UpdateStatefulSet(set, originalPods); err != nil {
		t.Fatal(err)
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if !reflect.DeepEqual(pods, originalPods[:4]) {
		t.Fatalf("Expected pods %v, got pods %v", originalPods[:3], pods)
	}

	// create new pods 4/5
	if err = ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 6 {
		t.Fatalf("Expected create pods 4/5, got pods %v", pods)
	}

	// if pod 4 ready, start to update pod 3
	spc.setPodRunning(set, 4)
	spc.setPodRunning(set, 5)
	originalPods, _ = spc.setPodReady(set, 4)
	sort.Sort(ascendingOrdinal(originalPods))
	if err = ssc.UpdateStatefulSet(set, originalPods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if !reflect.DeepEqual(pods, append(originalPods[:3], originalPods[4:]...)) {
		t.Fatalf("Expected pods %v, got pods %v", append(originalPods[:3], originalPods[4:]...), pods)
	}

	// create new pod 3
	if err = ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 6 {
		t.Fatalf("Expected create pods 2/3, got pods %v", pods)
	}

	// pods 3/4/5 ready, should not update other pods
	spc.setPodRunning(set, 3)
	spc.setPodRunning(set, 5)
	spc.setPodReady(set, 5)
	originalPods, _ = spc.setPodReady(set, 3)
	sort.Sort(ascendingOrdinal(originalPods))
	if err = ssc.UpdateStatefulSet(set, originalPods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if !reflect.DeepEqual(pods, originalPods) {
		t.Fatalf("Expected pods %v, got pods %v", originalPods, pods)
	}
}

func TestStatefulSetControlInPlaceUpdate(t *testing.T) {
	set := burst(newStatefulSet(3))
	var partition int32 = 1
	set.Spec.UpdateStrategy = appsv1alpha1.StatefulSetUpdateStrategy{
		Type: apps.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: func() *appsv1alpha1.RollingUpdateStatefulSetStrategy {
			return &appsv1alpha1.RollingUpdateStatefulSetStrategy{
				Partition:       &partition,
				PodUpdatePolicy: appsv1alpha1.InPlaceIfPossiblePodUpdateStrategyType,
			}
		}(),
	}
	set.Spec.Template.Spec.ReadinessGates = append(set.Spec.Template.Spec.ReadinessGates, v1.PodReadinessGate{ConditionType: appsv1alpha1.StatefulSetInPlaceUpdateReady})

	client := fake.NewSimpleClientset()
	kruiseClient := kruisefake.NewSimpleClientset(set)
	spc, _, ssc, stop := setupController(client, kruiseClient)
	defer close(stop)
	if err := scaleUpStatefulSetControl(set, ssc, spc, assertBurstInvariants); err != nil {
		t.Fatal(err)
	}
	set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		t.Fatal(err)
	}

	// ready to update
	set.Spec.Template.Spec.Containers[0].Image = "foo"

	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		t.Fatal(err)
	}
	originalPods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(originalPods))
	// mock pod container statuses
	for _, p := range originalPods {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, v1.ContainerStatus{
			Name:    "nginx",
			ImageID: "imgID1",
		})
	}
	oldRevision := originalPods[2].Labels[apps.StatefulSetRevisionLabel]

	// in-place update pod 2
	if err = ssc.UpdateStatefulSet(set, originalPods); err != nil {
		t.Fatal(err)
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if len(pods) < 3 {
		t.Fatalf("Expected in-place update, actually got pods num: %v", len(pods))
	}

	if pods[2].Spec.Containers[0].Image != "foo" ||
		pods[2].Labels[apps.StatefulSetRevisionLabel] == oldRevision {
		t.Fatalf("Expected in-place update pod2, actually got %+v", pods[2])
	}
	condition := getInPlaceUpdateReadyCondition(pods[2])
	if condition == nil || condition.Status != v1.ConditionFalse {
		t.Fatalf("Expected InPlaceUpdateReady condition False after in-place update, got %v", condition)
	}

	// should not update pod 1, because of pod2 status not changed
	if err = ssc.UpdateStatefulSet(set, originalPods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if pods[1].Labels[apps.StatefulSetRevisionLabel] != oldRevision {
		t.Fatalf("Expected not to update pod1, actually got %+v", pods[1])
	}

	// update pod2 status, then update pod 1
	pods[2].Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:    "nginx",
		ImageID: "imgID2",
	}}
	if err = ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if len(pods) < 3 {
		t.Fatalf("Expected in-place update, actually got pods num: %v", len(pods))
	}

	if pods[1].Spec.Containers[0].Image != "foo" ||
		pods[1].Labels[apps.StatefulSetRevisionLabel] == oldRevision {
		t.Fatalf("Expected in-place update pod1, actually got %+v", pods[2])
	}
	condition = getInPlaceUpdateReadyCondition(pods[1])
	if condition == nil || condition.Status != v1.ConditionFalse {
		t.Fatalf("Expected InPlaceUpdateReady condition False after in-place update, got %v", condition)
	}
	condition = getInPlaceUpdateReadyCondition(pods[2])
	if condition == nil || condition.Status != v1.ConditionTrue {
		t.Fatalf("Expected InPlaceUpdateReady condition True after in-place update completed, got %v", condition)
	}

	// should not update pod 0
	pods[1].Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:    "nginx",
		ImageID: "imgID2",
	}}
	if err = ssc.UpdateStatefulSet(set, pods); err != nil {
		t.Fatal(err)
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(ascendingOrdinal(pods))
	if len(pods) < 3 {
		t.Fatalf("Expected no update, actually got pods num: %v", len(pods))
	}
	if pods[0].Labels[apps.StatefulSetRevisionLabel] != oldRevision {
		t.Fatalf("Expected not to update pod0, actually got %+v", pods[1])
	}
	condition = getInPlaceUpdateReadyCondition(pods[1])
	if condition == nil || condition.Status != v1.ConditionTrue {
		t.Fatalf("Expected InPlaceUpdateReady condition True after in-place update completed, got %v", condition)
	}
}

func TestStatefulSetControlLimitsHistory(t *testing.T) {
	type testcase struct {
		name       string
		invariants func(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error
		initial    func() *appsv1alpha1.StatefulSet
	}

	testFn := func(test *testcase, t *testing.T) {
		set := test.initial()
		client := fake.NewSimpleClientset()
		kruiseClient := kruisefake.NewSimpleClientset(set)
		spc, _, ssc, stop := setupController(client, kruiseClient)
		defer close(stop)
		if err := scaleUpStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		for i := 0; i < 10; i++ {
			set.Spec.Template.Spec.Containers[0].Image = fmt.Sprintf("foo-%d", i)
			if err := updateStatefulSetControl(set, ssc, spc, assertUpdateInvariants); err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}
			selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
			if err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}
			pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
			if err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}
			set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
			if err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}
			err = ssc.UpdateStatefulSet(set, pods)
			if err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}
			revisions, err := ssc.ListRevisions(set)
			if err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}
			if len(revisions) > int(*set.Spec.RevisionHistoryLimit)+2 {
				t.Fatalf("%s: %d greater than limit %d", test.name, len(revisions), *set.Spec.RevisionHistoryLimit)
			}
		}
	}

	tests := []testcase{
		{
			name:       "monotonic update",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
		},
		{
			name:       "burst update",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
		},
	}
	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestStatefulSetControlRollback(t *testing.T) {
	type testcase struct {
		name             string
		invariants       func(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error
		initial          func() *appsv1alpha1.StatefulSet
		update           func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet
		validateUpdate   func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error
		validateRollback func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error
	}

	originalImage := newStatefulSet(3).Spec.Template.Spec.Containers[0].Image

	testFn := func(test *testcase, t *testing.T) {
		set := test.initial()
		client := fake.NewSimpleClientset()
		kruiseClient := kruisefake.NewSimpleClientset(set)
		spc, _, ssc, stop := setupController(client, kruiseClient)
		defer close(stop)
		if err := scaleUpStatefulSetControl(set, ssc, spc, test.invariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err := spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set = test.update(set)
		if err := updateStatefulSetControl(set, ssc, spc, assertUpdateInvariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := test.validateUpdate(set, pods); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		revisions, err := ssc.ListRevisions(set)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		history.SortControllerRevisions(revisions)
		set, err = ApplyRevision(set, revisions[0])
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := updateStatefulSetControl(set, ssc, spc, assertUpdateInvariants); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
		if err := test.validateRollback(set, pods); err != nil {
			t.Fatalf("%s: %s", test.name, err)
		}
	}

	tests := []testcase{
		{
			name:       "monotonic image update",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRollback: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale up",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(3)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRollback: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "monotonic image update and scale down",
			invariants: assertMonotonicInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return newStatefulSet(5)
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 3
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRollback: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRollback: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale up",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(3))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 5
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRollback: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
		{
			name:       "burst image update and scale down",
			invariants: assertBurstInvariants,
			initial: func() *appsv1alpha1.StatefulSet {
				return burst(newStatefulSet(5))
			},
			update: func(set *appsv1alpha1.StatefulSet) *appsv1alpha1.StatefulSet {
				*set.Spec.Replicas = 3
				set.Spec.Template.Spec.Containers[0].Image = "foo"
				return set
			},
			validateUpdate: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != "foo" {
						return fmt.Errorf("want pod %s image foo found %s", pods[i].Name, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
			validateRollback: func(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) error {
				sort.Sort(ascendingOrdinal(pods))
				for i := range pods {
					if pods[i].Spec.Containers[0].Image != originalImage {
						return fmt.Errorf("want pod %s image %s found %s", pods[i].Name, originalImage, pods[i].Spec.Containers[0].Image)
					}
				}
				return nil
			},
		},
	}
	for i := range tests {
		testFn(&tests[i], t)
	}
}

type requestTracker struct {
	requests int
	err      error
	after    int
}

func (rt *requestTracker) errorReady() bool {
	return rt.err != nil && rt.requests >= rt.after
}

func (rt *requestTracker) inc() {
	rt.requests++
}

func (rt *requestTracker) reset() {
	rt.err = nil
	rt.after = 0
}

type fakeStatefulPodControl struct {
	podsLister              corelisters.PodLister
	claimsLister            corelisters.PersistentVolumeClaimLister
	setsLister              kruiseappslisters.StatefulSetLister
	podsIndexer             cache.Indexer
	claimsIndexer           cache.Indexer
	setsIndexer             cache.Indexer
	createPodTracker        requestTracker
	updatePodTracker        requestTracker
	inPlaceUpdatePodTracker requestTracker
	deletePodTracker        requestTracker
}

func newFakeStatefulPodControl(podInformer coreinformers.PodInformer, setInformer kruiseappsinformers.StatefulSetInformer) *fakeStatefulPodControl {
	claimsIndexer := cache.NewIndexer(controller.KeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	return &fakeStatefulPodControl{
		podInformer.Lister(),
		corelisters.NewPersistentVolumeClaimLister(claimsIndexer),
		setInformer.Lister(),
		podInformer.Informer().GetIndexer(),
		claimsIndexer,
		setInformer.Informer().GetIndexer(),
		requestTracker{0, nil, 0},
		requestTracker{0, nil, 0},
		requestTracker{0, nil, 0},
		requestTracker{0, nil, 0}}
}

func (spc *fakeStatefulPodControl) SetCreateStatefulPodError(err error, after int) {
	spc.createPodTracker.err = err
	spc.createPodTracker.after = after
}

func (spc *fakeStatefulPodControl) SetUpdateStatefulPodError(err error, after int) {
	spc.updatePodTracker.err = err
	spc.updatePodTracker.after = after
}

func (spc *fakeStatefulPodControl) SetDeleteStatefulPodError(err error, after int) {
	spc.deletePodTracker.err = err
	spc.deletePodTracker.after = after
}

func (spc *fakeStatefulPodControl) setPodPending(set *appsv1alpha1.StatefulSet, ordinal int) ([]*v1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	if 0 > ordinal || ordinal >= len(pods) {
		return nil, fmt.Errorf("ordinal %d out of range [0,%d)", ordinal, len(pods))
	}
	sort.Sort(ascendingOrdinal(pods))
	pod := pods[ordinal].DeepCopy()
	pod.Status.Phase = v1.PodPending
	fakeResourceVersion(pod)
	spc.podsIndexer.Update(pod)
	return spc.podsLister.Pods(set.Namespace).List(selector)
}

func (spc *fakeStatefulPodControl) setPodRunning(set *appsv1alpha1.StatefulSet, ordinal int) ([]*v1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	if 0 > ordinal || ordinal >= len(pods) {
		return nil, fmt.Errorf("ordinal %d out of range [0,%d)", ordinal, len(pods))
	}
	sort.Sort(ascendingOrdinal(pods))
	pod := pods[ordinal].DeepCopy()
	pod.Status.Phase = v1.PodRunning
	fakeResourceVersion(pod)
	spc.podsIndexer.Update(pod)
	return spc.podsLister.Pods(set.Namespace).List(selector)
}

func (spc *fakeStatefulPodControl) setPodReady(set *appsv1alpha1.StatefulSet, ordinal int) ([]*v1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	if 0 > ordinal || ordinal >= len(pods) {
		return nil, fmt.Errorf("ordinal %d out of range [0,%d)", ordinal, len(pods))
	}
	sort.Sort(ascendingOrdinal(pods))
	pod := pods[ordinal].DeepCopy()
	condition := v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionTrue}
	podutil.UpdatePodCondition(&pod.Status, &condition)
	fakeResourceVersion(pod)
	spc.podsIndexer.Update(pod)
	return spc.podsLister.Pods(set.Namespace).List(selector)
}

func (spc *fakeStatefulPodControl) addTerminatingPod(set *appsv1alpha1.StatefulSet, ordinal int) ([]*v1.Pod, error) {
	pod := newStatefulSetPod(set, ordinal)
	pod.Status.Phase = v1.PodRunning
	deleted := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &deleted
	condition := v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionTrue}
	fakeResourceVersion(pod)
	podutil.UpdatePodCondition(&pod.Status, &condition)
	spc.podsIndexer.Update(pod)
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return nil, err
	}
	return spc.podsLister.Pods(set.Namespace).List(selector)
}

func (spc *fakeStatefulPodControl) setPodTerminated(set *appsv1alpha1.StatefulSet, ordinal int) ([]*v1.Pod, error) {
	pod := newStatefulSetPod(set, ordinal)
	deleted := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &deleted
	fakeResourceVersion(pod)
	spc.podsIndexer.Update(pod)
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return nil, err
	}
	return spc.podsLister.Pods(set.Namespace).List(selector)
}

func (spc *fakeStatefulPodControl) CreateStatefulPod(set *appsv1alpha1.StatefulSet, pod *v1.Pod) error {
	defer spc.createPodTracker.inc()
	if spc.createPodTracker.errorReady() {
		defer spc.createPodTracker.reset()
		return spc.createPodTracker.err
	}

	for _, claim := range getPersistentVolumeClaims(set, pod) {
		spc.claimsIndexer.Update(&claim)
	}
	spc.podsIndexer.Update(pod)
	return nil
}

func (spc *fakeStatefulPodControl) UpdateStatefulPod(set *appsv1alpha1.StatefulSet, pod *v1.Pod) error {
	defer spc.updatePodTracker.inc()
	if spc.updatePodTracker.errorReady() {
		defer spc.updatePodTracker.reset()
		return spc.updatePodTracker.err
	}
	if !identityMatches(set, pod) {
		updateIdentity(set, pod)
	}
	if !storageMatches(set, pod) {
		updateStorage(set, pod)
		for _, claim := range getPersistentVolumeClaims(set, pod) {
			spc.claimsIndexer.Update(&claim)
		}
	}
	spc.podsIndexer.Update(pod)
	return nil
}

func (spc *fakeStatefulPodControl) InPlaceUpdateStatefulPod(set *appsv1alpha1.StatefulSet, pod *v1.Pod, spec *InPlaceUpdateSpec) error {
	defer spc.inPlaceUpdatePodTracker.inc()
	if spc.inPlaceUpdatePodTracker.errorReady() {
		defer spc.inPlaceUpdatePodTracker.reset()
		return spc.inPlaceUpdatePodTracker.err
	}

	newPod, err := podInPlaceUpdate(pod, spec)
	if err != nil {
		return err
	}
	spc.podsIndexer.Update(newPod)

	return nil
}

func (spc *fakeStatefulPodControl) UpdateStatefulPodCondition(set *appsv1alpha1.StatefulSet, pod *v1.Pod, condition v1.PodCondition) error {
	updatePodCondition(pod, condition)
	spc.podsIndexer.Update(pod)

	return nil
}

func (spc *fakeStatefulPodControl) DeleteStatefulPod(set *appsv1alpha1.StatefulSet, pod *v1.Pod) error {
	defer spc.deletePodTracker.inc()
	if spc.deletePodTracker.errorReady() {
		defer spc.deletePodTracker.reset()
		return spc.deletePodTracker.err
	}
	if key, err := controller.KeyFunc(pod); err != nil {
		return err
	} else if obj, found, err := spc.podsIndexer.GetByKey(key); err != nil {
		return err
	} else if found {
		spc.podsIndexer.Delete(obj)
	}

	return nil
}

var _ StatefulPodControlInterface = &fakeStatefulPodControl{}

type fakeStatefulSetStatusUpdater struct {
	setsLister          kruiseappslisters.StatefulSetLister
	setsIndexer         cache.Indexer
	updateStatusTracker requestTracker
}

func newFakeStatefulSetStatusUpdater(setInformer kruiseappsinformers.StatefulSetInformer) *fakeStatefulSetStatusUpdater {
	return &fakeStatefulSetStatusUpdater{
		setInformer.Lister(),
		setInformer.Informer().GetIndexer(),
		requestTracker{0, nil, 0},
	}
}

func (ssu *fakeStatefulSetStatusUpdater) UpdateStatefulSetStatus(set *appsv1alpha1.StatefulSet, status *appsv1alpha1.StatefulSetStatus) error {
	defer ssu.updateStatusTracker.inc()
	if ssu.updateStatusTracker.errorReady() {
		defer ssu.updateStatusTracker.reset()
		return ssu.updateStatusTracker.err
	}
	set.Status = *status
	ssu.setsIndexer.Update(set)
	return nil
}

func (ssu *fakeStatefulSetStatusUpdater) SetUpdateStatefulSetStatusError(err error, after int) {
	ssu.updateStatusTracker.err = err
	ssu.updateStatusTracker.after = after
}

var _ StatefulSetStatusUpdaterInterface = &fakeStatefulSetStatusUpdater{}

func assertMonotonicInvariants(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return err
	}
	sort.Sort(ascendingOrdinal(pods))
	for ord := 0; ord < len(pods); ord++ {
		if ord > 0 && isRunningAndReady(pods[ord]) && !isRunningAndReady(pods[ord-1]) {
			return fmt.Errorf("Successor %s is Running and Ready while %s is not", pods[ord].Name, pods[ord-1].Name)
		}

		if getOrdinal(pods[ord]) != ord {
			return fmt.Errorf("pods %s deployed in the wrong order %d", pods[ord].Name, ord)
		}

		if !storageMatches(set, pods[ord]) {
			return fmt.Errorf("pods %s does not match the storage specification of StatefulSet %s ", pods[ord].Name, set.Name)
		}

		for _, claim := range getPersistentVolumeClaims(set, pods[ord]) {
			claim, err := spc.claimsLister.PersistentVolumeClaims(set.Namespace).Get(claim.Name)
			if err != nil {
				return err
			}
			if claim == nil {
				return fmt.Errorf("claim %s for Pod %s was not created", claim.Name, pods[ord].Name)
			}
		}

		if !identityMatches(set, pods[ord]) {
			return fmt.Errorf("pods %s does not match the identity specification of StatefulSet %s ", pods[ord].Name, set.Name)
		}
	}
	return nil
}

func assertBurstInvariants(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return err
	}
	sort.Sort(ascendingOrdinal(pods))
	for ord := 0; ord < len(pods); ord++ {
		if !storageMatches(set, pods[ord]) {
			return fmt.Errorf("pods %s does not match the storage specification of StatefulSet %s ", pods[ord].Name, set.Name)
		}

		for _, claim := range getPersistentVolumeClaims(set, pods[ord]) {
			claim, err := spc.claimsLister.PersistentVolumeClaims(set.Namespace).Get(claim.Name)
			if err != nil {
				return err
			}
			if claim == nil {
				return fmt.Errorf("claim %s for Pod %s was not created", claim.Name, pods[ord].Name)
			}
		}

		if !identityMatches(set, pods[ord]) {
			return fmt.Errorf("pods %s does not match the identity specification of StatefulSet %s ",
				pods[ord].Name,
				set.Name)
		}
	}
	return nil
}

func assertUpdateInvariants(set *appsv1alpha1.StatefulSet, spc *fakeStatefulPodControl) error {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return err
	}
	sort.Sort(ascendingOrdinal(pods))
	for ord := 0; ord < len(pods); ord++ {

		if !storageMatches(set, pods[ord]) {
			return fmt.Errorf("pod %s does not match the storage specification of StatefulSet %s ", pods[ord].Name, set.Name)
		}

		for _, claim := range getPersistentVolumeClaims(set, pods[ord]) {
			claim, err := spc.claimsLister.PersistentVolumeClaims(set.Namespace).Get(claim.Name)
			if err != nil {
				return err
			}
			if claim == nil {
				return fmt.Errorf("claim %s for Pod %s was not created", claim.Name, pods[ord].Name)
			}
		}

		if !identityMatches(set, pods[ord]) {
			return fmt.Errorf("pod %s does not match the identity specification of StatefulSet %s ", pods[ord].Name, set.Name)
		}
	}
	if set.Spec.UpdateStrategy.Type == apps.OnDeleteStatefulSetStrategyType {
		return nil
	}
	if set.Spec.UpdateStrategy.Type == apps.RollingUpdateStatefulSetStrategyType {
		for i := 0; i < int(set.Status.CurrentReplicas) && i < len(pods); i++ {
			if want, got := set.Status.CurrentRevision, getPodRevision(pods[i]); want != got {
				return fmt.Errorf("pod %s want current revision %s got %s", pods[i].Name, want, got)
			}
		}
		for i, j := len(pods)-1, 0; j < int(set.Status.UpdatedReplicas); i, j = i-1, j+1 {
			if want, got := set.Status.UpdateRevision, getPodRevision(pods[i]); want != got {
				return fmt.Errorf("pod %s want update revision %s got %s", pods[i].Name, want, got)
			}
		}
	}
	return nil
}

func fakeResourceVersion(object interface{}) {
	obj, isObj := object.(metav1.Object)
	if !isObj {
		return
	}
	if version := obj.GetResourceVersion(); version == "" {
		obj.SetResourceVersion("1")
	} else if intValue, err := strconv.ParseInt(version, 10, 32); err == nil {
		obj.SetResourceVersion(strconv.FormatInt(intValue+1, 10))
	}
}

func scaleUpStatefulSetControl(set *appsv1alpha1.StatefulSet,
	ssc StatefulSetControlInterface,
	spc *fakeStatefulPodControl,
	invariants invariantFunc) error {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return err
	}
	for set.Status.ReadyReplicas < *set.Spec.Replicas {
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			return err
		}
		sort.Sort(ascendingOrdinal(pods))

		// ensure all pods are valid (have a phase)
		initialized := false
		for ord, pod := range pods {
			if pod.Status.Phase == "" {
				if pods, err = spc.setPodPending(set, ord); err != nil {
					return err
				}
				break
			}
		}
		if initialized {
			continue
		}

		// select one of the pods and move it forward in status
		if len(pods) > 0 {
			ord := int(rand.Int63n(int64(len(pods))))
			pod := pods[ord]
			switch pod.Status.Phase {
			case v1.PodPending:
				if pods, err = spc.setPodRunning(set, ord); err != nil {
					return err
				}
			case v1.PodRunning:
				if pods, err = spc.setPodReady(set, ord); err != nil {
					return err
				}
			default:
				continue
			}
		}

		// run the controller once and check invariants
		if err = ssc.UpdateStatefulSet(set, pods); err != nil {
			return err
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			return err
		}
		if err := invariants(set, spc); err != nil {
			return err
		}
	}
	return invariants(set, spc)
}

func scaleDownStatefulSetControl(set *appsv1alpha1.StatefulSet, ssc StatefulSetControlInterface, spc *fakeStatefulPodControl, invariants invariantFunc) error {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return err
	}
	for set.Status.Replicas > *set.Spec.Replicas {
		pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			return err
		}
		sort.Sort(ascendingOrdinal(pods))
		if ordinal := len(pods) - 1; ordinal >= 0 {
			if err := ssc.UpdateStatefulSet(set, pods); err != nil {
				return err
			}
			set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
			if err != nil {
				return err
			}
			if pods, err = spc.addTerminatingPod(set, ordinal); err != nil {
				return err
			}
			if err = ssc.UpdateStatefulSet(set, pods); err != nil {
				return err
			}
			set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
			if err != nil {
				return err
			}
			pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
			if err != nil {
				return err
			}
			sort.Sort(ascendingOrdinal(pods))

			if len(pods) > 0 {
				spc.podsIndexer.Delete(pods[len(pods)-1])
			}
		}
		if err := ssc.UpdateStatefulSet(set, pods); err != nil {
			return err
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			return err
		}
		if err := invariants(set, spc); err != nil {
			return err
		}
	}
	return invariants(set, spc)
}

func updateComplete(set *appsv1alpha1.StatefulSet, pods []*v1.Pod) bool {
	sort.Sort(ascendingOrdinal(pods))
	if len(pods) != int(*set.Spec.Replicas) {
		return false
	}
	if set.Status.ReadyReplicas != *set.Spec.Replicas {
		return false
	}

	switch set.Spec.UpdateStrategy.Type {
	case apps.OnDeleteStatefulSetStrategyType:
		return true
	case apps.RollingUpdateStatefulSetStrategyType:
		if set.Spec.UpdateStrategy.RollingUpdate == nil || *set.Spec.UpdateStrategy.RollingUpdate.Partition <= 0 {
			if set.Status.CurrentReplicas < *set.Spec.Replicas {
				return false
			}
			for i := range pods {
				if getPodRevision(pods[i]) != set.Status.CurrentRevision {
					return false
				}
			}
		} else {
			partition := int(*set.Spec.UpdateStrategy.RollingUpdate.Partition)
			if len(pods) < partition {
				return false
			}
			for i := partition; i < len(pods); i++ {
				if getPodRevision(pods[i]) != set.Status.UpdateRevision {
					return false
				}
			}
		}
	}
	return true
}

func updateStatefulSetControl(set *appsv1alpha1.StatefulSet,
	ssc StatefulSetControlInterface,
	spc *fakeStatefulPodControl,
	invariants invariantFunc) error {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return err
	}
	if err = ssc.UpdateStatefulSet(set, pods); err != nil {
		return err
	}

	set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
	if err != nil {
		return err
	}
	pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
	if err != nil {
		return err
	}
	for !updateComplete(set, pods) {
		pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			return err
		}
		sort.Sort(ascendingOrdinal(pods))
		initialized := false
		for ord, pod := range pods {
			if pod.Status.Phase == "" {
				if pods, err = spc.setPodPending(set, ord); err != nil {
					return err
				}
				break
			}
		}
		if initialized {
			continue
		}

		if len(pods) > 0 {
			ord := int(rand.Int63n(int64(len(pods))))
			pod := pods[ord]
			switch pod.Status.Phase {
			case v1.PodPending:
				if pods, err = spc.setPodRunning(set, ord); err != nil {
					return err
				}
			case v1.PodRunning:
				if pods, err = spc.setPodReady(set, ord); err != nil {
					return err
				}
			default:
				continue
			}
		}

		if err = ssc.UpdateStatefulSet(set, pods); err != nil {
			return err
		}
		set, err = spc.setsLister.StatefulSets(set.Namespace).Get(set.Name)
		if err != nil {
			return err
		}
		if err := invariants(set, spc); err != nil {
			return err
		}
		pods, err = spc.podsLister.Pods(set.Namespace).List(selector)
		if err != nil {
			return err
		}
	}
	return invariants(set, spc)
}

func newRevisionOrDie(set *appsv1alpha1.StatefulSet, revision int64) *apps.ControllerRevision {
	rev, err := newRevision(set, revision, set.Status.CollisionCount)
	if err != nil {
		panic(err)
	}
	return rev
}
