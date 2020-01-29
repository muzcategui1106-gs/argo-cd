package sync

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/argoproj/argo-cd/common"

	"github.com/argoproj/argo-cd/engine/pkg/utils/kube"

	synccommon "github.com/argoproj/argo-cd/engine/pkg/utils/kube/sync/common"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	fakedisco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/rest"
	testcore "k8s.io/client-go/testing"

	"github.com/argoproj/argo-cd/engine/pkg/utils/kube/kubetest"
	"github.com/argoproj/argo-cd/test"
)

func newTestSyncCtx(opts ...SyncOpt) *syncContext {
	fakeDisco := &fakedisco.FakeDiscovery{Fake: &testcore.Fake{}}
	fakeDisco.Resources = append(make([]*v1.APIResourceList, 0),
		&v1.APIResourceList{
			GroupVersion: "v1",
			APIResources: []v1.APIResource{
				{Kind: "Pod", Group: "", Version: "v1", Namespaced: true},
				{Kind: "Service", Group: "", Version: "v1", Namespaced: true},
			},
		},
		&v1.APIResourceList{
			GroupVersion: "apps/v1",
			APIResources: []v1.APIResource{
				{Kind: "Deployment", Group: "apps", Version: "v1", Namespaced: true},
			},
		})
	sc := syncContext{
		config:    &rest.Config{},
		namespace: test.FakeArgoCDNamespace,
		revision:  "FooBarBaz",
		disco:     fakeDisco,
		log:       log.WithFields(log.Fields{"application": "fake-app"}),
		resources: map[kube.ResourceKey]reconciledResource{},
		syncRes:   map[string]synccommon.ResourceSyncResult{},
	}
	sc.permissionValidator = func(un *unstructured.Unstructured, res *v1.APIResource) error {
		return nil
	}
	sc.kubectl = &kubetest.MockKubectlCmd{}
	for _, opt := range opts {
		opt(&sc)
	}
	return &sc
}

//func newManagedResource(live *unstructured.Unstructured) reconciledResource {
//	return reconciledResource{Live: live}
//}

func TestSyncNotPermittedNamespace(t *testing.T) {
	syncCtx := newTestSyncCtx(WithPermissionValidator(func(un *unstructured.Unstructured, res *v1.APIResource) error {
		return fmt.Errorf("not permitted in project")
	}))
	targetPod := test.NewPod()
	targetPod.SetNamespace("kube-system")
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil, nil},
		Target: []*unstructured.Unstructured{targetPod, test.NewService()},
	})
	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()
	assert.Equal(t, synccommon.OperationFailed, phase)
	assert.Contains(t, resources[0].Message, "not permitted in project")
}

func TestSyncCreateInSortedOrder(t *testing.T) {
	syncCtx := newTestSyncCtx()
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil, nil},
		Target: []*unstructured.Unstructured{test.NewPod(), test.NewService()},
	})
	syncCtx.Sync()

	phase, _, resources := syncCtx.GetState()

	assert.Equal(t, synccommon.OperationSucceeded, phase)
	assert.Len(t, resources, 2)
	for i := range resources {
		result := resources[i]
		if result.ResourceKey.Kind == "Pod" {
			assert.Equal(t, synccommon.ResultCodeSynced, result.Status)
			assert.Equal(t, "", result.Message)
		} else if result.ResourceKey.Kind == "Service" {
			assert.Equal(t, "", result.Message)
		} else {
			t.Error("Resource isn't a pod or a service")
		}
	}
}

