package operator

import (
	"fmt"

	"github.com/Sirupsen/logrus"
	componentsv1alpha1 "github.com/dansksupermarked/mariadb-galera-operator/pkg/apis/components/v1alpha1"
	componentinformers "github.com/dansksupermarked/mariadb-galera-operator/pkg/generated/informers/externalversions"
	listers "github.com/dansksupermarked/mariadb-galera-operator/pkg/generated/listers/components/v1alpha1"
	"github.com/dansksupermarked/mariadb-galera-operator/pkg/util"
	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type cluster struct {
	name string
}

type Controller struct {
	operator *Operator
	clusters map[string]*cluster

	// Listers and informers for objects on changes to which controller will react
	configmapLister       corelisters.ConfigMapLister
	configmapSynced       cache.InformerSynced
	statefulsetLister     appslisters.StatefulSetLister
	statefulsetSynced     cache.InformerSynced
	mariadbclustersLister listers.MariaDBClusterLister
	mariadbclustersSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	stopChan  chan struct{}
}

func NewController(op *Operator, kubeInformerFactory informers.SharedInformerFactory, componentsInformerFactory componentinformers.SharedInformerFactory) *Controller {
	statefulsetInformer := kubeInformerFactory.Apps().V1().StatefulSets()
	configmapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
	mariaInformer := componentsInformerFactory.Components().V1alpha1().MariaDBClusters()
	c := &Controller{
		operator:              op,
		configmapLister:       configmapInformer.Lister(),
		configmapSynced:       configmapInformer.Informer().HasSynced,
		statefulsetLister:     statefulsetInformer.Lister(),
		statefulsetSynced:     statefulsetInformer.Informer().HasSynced,
		mariadbclustersLister: mariaInformer.Lister(),
		mariadbclustersSynced: mariaInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "MariaDBClusters"),
	}

	logrus.Info("Adding event handlers for MariaDBClusters informer")
	mariaInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.MariaDBClusterAddEventHandler,
			UpdateFunc: c.MariaDBClusterUpdateEventHandler,
			DeleteFunc: c.MariaDBClusterDeleteEventHandler,
		})

	logrus.Info("Adding event handlers for StatefulSet informer")
	statefulsetInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.StatefulSetAddEventHandler,
			UpdateFunc: c.StatefulSetUpdateEventHandler,
			DeleteFunc: c.StatefulSetDeleteEventHandler,
		})

	return c
}

func (c *Controller) WaitForCacheSync() {
	if ok := cache.WaitForCacheSync(c.stopChan, c.statefulsetSynced, c.configmapSynced, c.mariadbclustersSynced); !ok {
		panic("Failed to sync cache")
	}
}

func (c *Controller) MariaDBClusterEnqueue(obj interface{}) error {
	mdb := obj.(*componentsv1alpha1.MariaDBCluster)
	logrus.WithFields(logrus.Fields{"cluster": mdb.Namespace + "/" + mdb.Name}).Debugf("Adding MariaDBCluster to workqueue")
	key, err := cache.MetaNamespaceKeyFunc(obj)
	c.workqueue.AddRateLimited(key)
	return err
}

func (c *Controller) processNextFromQueue() error {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return nil
	}
	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		return nil
	}(obj)
	if err != nil {
		return fmt.Errorf("Ooops somthing failed processing a work item")
	}
	return nil
}

