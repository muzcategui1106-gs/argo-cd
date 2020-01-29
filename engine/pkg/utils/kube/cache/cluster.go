package cache

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/argoproj/argo-cd/controller/metrics"
	"github.com/argoproj/argo-cd/engine/pkg/utils/health"
	"github.com/argoproj/argo-cd/engine/pkg/utils/kube"
)

const (
	clusterSyncTimeout         = 24 * time.Hour
	watchResourcesRetryTimeout = 1 * time.Second
	ClusterRetryTimeout        = 10 * time.Second
)

type apiMeta struct {
	namespaced      bool
	resourceVersion string
	watchCancel     context.CancelFunc
}

type Settings struct {
	ResourceHealthOverride health.HealthOverride
	ResourcesFilter        kube.ResourceFilter
}

type EventHandlers struct {
	OnEvent                func(event watch.EventType, un *unstructured.Unstructured)
	OnPopulateResourceInfo func(un *unstructured.Unstructured, isRoot bool) (info interface{}, cacheManifest bool)
	OnResourceUpdated      func(newRes *Resource, oldRes *Resource, namespaceResources map[kube.ResourceKey]*Resource)
}

type ClusterCache interface {
	EnsureSynced() error
	GetServerVersion() string
	Invalidate(settingsCallback func(*rest.Config, []string, Settings) (*rest.Config, []string, Settings))
	GetNamespaceTopLevelResources(namespace string) map[kube.ResourceKey]*Resource
	IterateHierarchy(key kube.ResourceKey, action func(resource *Resource, namespaceResources map[kube.ResourceKey]*Resource))
	IsNamespaced(gk schema.GroupKind) bool
	GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error)
	GetClusterInfo() metrics.ClusterInfo
}

func NewClusterCache(settings Settings, config *rest.Config, namespaces []string, kubectl kube.Kubectl, handlers EventHandlers) *clusterCache {
	return &clusterCache{
		settings:   settings,
		apisMeta:   make(map[schema.GroupKind]*apiMeta),
		resources:  make(map[kube.ResourceKey]*Resource),
		nsIndex:    make(map[string]map[kube.ResourceKey]*Resource),
		config:     config,
		namespaces: namespaces,
		kubectl:    kubectl,
		syncTime:   nil,
		log:        log.WithField("server", config.Host),
		handlers:   handlers,
	}
}

type clusterCache struct {
	syncTime      *time.Time
	syncError     error
	apisMeta      map[schema.GroupKind]*apiMeta
	serverVersion string
	handlers      EventHandlers

	lock      sync.Mutex
	resources map[kube.ResourceKey]*Resource
	nsIndex   map[string]map[kube.ResourceKey]*Resource

	kubectl    kube.Kubectl
	log        *log.Entry
	config     *rest.Config
	namespaces []string
	settings   Settings
}

func (c *clusterCache) GetServerVersion() string {
	return c.serverVersion
}

func (c *clusterCache) replaceResourceCache(gk schema.GroupKind, resourceVersion string, objs []unstructured.Unstructured, ns string) {
	info, ok := c.apisMeta[gk]
	if ok {
		objByKey := make(map[kube.ResourceKey]*unstructured.Unstructured)
		for i := range objs {
			objByKey[kube.GetResourceKey(&objs[i])] = &objs[i]
		}

		// update existing nodes
		for i := range objs {
			obj := &objs[i]
			key := kube.GetResourceKey(&objs[i])
			c.onNodeUpdated(c.resources[key], obj)
		}

		for key := range c.resources {
			if key.Kind != gk.Kind || key.Group != gk.Group || ns != "" && key.Namespace != ns {
				continue
			}

			if _, ok := objByKey[key]; !ok {
				c.onNodeRemoved(key)
			}
		}
		info.resourceVersion = resourceVersion
	}
}

func isServiceAccountTokenSecret(un *unstructured.Unstructured) (bool, metav1.OwnerReference) {
	ref := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       kube.ServiceAccountKind,
	}
	if un.GetKind() != kube.SecretKind || un.GroupVersionKind().Group != "" {
		return false, ref
	}

	if typeVal, ok, err := unstructured.NestedString(un.Object, "type"); !ok || err != nil || typeVal != "kubernetes.io/service-account-token" {
		return false, ref
	}

	annotations := un.GetAnnotations()
	if annotations == nil {
		return false, ref
	}

	id, okId := annotations["kubernetes.io/service-account.uid"]
	name, okName := annotations["kubernetes.io/service-account.name"]
	if okId && okName {
		ref.Name = name
		ref.UID = types.UID(id)
	}
	return ref.Name != "" && ref.UID != "", ref
}