//func TestSyncCreateNotWhitelistedClusterResources(t *testing.T) {
//	syncCtx := newTestSyncCtx(&v1.APIResourceList{
//		GroupVersion: v1alpha1.SchemeGroupVersion.String(),
//		APIResources: []v1.APIResource{
//			{Name: "workflows", Namespaced: false, Kind: "Workflow", Group: "argoproj.io"},
//			{Name: "application", Namespaced: false, Kind: "Application", Group: "argoproj.io"},
//		},
//	}, &v1.APIResourceList{
//		GroupVersion: "rbac.authorization.k8s.io/v1",
//		APIResources: []v1.APIResource{
//			{Name: "clusterroles", Namespaced: false, Kind: "ClusterRole", Group: "rbac.authorization.k8s.io"},
//		},
//	})
//
//	syncCtx.proj.Spec.ClusterResourceWhitelist = []v1.GroupKind{
//		{Group: "argoproj.io", Kind: "*"},
//	}
//
//	syncCtx.kubectl = &kubetest.MockKubectlCmd{}
//	syncCtx.compareResult = &controller.comparisonResult{
//		managedResources: []controller.managedResource{{
//			Live: nil,
//			Target: kube.MustToUnstructured(&rbacv1.ClusterRole{
//				TypeMeta:   metav1.TypeMeta{Kind: "ClusterRole", APIVersion: "rbac.authorization.k8s.io/v1"},
//				ObjectMeta: metav1.ObjectMeta{Name: "argo-ui-cluster-role"}}),
//		}},
//	}
//	syncCtx.sync()
//	assert.Len(t, syncCtx.syncRes.Resources, 1)
//	result := syncCtx.syncRes.Resources[0]
//	assert.Equal(t, v1alpha1.ResultCodeSyncFailed, result.Status)
//	assert.Contains(t, result.Message, "not permitted in project")
//}
//
//func TestSyncBlacklistedNamespacedResources(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//
//	syncCtx.proj.Spec.NamespaceResourceBlacklist = []v1.GroupKind{
//		{Group: "*", Kind: "Deployment"},
//	}
//
//	syncCtx.compareResult = &controller.comparisonResult{
//		managedResources: []controller.managedResource{{
//			Live:   nil,
//			Target: test.NewDeployment(),
//		}},
//	}
//	syncCtx.sync()
//	assert.Len(t, syncCtx.syncRes.Resources, 1)
//	result := syncCtx.syncRes.Resources[0]
//	assert.Equal(t, v1alpha1.ResultCodeSyncFailed, result.Status)
//	assert.Contains(t, result.Message, "not permitted in project")
//}
//
func TestSyncSuccessfully(t *testing.T) {
	syncCtx := newTestSyncCtx(WithOperationSettings(false, true, false))
	pod := test.NewPod()
	pod.SetNamespace(test.FakeArgoCDNamespace)
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil, pod},
		Target: []*unstructured.Unstructured{test.NewService(), nil},
	})

	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()

	assert.Equal(t, synccommon.OperationSucceeded, phase)
	assert.Len(t, resources, 2)
	for i := range resources {
		result := resources[i]
		if result.ResourceKey.Kind == "Pod" {
			assert.Equal(t, synccommon.ResultCodePruned, result.Status)
			assert.Equal(t, "pruned", result.Message)
		} else if result.ResourceKey.Kind == "Service" {
			assert.Equal(t, synccommon.ResultCodeSynced, result.Status)
			assert.Equal(t, "", result.Message)
		} else {
			t.Error("Resource isn't a pod or a service")
		}
	}
}

func TestSyncDeleteSuccessfully(t *testing.T) {
	syncCtx := newTestSyncCtx(WithOperationSettings(false, true, false))
	svc := test.NewService()
	svc.SetNamespace(test.FakeArgoCDNamespace)
	pod := test.NewPod()
	pod.SetNamespace(test.FakeArgoCDNamespace)
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{svc, pod},
		Target: []*unstructured.Unstructured{nil, nil},
	})

	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()

	assert.Equal(t, synccommon.OperationSucceeded, phase)
	for i := range resources {
		result := resources[i]
		if result.ResourceKey.Kind == "Pod" {
			assert.Equal(t, synccommon.ResultCodePruned, result.Status)
			assert.Equal(t, "pruned", result.Message)
		} else if result.ResourceKey.Kind == "Service" {
			assert.Equal(t, synccommon.ResultCodePruned, result.Status)
			assert.Equal(t, "pruned", result.Message)
		} else {
			t.Error("Resource isn't a pod or a service")
		}
	}
}

func TestSyncCreateFailure(t *testing.T) {
	syncCtx := newTestSyncCtx()
	testSvc := test.NewService()
	syncCtx.kubectl = &kubetest.MockKubectlCmd{
		Commands: map[string]kubetest.KubectlOutput{
			testSvc.GetName(): {
				Output: "",
				Err:    fmt.Errorf("foo"),
			},
		},
	}
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil},
		Target: []*unstructured.Unstructured{testSvc},
	})

	syncCtx.Sync()
	_, _, resources := syncCtx.GetState()

	assert.Len(t, resources, 1)
	result := resources[0]
	assert.Equal(t, synccommon.ResultCodeSyncFailed, result.Status)
	assert.Equal(t, "foo", result.Message)
}