func (c *Controller) syncHandler(key string) error {
	logrus.Debugf("Controller.syncHandler called with %s", key)
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Cluster resource with this namespace/name
	cluster, err := c.mariadbclustersLister.MariaDBClusters(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("Cluster '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	c.reconcileCluster(cluster)
	return nil
}

func (c *Controller) noConflictingResources(cluster *componentsv1alpha1.MariaDBCluster) bool {
	var resources string
	var err error
	_, err = c.statefulsetLister.StatefulSets(cluster.Namespace).Get(cluster.Name)
	if !errors.IsNotFound(err) {
		resources = resources + " StatefulSet"
	}
	_, err = c.configmapLister.ConfigMaps(cluster.Namespace).Get(cluster.Name)
	if !errors.IsNotFound(err) {
		resources = resources + " ConfigMap"
	}

	if resources == "" {
		return true
	} else {
		logrus.Debugf("Found conflicting resources : %s", resources)
		return false
	}
}

func (c *Controller) reconcileCluster(cluster *componentsv1alpha1.MariaDBCluster) {
	c.reconcileMariaDBCluster(cluster)
	pvc := cluster.GetSnapshotPVC()
	reconcile(c.operator.Client.CoreV1(), cluster, pvc)
	c.operator.reconcileServerServiceAccount(cluster)
	c.operator.reconcileServerRole(cluster)
	c.operator.reconcileServerRoleBinding(cluster)
	// c.operator.reconcileServerConfigMap(cluster)
	c.operator.reconcileStatefulSet(cluster)
	c.operator.reconcileServerService(cluster)
	c.operator.reconcileProxyService(cluster)
}

type Patch []PatchSpec

type PatchSpec struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

func (c *Controller) syncWorker() {
	for {
		c.processNextFromQueue()
	}
}

func (c *Controller) Run() {
	c.WaitForCacheSync()
	go c.syncWorker()
}

// check if any criteria for state transition are met
func (c *Controller) MariaDBClusterTransform(mdbc *componentsv1alpha1.MariaDBCluster) error {
	logger := logrus.WithField("kind", "MariaDBCluster")
	logger.Debug("Detected " + mdbc.Status.Phase + " Phase, checking transitions")
	// Start cluster bootstrap if phase is empty
	switch mdbc.Status.Phase {

	case "":
		mdbc.Status.Phase = componentsv1alpha1.PhasePreFlight

	case componentsv1alpha1.PhasePreFlight:
		// TODO : implement preflight checks verifying the definition of cluster, naming collisions etc.
		mdbc.Status.Phase = componentsv1alpha1.PhaseBootstrapFirst

	// First phase of bootstrap, starting the cluster with --wsrep-cluster-new
	case componentsv1alpha1.PhaseBootstrapFirst:
		sset, err := c.statefulsetLister.StatefulSets(mdbc.Namespace).Get(mdbc.GetServerName())
		if err == nil {
			if mdbc.Spec.Replicas > 1 &&
				isStatefulSetReady(sset) {
				logger.WithField("event", "phaseTransition").Info("Transitioning to BootstrapFirstRestart phase")
				mdbc.Status.Phase = componentsv1alpha1.PhaseBootstrapFirstRestart
				mdbc.Status.StatefulSetObservedGeneration = sset.Status.ObservedGeneration
			}
		}

	// Restart loosing --wsrep-cluster-new so we do not wipe cluster IP
	// TODO : move this phase into initialiser internal logic
	case componentsv1alpha1.PhaseBootstrapFirstRestart:
		sset, _ := c.statefulsetLister.StatefulSets(mdbc.Namespace).Get(mdbc.GetServerName())
		if mdbc.Spec.Replicas > 1 &&
			isStatefulSetUpdated(mdbc, sset) &&
			isStatefulSetReady(sset) {
			logger.WithField("event", "phaseTransition").Info("Transitioning to BootstrapSecond phase")
			mdbc.Status.Phase = componentsv1alpha1.PhaseBootstrapSecond
			mdbc.Status.StatefulSetObservedGeneration = sset.Status.ObservedGeneration
		}

	// Bootstrap second node of galera cluster
	case componentsv1alpha1.PhaseBootstrapSecond:
		sset, _ := c.statefulsetLister.StatefulSets(mdbc.Namespace).Get(mdbc.GetServerName())
		if mdbc.Spec.Replicas > 2 &&
			isStatefulSetUpdated(mdbc, sset) &&
			isStatefulSetReady(sset) {
			logger.WithField("event", "phaseTransition").Info("Transitioning to BootstrapSecond phase")
			mdbc.Status.Phase = componentsv1alpha1.PhaseBootstrapThird
			mdbc.Status.StatefulSetObservedGeneration = sset.Status.ObservedGeneration
		}

	// Bootstrap third node of galera cluster
	case componentsv1alpha1.PhaseBootstrapThird:
		sset, _ := c.statefulsetLister.StatefulSets(mdbc.Namespace).Get(mdbc.GetServerName())
		if mdbc.Spec.Replicas > 2 &&
			isStatefulSetUpdated(mdbc, sset) &&
			isStatefulSetReady(sset) {
			logger.WithField("event", "phaseTransition").Info("Transitioning to BootstrapSecond phase")
			mdbc.Status.Phase = componentsv1alpha1.PhaseOperational
			mdbc.Status.StatefulSetObservedGeneration = sset.Status.ObservedGeneration
		}
		// Detect unhealthy state
	case componentsv1alpha1.PhaseOperational:
		sset, _ := c.statefulsetLister.StatefulSets(mdbc.Namespace).Get(mdbc.GetServerName())
		if sset.Status.ReadyReplicas == 0 {
			mdbc.Status.Phase = componentsv1alpha1.PhaseRecovery
		} else if isStatefulSetReady(sset) {
			mdbc.Status.Stage = componentsv1alpha1.StageSynced
		}

	case componentsv1alpha1.PhaseRecovery:
		// A bootstrap pod has been indicated, parse status of the pod to verify
		// if it bootstrapped successfully (indicated by readiness probe success)
		// when successfull, transition to PrimaryRecovered stage of Recovery Phase
		if mdbc.Status.BootstrapFrom != "" {
			pod, err := c.operator.Client.Core().Pods(mdbc.Namespace).Get(mdbc.Status.BootstrapFrom, metav1.GetOptions{})
			if err != nil {
				return err
			}
			var ready bool
			ready = true
			for _, status := range pod.Status.ContainerStatuses {
				if !status.Ready {
					ready = false
				}
			}
			if ready {
				// Bootstrap pod is alive and ready, remove bootstrap indicator and start joining others
				mdbc.Status.Stage = componentsv1alpha1.StagePrimaryRecovered
				mdbc.Status.BootstrapFrom = ""
			}
			return nil
		}

		// Transition to operational if Primary Component is recovered
		// so that other galera cluster nodes can join new primary
		if mdbc.Status.Stage == componentsv1alpha1.StagePrimaryRecovered {
			sset, err := c.statefulsetLister.StatefulSets(mdbc.Namespace).Get(mdbc.GetServerName())
			if err != nil {
				logger.Error(err.Error())
			}
		        if sset.Status.ReadyReplicas == 0 {
			        mdbc.Status.Phase = componentsv1alpha1.PhaseRecovery
				mdbc.Status.Stage = ""

		        } else if isStatefulSetReady(sset) {
				mdbc.Status.Phase = componentsv1alpha1.PhaseOperational
				mdbc.Status.Stage = componentsv1alpha1.StageDegraded
				mdbc.Status.StatefulSetPodConditions = nil
				mdbc.Status.BootstrapFrom = ""
			}
			return nil
		}

		// Check if all pods reported their conditions and select the most advanced one
		reported := int32(len(mdbc.Status.StatefulSetPodConditions))
		if mdbc.Spec.Replicas > 1 {
			if reported == mdbc.Spec.Replicas {
				var maxSeqNoHostname string
				var maxSeqNoValue, minSeqNoValue int64
				maxSeqNoValue = -1
				minSeqNoValue = -1
				for _, v := range mdbc.Status.StatefulSetPodConditions {
					if v.GRAState.SeqNo > maxSeqNoValue {
						maxSeqNoValue = v.GRAState.SeqNo
						maxSeqNoHostname = v.Hostname
					} else {
						minSeqNoValue = v.GRAState.SeqNo
					}
				}
				// Select bootstrap node only if all nodes reported positive values
				// to avoid risk of missing out on the most advanced node
				if minSeqNoValue > 0 && maxSeqNoValue > 0 {
					mdbc.Status.BootstrapFrom = maxSeqNoHostname
				} else {
					mdbc.Status.Stage = componentsv1alpha1.StageInvalidReport
				}
			}
		}
	}
	return nil
}

func isStatefulSetUpdated(mdbc *componentsv1alpha1.MariaDBCluster, sset *apps.StatefulSet) bool {
	return sset.Status.ObservedGeneration > mdbc.Status.StatefulSetObservedGeneration
}

func isStatefulSetReady(sset *apps.StatefulSet) bool {
	return *sset.Spec.Replicas == sset.Status.CurrentReplicas &&
		*sset.Spec.Replicas == sset.Status.Replicas &&
		*sset.Spec.Replicas == sset.Status.ReadyReplicas &&
		sset.Status.CurrentRevision == sset.Status.UpdateRevision
}

func (c *Controller) reconcileMariaDBCluster(mdbc *componentsv1alpha1.MariaDBCluster) error {
	logger := util.GetClusterLogger(mdbc).WithField("kind", "MariaDBCluster").WithField("action", "reconcile")
	logger.WithField("event", "started").Debug()
	defer logger.WithField("event", "finished").Debug()
	original := mdbc.DeepCopy()
	c.MariaDBClusterTransform(mdbc)
	checkAndPatchMariaDBCluster(original, mdbc, c.operator.ComponentsClient.Components(), logger)
	return nil
}