func (c *clusterCache) newResource(un *unstructured.Unstructured) *Resource {
	ownerRefs := un.GetOwnerReferences()
	// Special case for endpoint. Remove after https://github.com/kubernetes/kubernetes/issues/28483 is fixed
	if un.GroupVersionKind().Group == "" && un.GetKind() == kube.EndpointsKind && len(un.GetOwnerReferences()) == 0 {
		ownerRefs = append(ownerRefs, metav1.OwnerReference{
			Name:       un.GetName(),
			Kind:       kube.ServiceKind,
			APIVersion: "v1",
		})
	}

	// edge case. Consider auto-created service account tokens as a child of service account objects
	if yes, ref := isServiceAccountTokenSecret(un); yes {
		ownerRefs = append(ownerRefs, ref)
	}

	cacheManifest := false
	var info interface{}
	if c.handlers.OnPopulateResourceInfo != nil {
		info, cacheManifest = c.handlers.OnPopulateResourceInfo(un, len(ownerRefs) == 0)
	}
	resource := &Resource{
		ResourceVersion: un.GetResourceVersion(),
		Ref:             kube.GetObjectRef(un),
		OwnerRefs:       ownerRefs,
		Info:            info,
	}
	if cacheManifest {
		resource.Resource = un
	}

	return resource
}

func (c *clusterCache) setNode(n *Resource) {
	key := n.ResourceKey()
	c.resources[key] = n
	ns, ok := c.nsIndex[key.Namespace]
	if !ok {
		ns = make(map[kube.ResourceKey]*Resource)
		c.nsIndex[key.Namespace] = ns
	}
	ns[key] = n
}

func (c *clusterCache) Invalidate(settingsCallback func(*rest.Config, []string, Settings) (*rest.Config, []string, Settings)) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.syncTime = nil
	for i := range c.apisMeta {
		c.apisMeta[i].watchCancel()
	}
	if settingsCallback != nil {
		c.config, c.namespaces, c.settings = settingsCallback(c.config, c.namespaces, c.settings)
	}
	c.apisMeta = nil
}

func (c *clusterCache) synced() bool {
	if c.syncTime == nil {
		return false
	}
	if c.syncError != nil {
		return time.Now().Before(c.syncTime.Add(ClusterRetryTimeout))
	}
	return time.Now().Before(c.syncTime.Add(clusterSyncTimeout))
}

func (c *clusterCache) stopWatching(gk schema.GroupKind, ns string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if info, ok := c.apisMeta[gk]; ok {
		info.watchCancel()
		delete(c.apisMeta, gk)
		c.replaceResourceCache(gk, "", []unstructured.Unstructured{}, ns)
		log.Warnf("Stop watching %s not found on %s.", gk, c.config.Host)
	}
}

// startMissingWatches lists supported cluster resources and start watching for changes unless watch is already running
func (c *clusterCache) startMissingWatches() error {
	apis, err := c.kubectl.GetAPIResources(c.config, c.settings.ResourcesFilter)
	if err != nil {
		return err
	}
	client, err := c.kubectl.NewDynamicClient(c.config)
	if err != nil {
		return err
	}

	for i := range apis {
		api := apis[i]
		if _, ok := c.apisMeta[api.GroupKind]; !ok {
			ctx, cancel := context.WithCancel(context.Background())
			info := &apiMeta{namespaced: api.Meta.Namespaced, watchCancel: cancel}
			c.apisMeta[api.GroupKind] = info

			err = c.processApi(client, api, func(resClient dynamic.ResourceInterface, ns string) error {
				go c.watchEvents(ctx, api, info, resClient, ns)
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func runSynced(lock *sync.Mutex, action func() error) error {
	lock.Lock()
	defer lock.Unlock()
	return action()
}

func (c *clusterCache) watchEvents(ctx context.Context, api kube.APIResourceInfo, info *apiMeta, resClient dynamic.ResourceInterface, ns string) {
	kube.RetryUntilSucceed(func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
			}
		}()

		err = runSynced(&c.lock, func() error {
			if info.resourceVersion == "" {
				list, err := resClient.List(metav1.ListOptions{})
				if err != nil {
					return err
				}
				c.replaceResourceCache(api.GroupKind, list.GetResourceVersion(), list.Items, ns)
			}
			return nil
		})

		if err != nil {
			return err
		}

		w, err := resClient.Watch(metav1.ListOptions{ResourceVersion: info.resourceVersion})
		if errors.IsNotFound(err) {
			c.stopWatching(api.GroupKind, ns)
			return nil
		}

		err = runSynced(&c.lock, func() error {
			if errors.IsGone(err) {
				info.resourceVersion = ""
				log.Warnf("Resource version of %s on %s is too old.", api.GroupKind, c.config.Host)
			}
			return err
		})

		if err != nil {
			return err
		}
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-w.ResultChan():
				if ok {
					obj := event.Object.(*unstructured.Unstructured)
					info.resourceVersion = obj.GetResourceVersion()
					c.processEvent(event.Type, obj)
					if kube.IsCRD(obj) {
						if event.Type == watch.Deleted {
							group, groupOk, groupErr := unstructured.NestedString(obj.Object, "spec", "group")
							kind, kindOk, kindErr := unstructured.NestedString(obj.Object, "spec", "names", "kind")

							if groupOk && groupErr == nil && kindOk && kindErr == nil {
								gk := schema.GroupKind{Group: group, Kind: kind}
								c.stopWatching(gk, ns)
							}
						} else {
							err = runSynced(&c.lock, func() error {
								return c.startMissingWatches()
							})

						}
					}
					if err != nil {
						log.Warnf("Failed to start missing watch: %v", err)
					}
				} else {
					return fmt.Errorf("Watch %s on %s has closed", api.GroupKind, c.config.Host)
				}
			}
		}

	}, fmt.Sprintf("watch %s on %s", api.GroupKind, c.config.Host), ctx, watchResourcesRetryTimeout)
}