func TestSyncPruneFailure(t *testing.T) {
	syncCtx := newTestSyncCtx(WithOperationSettings(false, true, false))
	syncCtx.kubectl = &kubetest.MockKubectlCmd{
		Commands: map[string]kubetest.KubectlOutput{
			"test-service": {
				Output: "",
				Err:    fmt.Errorf("foo"),
			},
		},
	}
	testSvc := test.NewService()
	testSvc.SetName("test-service")
	testSvc.SetNamespace(test.FakeArgoCDNamespace)
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{testSvc},
		Target: []*unstructured.Unstructured{testSvc},
	})

	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()

	assert.Equal(t, synccommon.OperationFailed, phase)
	assert.Len(t, resources, 1)
	result := resources[0]
	assert.Equal(t, synccommon.ResultCodeSyncFailed, result.Status)
	assert.Equal(t, "foo", result.Message)
}

func TestDontSyncOrPruneHooks(t *testing.T) {
	syncCtx := newTestSyncCtx(WithOperationSettings(false, false, true))
	targetPod := test.NewPod()
	targetPod.SetName("dont-create-me")
	targetPod.SetAnnotations(map[string]string{common.AnnotationKeyHook: "PreSync"})
	liveSvc := test.NewService()
	liveSvc.SetName("dont-prune-me")
	liveSvc.SetNamespace(test.FakeArgoCDNamespace)
	liveSvc.SetAnnotations(map[string]string{common.AnnotationKeyHook: "PreSync"})

	syncCtx.hooks = []*unstructured.Unstructured{targetPod, liveSvc}
	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()
	assert.Len(t, resources, 0)
	assert.Equal(t, synccommon.OperationSucceeded, phase)
}

// make sure that we do not prune resources with Prune=false
func TestDontPrunePruneFalse(t *testing.T) {
	syncCtx := newTestSyncCtx(WithOperationSettings(false, true, false))
	pod := test.NewPod()
	pod.SetAnnotations(map[string]string{common.AnnotationSyncOptions: "Prune=false"})
	pod.SetNamespace(test.FakeArgoCDNamespace)
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{pod},
		Target: []*unstructured.Unstructured{nil},
	})

	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()

	assert.Equal(t, synccommon.OperationSucceeded, phase)
	assert.Len(t, resources, 1)
	assert.Equal(t, synccommon.ResultCodePruneSkipped, resources[0].Status)
	assert.Equal(t, "ignored (no prune)", resources[0].Message)

	syncCtx.Sync()

	phase, _, _ = syncCtx.GetState()
	assert.Equal(t, synccommon.OperationSucceeded, phase)
}

//// make sure Validate=false means we don't validate
func TestSyncOptionValidate(t *testing.T) {
	tests := []struct {
		name          string
		annotationVal string
		want          bool
	}{
		{"Empty", "", true},
		{"True", "Validate=true", true},
		{"False", "Validate=false", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncCtx := newTestSyncCtx()
			pod := test.NewPod()
			pod.SetAnnotations(map[string]string{common.AnnotationSyncOptions: tt.annotationVal})
			pod.SetNamespace(test.FakeArgoCDNamespace)
			syncCtx.resources = groupResources(ReconciliationResult{
				Live:   []*unstructured.Unstructured{pod},
				Target: []*unstructured.Unstructured{pod},
			})

			syncCtx.Sync()

			kubectl, _ := syncCtx.kubectl.(*kubetest.MockKubectlCmd)
			assert.Equal(t, tt.want, kubectl.LastValidate)
		})
	}
}

//func TestSelectiveSyncOnly(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//	pod1 := test.NewPod()
//	pod1.SetName("pod-1")
//	pod2 := test.NewPod()
//	pod2.SetName("pod-2")
//	syncCtx.compareResult = &controller.comparisonResult{
//		managedResources: []controller.managedResource{{Target: pod1}},
//	}
//	syncCtx.syncResources = []v1alpha1.SyncOperationResource{{Kind: "Pod", Name: "pod-1"}}
//
//	tasks, successful := syncCtx.getSyncTasks()
//
//	assert.True(t, successful)
//	assert.Len(t, tasks, 1)
//	assert.Equal(t, "pod-1", tasks[0].name())
//}

