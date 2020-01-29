package common

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/argoproj/argo-cd/engine/pkg/utils/kube"
)

type PermissionValidator func(un *unstructured.Unstructured, res *metav1.APIResource) error

type SyncPhase string

const (
	SyncPhasePreSync  = "PreSync"
	SyncPhaseSync     = "Sync"
	SyncPhasePostSync = "PostSync"
	SyncPhaseSyncFail = "SyncFail"
)

type OperationPhase string

const (
	OperationRunning     OperationPhase = "Running"
	OperationTerminating OperationPhase = "Terminating"
	OperationFailed      OperationPhase = "Failed"
	OperationError       OperationPhase = "Error"
	OperationSucceeded   OperationPhase = "Succeeded"
)

func (os OperationPhase) Completed() bool {
	switch os {
	case OperationFailed, OperationError, OperationSucceeded:
		return true
	}
	return false
}

func (os OperationPhase) Running() bool {
	return os == OperationRunning
}

func (os OperationPhase) Successful() bool {
	return os == OperationSucceeded
}

func (os OperationPhase) Failed() bool {
	return os == OperationFailed
}

type ResultCode string

const (
	ResultCodeSynced       ResultCode = "Synced"
	ResultCodeSyncFailed   ResultCode = "SyncFailed"
	ResultCodePruned       ResultCode = "Pruned"
	ResultCodePruneSkipped ResultCode = "PruneSkipped"
)

type HookType string

const (
	HookTypePreSync  HookType = "PreSync"
	HookTypeSync     HookType = "Sync"
	HookTypePostSync HookType = "PostSync"
	HookTypeSkip     HookType = "Skip"
	HookTypeSyncFail HookType = "SyncFail"
)

func NewHookType(t string) (HookType, bool) {
	return HookType(t),
		t == string(HookTypePreSync) ||
			t == string(HookTypeSync) ||
			t == string(HookTypePostSync) ||
			t == string(HookTypeSyncFail) ||
			t == string(HookTypeSkip)

}

type HookDeletePolicy string

const (
	HookDeletePolicyHookSucceeded      HookDeletePolicy = "HookSucceeded"
	HookDeletePolicyHookFailed         HookDeletePolicy = "HookFailed"
	HookDeletePolicyBeforeHookCreation HookDeletePolicy = "BeforeHookCreation"
)

func NewHookDeletePolicy(p string) (HookDeletePolicy, bool) {
	return HookDeletePolicy(p),
		p == string(HookDeletePolicyHookSucceeded) ||
			p == string(HookDeletePolicyHookFailed) ||
			p == string(HookDeletePolicyBeforeHookCreation)
}

type ResourceSyncResult struct {
	// holds associated resource key
	ResourceKey kube.ResourceKey
	// holds resource version
	Version string
	// holds the execution order
	Order int
	// result code
	Status ResultCode
	// message for the last sync OR operation
	Message string
	// the type of the hook, empty for non-hook resources
	HookType HookType
	// the state of any operation associated with this resource OR hook
	// note: can contain values for non-hook resources
	HookPhase OperationPhase
	// indicates the particular phase of the sync that this is for
	SyncPhase SyncPhase
}