func (c *clusterCache) processApi(client dynamic.Interface, api kube.APIResourceInfo, callback func(resClient dynamic.ResourceInterface, ns string) error) error {
	resClient := client.Resource(api.GroupVersionResource)
	if len(c.namespaces) == 0 {
		return callback(resClient, "")
	}

	if !api.Meta.Namespaced {
		return nil
	}

	for _, ns := range c.namespaces {
		err := callback(resClient.Namespace(ns), ns)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *clusterCache) sync() (err error) {

	c.log.Info("Start syncing cluster")

	for i := range c.apisMeta {
		c.apisMeta[i].watchCancel()
	}
	c.apisMeta = make(map[schema.GroupKind]*apiMeta)
	c.resources = make(map[kube.ResourceKey]*Resource)
	version, err := c.kubectl.GetServerVersion(c.config)
	if err != nil {
		return err
	}
	c.serverVersion = version
	apis, err := c.kubectl.GetAPIResources(c.config, c.settings.ResourcesFilter)
	if err != nil {
		return err
	}
	client, err := c.kubectl.NewDynamicClient(c.config)
	if err != nil {
		return err
	}
	lock := sync.Mutex{}
	err = kube.RunAllAsync(len(apis), func(i int) error {
		return c.processApi(client, apis[i], func(resClient dynamic.ResourceInterface, _ string) error {
			list, err := resClient.List(metav1.ListOptions{})
			if err != nil {
				return err
			}

			lock.Lock()
			for i := range list.Items {
				c.setNode(c.newResource(&list.Items[i]))
			}
			lock.Unlock()
			return nil
		})
	})

	if err == nil {
		err = c.startMissingWatches()
	}

	if err != nil {
		log.Errorf("Failed to sync cluster %s: %v", c.config.Host, err)
		return err
	}

	c.log.Info("Cluster successfully synced")
	return nil
}

func (c *clusterCache) EnsureSynced() error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.synced() {
		return c.syncError
	}

	err := c.sync()
	syncTime := time.Now()
	c.syncTime = &syncTime
	c.syncError = err
	return c.syncError
}

func (c *clusterCache) GetNamespaceTopLevelResources(namespace string) map[kube.ResourceKey]*Resource {
	c.lock.Lock()
	defer c.lock.Unlock()
	resources := make(map[kube.ResourceKey]*Resource)
	for _, res := range c.nsIndex[namespace] {
		if len(res.OwnerRefs) == 0 {
			resources[res.ResourceKey()] = res
		}
	}
	return resources
}

func (c *clusterCache) IterateHierarchy(key kube.ResourceKey, action func(resource *Resource, namespaceResources map[kube.ResourceKey]*Resource)) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if res, ok := c.resources[key]; ok {
		nsNodes := c.nsIndex[key.Namespace]
		action(res, nsNodes)
		childrenByUID := make(map[types.UID][]*Resource)
		for _, child := range nsNodes {
			if res.isParentOf(child) {
				childrenByUID[child.Ref.UID] = append(childrenByUID[child.Ref.UID], child)
			}
		}
		// make sure children has no duplicates
		for _, children := range childrenByUID {
			if len(children) > 0 {
				// The object might have multiple children with the same UID (e.g. replicaset from apps and extensions group). It is ok to pick any object but we need to make sure
				// we pick the same child after every refresh.
				sort.Slice(children, func(i, j int) bool {
					key1 := children[i].ResourceKey()
					key2 := children[j].ResourceKey()
					return strings.Compare(key1.String(), key2.String()) < 0
				})
				child := children[0]
				action(child, nsNodes)
				child.iterateChildren(nsNodes, map[kube.ResourceKey]bool{res.ResourceKey(): true}, action)
			}
		}
	}
}