//func TestUnnamedHooksGetUniqueNames(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//	syncCtx.syncOp.SyncStrategy.Apply = nil
//	pod := test.NewPod()
//	pod.SetName("")
//	pod.SetAnnotations(map[string]string{common.AnnotationKeyHook: "PreSync,PostSync"})
//	syncCtx.compareResult = &controller.comparisonResult{hooks: []*unstructured.Unstructured{pod}}
//
//	tasks, successful := syncCtx.getSyncTasks()
//
//	assert.True(t, successful)
//	assert.Len(t, tasks, 2)
//	assert.Contains(t, tasks[0].name(), "foobarb-presync-")
//	assert.Contains(t, tasks[1].name(), "foobarb-postsync-")
//	assert.Equal(t, "", pod.GetName())
//}
//
//func TestManagedResourceAreNotNamed(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//	pod := test.NewPod()
//	pod.SetName("")
//	syncCtx.compareResult = &controller.comparisonResult{managedResources: []controller.managedResource{{Target: pod}}}
//
//	tasks, successful := syncCtx.getSyncTasks()
//
//	assert.True(t, successful)
//	assert.Len(t, tasks, 1)
//	assert.Equal(t, "", tasks[0].name())
//	assert.Equal(t, "", pod.GetName())
//}

func TestDeDupingTasks(t *testing.T) {
	syncCtx := newTestSyncCtx(WithOperationSettings(false, true, false))
	pod := test.NewPod()
	pod.SetAnnotations(map[string]string{common.AnnotationKeyHook: "Sync"})
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil},
		Target: []*unstructured.Unstructured{pod},
	})
	syncCtx.hooks = []*unstructured.Unstructured{pod}

	tasks, successful := syncCtx.getSyncTasks()

	assert.True(t, successful)
	assert.Len(t, tasks, 1)
}

//
//func TestObjectsGetANamespace(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//	pod := test.NewPod()
//	syncCtx.compareResult = &controller.comparisonResult{managedResources: []controller.managedResource{{Target: pod}}}
//
//	tasks, successful := syncCtx.getSyncTasks()
//
//	assert.True(t, successful)
//	assert.Len(t, tasks, 1)
//	assert.Equal(t, test.FakeArgoCDNamespace, tasks[0].namespace())
//	assert.Equal(t, "", pod.GetNamespace())
//}
//
//func TestPersistRevisionHistory(t *testing.T) {
//	app := controller.newFakeApp()
//	app.Status.OperationState = nil
//	app.Status.History = nil
//
//	defaultProject := &v1alpha1.AppProject{
//		ObjectMeta: v1.ObjectMeta{
//			Namespace: test.FakeArgoCDNamespace,
//			Name:      "default",
//		},
//	}
//	data := controller.fakeData{
//		apps: []runtime.Object{app, defaultProject},
//		manifestResponse: &apiclient.ManifestResponse{
//			Manifests: []string{},
//			Namespace: test.FakeDestNamespace,
//			Server:    test.FakeClusterURL,
//			Revision:  "abc123",
//		},
//		managedLiveObjs: make(map[kube.ResourceKey]*unstructured.Unstructured),
//	}
//	ctrl := controller.newFakeController(&data)
//
//	// Sync with source unspecified
//	opState := &v1alpha1.OperationState{Operation: v1alpha1.Operation{
//		Sync: &v1alpha1.SyncOperation{},
//	}}
//	ctrl.appStateManager.SyncAppState(app, opState)
//	// Ensure we record spec.source into sync result
//	assert.Equal(t, app.Spec.Source, opState.SyncResult.Source)
//
//	updatedApp, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(app.Name, v1.GetOptions{})
//	assert.Nil(t, err)
//	assert.Equal(t, 1, len(updatedApp.Status.History))
//	assert.Equal(t, app.Spec.Source, updatedApp.Status.History[0].Source)
//	assert.Equal(t, "abc123", updatedApp.Status.History[0].Revision)
//}
//
//func TestPersistRevisionHistoryRollback(t *testing.T) {
//	app := controller.newFakeApp()
//	app.Status.OperationState = nil
//	app.Status.History = nil
//	defaultProject := &v1alpha1.AppProject{
//		ObjectMeta: v1.ObjectMeta{
//			Namespace: test.FakeArgoCDNamespace,
//			Name:      "default",
//		},
//	}
//	data := controller.fakeData{
//		apps: []runtime.Object{app, defaultProject},
//		manifestResponse: &apiclient.ManifestResponse{
//			Manifests: []string{},
//			Namespace: test.FakeDestNamespace,
//			Server:    test.FakeClusterURL,
//			Revision:  "abc123",
//		},
//		managedLiveObjs: make(map[kube.ResourceKey]*unstructured.Unstructured),
//	}
//	ctrl := controller.newFakeController(&data)
//
//	// Sync with source specified
//	source := v1alpha1.ApplicationSource{
//		Helm: &v1alpha1.ApplicationSourceHelm{
//			Parameters: []v1alpha1.HelmParameter{
//				{
//					Name:  "test",
//					Value: "123",
//				},
//			},
//		},
//	}
//	opState := &v1alpha1.OperationState{Operation: v1alpha1.Operation{
//		Sync: &v1alpha1.SyncOperation{
//			Source: &source,
//		},
//	}}
//	ctrl.appStateManager.SyncAppState(app, opState)
//	// Ensure we record opState's source into sync result
//	assert.Equal(t, source, opState.SyncResult.Source)
//
//	updatedApp, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(app.Name, v1.GetOptions{})
//	assert.Nil(t, err)
//	assert.Equal(t, 1, len(updatedApp.Status.History))
//	assert.Equal(t, source, updatedApp.Status.History[0].Source)
//	assert.Equal(t, "abc123", updatedApp.Status.History[0].Revision)
//}

func TestSyncFailureHookWithSuccessfulSync(t *testing.T) {
	syncCtx := newTestSyncCtx()
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil},
		Target: []*unstructured.Unstructured{test.NewPod()},
	})
	syncCtx.hooks = []*unstructured.Unstructured{test.NewHook(synccommon.HookTypeSyncFail)}

	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()
	assert.Equal(t, synccommon.OperationSucceeded, phase)
	// only one result, we did not run the failure failureHook
	assert.Len(t, resources, 1)
}

//
//func TestSyncFailureHookWithFailedSync(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//	syncCtx.syncOp.SyncStrategy.Apply = nil
//	pod := test.NewPod()
//	syncCtx.compareResult = &controller.comparisonResult{
//		managedResources: []controller.managedResource{{Target: pod}},
//		hooks:            []*unstructured.Unstructured{test.NewHook(HookTypeSyncFail)},
//	}
//	syncCtx.kubectl = &kubetest.MockKubectlCmd{
//		Commands: map[string]kubetest.KubectlOutput{pod.GetName(): {Err: fmt.Errorf("")}},
//	}
//
//	syncCtx.sync()
//	syncCtx.sync()
//
//	assert.Equal(t, OperationFailed, syncCtx.opState.Phase)
//	assert.Len(t, syncCtx.syncRes.Resources, 2)
//}
//
//func TestBeforeHookCreation(t *testing.T) {
//	syncCtx := newTestSyncCtx()
//	syncCtx.syncOp.SyncStrategy.Apply = nil
//	hook := test.Annotate(test.Annotate(test.NewPod(), common.AnnotationKeyHook, "Sync"), common.AnnotationKeyHookDeletePolicy, "BeforeHookCreation")
//	hook.SetNamespace(test.FakeArgoCDNamespace)
//	syncCtx.compareResult = &controller.comparisonResult{
//		managedResources: []controller.managedResource{newManagedResource(hook)},
//		hooks:            []*unstructured.Unstructured{hook},
//	}
//	syncCtx.dynamicIf = fake.NewSimpleDynamicClient(runtime.NewScheme())
//
//	syncCtx.sync()
//	assert.Len(t, syncCtx.syncRes.Resources, 1)
//	assert.Empty(t, syncCtx.syncRes.Resources[0].Message)
//}
//
func TestRunSyncFailHooksFailed(t *testing.T) {
	// Tests that other SyncFail Hooks run even if one of them fail.

	syncCtx := newTestSyncCtx()
	pod := test.NewPod()
	successfulSyncFailHook := test.NewHook(synccommon.HookTypeSyncFail)
	successfulSyncFailHook.SetName("successful-sync-fail-hook")
	failedSyncFailHook := test.NewHook(synccommon.HookTypeSyncFail)
	failedSyncFailHook.SetName("failed-sync-fail-hook")
	syncCtx.resources = groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil},
		Target: []*unstructured.Unstructured{pod},
	})
	syncCtx.hooks = []*unstructured.Unstructured{successfulSyncFailHook, failedSyncFailHook}

	syncCtx.kubectl = &kubetest.MockKubectlCmd{
		Commands: map[string]kubetest.KubectlOutput{
			// Fail operation
			pod.GetName(): {Err: fmt.Errorf("")},
			// Fail a single SyncFail hook
			failedSyncFailHook.GetName(): {Err: fmt.Errorf("")}},
	}

	syncCtx.Sync()
	syncCtx.Sync()
	phase, _, resources := syncCtx.GetState()

	// Operation as a whole should fail
	assert.Equal(t, synccommon.OperationFailed, phase)
	// failedSyncFailHook should fail
	assert.Equal(t, synccommon.OperationFailed, resources[1].HookPhase)
	assert.Equal(t, synccommon.ResultCodeSyncFailed, resources[1].Status)
	// successfulSyncFailHook should be synced running (it is an nginx pod)
	assert.Equal(t, synccommon.OperationRunning, resources[2].HookPhase)
	assert.Equal(t, synccommon.ResultCodeSynced, resources[2].Status)
}