func (c *clusterCache) IsNamespaced(gk schema.GroupKind) bool {
	if api, ok := c.apisMeta[gk]; ok && !api.namespaced {
		return false
	}
	return true
}

func (c *clusterCache) GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	managedObjs := make(map[kube.ResourceKey]*unstructured.Unstructured)
	// iterate all objects in live state cache to find ones associated with app
	for key, o := range c.resources {
		if isManaged(o) && o.Resource != nil && len(o.OwnerRefs) == 0 {
			managedObjs[key] = o.Resource
		}
	}
	// iterate target objects and identify ones that already exist in the cluster,\
	// but are simply missing our label
	lock := &sync.Mutex{}
	err := kube.RunAllAsync(len(targetObjs), func(i int) error {
		targetObj := targetObjs[i]
		key := kube.GetResourceKey(targetObj)
		lock.Lock()
		managedObj := managedObjs[key]
		lock.Unlock()

		if managedObj == nil {
			if existingObj, exists := c.resources[key]; exists {
				if existingObj.Resource != nil {
					managedObj = existingObj.Resource
				} else {
					var err error
					managedObj, err = c.kubectl.GetResource(c.config, targetObj.GroupVersionKind(), existingObj.Ref.Name, existingObj.Ref.Namespace)
					if err != nil {
						if errors.IsNotFound(err) {
							return nil
						}
						return err
					}
				}
			} else if _, watched := c.apisMeta[key.GroupKind()]; !watched {
				var err error
				managedObj, err = c.kubectl.GetResource(c.config, targetObj.GroupVersionKind(), targetObj.GetName(), targetObj.GetNamespace())
				if err != nil {
					if errors.IsNotFound(err) {
						return nil
					}
					return err
				}
			}
		}

		if managedObj != nil {
			converted, err := c.kubectl.ConvertToVersion(managedObj, targetObj.GroupVersionKind().Group, targetObj.GroupVersionKind().Version)
			if err != nil {
				// fallback to loading resource from kubernetes if conversion fails
				log.Warnf("Failed to convert resource: %v", err)
				managedObj, err = c.kubectl.GetResource(c.config, targetObj.GroupVersionKind(), managedObj.GetName(), managedObj.GetNamespace())
				if err != nil {
					if errors.IsNotFound(err) {
						return nil
					}
					return err
				}
			} else {
				managedObj = converted
			}
			lock.Lock()
			managedObjs[key] = managedObj
			lock.Unlock()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return managedObjs, nil
}

func (c *clusterCache) processEvent(event watch.EventType, un *unstructured.Unstructured) {
	if c.handlers.OnEvent != nil {
		c.handlers.OnEvent(event, un)
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	key := kube.GetResourceKey(un)
	existingNode, exists := c.resources[key]
	if event == watch.Deleted {
		if exists {
			c.onNodeRemoved(key)
		}
	} else if event != watch.Deleted {
		c.onNodeUpdated(existingNode, un)
	}
}

func (c *clusterCache) onNodeUpdated(oldRes *Resource, un *unstructured.Unstructured) {
	newRes := c.newResource(un)
	c.setNode(newRes)
	if c.handlers.OnResourceUpdated != nil {
		c.handlers.OnResourceUpdated(newRes, oldRes, c.nsIndex[newRes.Ref.Namespace])
	}
}

func (c *clusterCache) onNodeRemoved(key kube.ResourceKey) {
	existing, ok := c.resources[key]
	if ok {
		delete(c.resources, key)
		ns, ok := c.nsIndex[key.Namespace]
		if ok {
			delete(ns, key)
			if len(ns) == 0 {
				delete(c.nsIndex, key.Namespace)
			}
		}
		if c.handlers.OnResourceUpdated != nil {
			c.handlers.OnResourceUpdated(nil, existing, ns)
		}
	}
}

func (c *clusterCache) GetClusterInfo() metrics.ClusterInfo {
	c.lock.Lock()
	defer c.lock.Unlock()
	return metrics.ClusterInfo{
		APIsCount:         len(c.apisMeta),
		K8SVersion:        c.serverVersion,
		ResourcesCount:    len(c.resources),
		Server:            c.config.Host,
		LastCacheSyncTime: c.syncTime,
	}
}