//
//func Test_syncContext_isSelectiveSync(t *testing.T) {
//	type fields struct {
//		compareResult *controller.comparisonResult
//		syncResources []SyncOperationResource
//	}
//	oneSyncResource := []SyncOperationResource{{}}
//	oneResource := func(group, kind, name string, hook bool) *controller.comparisonResult {
//		return &controller.comparisonResult{resources: []v1alpha1.ResourceStatus{{Group: group, Kind: kind, Name: name, Hook: hook}}}
//	}
//	tests := []struct {
//		name   string
//		fields fields
//		want   bool
//	}{
//		{"Empty", fields{}, false},
//		{"OneCompareResult", fields{oneResource("", "", "", false), []SyncOperationResource{}}, true},
//		{"OneSyncResource", fields{&controller.comparisonResult{}, oneSyncResource}, true},
//		{"Equal", fields{oneResource("", "", "", false), oneSyncResource}, false},
//		{"EqualOutOfOrder", fields{&controller.comparisonResult{resources: []v1alpha1.ResourceStatus{{Group: "a"}, {Group: "b"}}}, []SyncOperationResource{{Group: "b"}, {Group: "a"}}}, false},
//		{"KindDifferent", fields{oneResource("foo", "", "", false), oneSyncResource}, true},
//		{"GroupDifferent", fields{oneResource("", "foo", "", false), oneSyncResource}, true},
//		{"NameDifferent", fields{oneResource("", "", "foo", false), oneSyncResource}, true},
//		{"HookIgnored", fields{oneResource("", "", "", true), []SyncOperationResource{}}, false},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			sc := &syncContext{
//				compareResult: tt.fields.compareResult,
//				syncResources: tt.fields.syncResources,
//			}
//			if got := sc.isSelectiveSync(); got != tt.want {
//				t.Errorf("syncContext.isSelectiveSync() = %v, want %v", got, tt.want)
//			}
//		})
//	}
//}
//
func Test_syncContext_liveObj(t *testing.T) {
	type fields struct {
		compareResult ReconciliationResult
	}
	type args struct {
		obj *unstructured.Unstructured
	}
	obj := test.NewPod()
	obj.SetNamespace("my-ns")

	found := test.NewPod()
	foundNoNamespace := test.NewPod()
	foundNoNamespace.SetNamespace("")

	tests := []struct {
		name   string
		fields fields
		args   args
		want   *unstructured.Unstructured
	}{
		{"None", fields{compareResult: ReconciliationResult{}}, args{obj: &unstructured.Unstructured{}}, nil},
		{"Found", fields{compareResult: ReconciliationResult{Target: []*unstructured.Unstructured{nil}, Live: []*unstructured.Unstructured{found}}}, args{obj: obj}, found},
		{"EmptyNamespace", fields{compareResult: ReconciliationResult{Target: []*unstructured.Unstructured{nil}, Live: []*unstructured.Unstructured{foundNoNamespace}}}, args{obj: obj}, found},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &syncContext{
				resources: groupResources(tt.fields.compareResult),
				hooks:     tt.fields.compareResult.Hooks,
			}
			if got := sc.liveObj(tt.args.obj); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("syncContext.liveObj() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_syncContext_hasCRDOfGroupKind(t *testing.T) {
	// target
	assert.False(t, (&syncContext{resources: groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil},
		Target: []*unstructured.Unstructured{test.NewCRD()},
	})}).hasCRDOfGroupKind("", ""))
	assert.True(t, (&syncContext{resources: groupResources(ReconciliationResult{
		Live:   []*unstructured.Unstructured{nil},
		Target: []*unstructured.Unstructured{test.NewCRD()},
	})}).hasCRDOfGroupKind("argoproj.io", "TestCrd"))

	// hook
	assert.False(t, (&syncContext{hooks: []*unstructured.Unstructured{test.NewCRD()}}).hasCRDOfGroupKind("", ""))
	assert.True(t, (&syncContext{hooks: []*unstructured.Unstructured{test.NewCRD()}}).hasCRDOfGroupKind("argoproj.io", "TestCrd"))
}
