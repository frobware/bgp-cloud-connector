/*
Copyright 2026.

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
	"errors"
	"fmt"
	"strings"
	"time"

	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
	gcpplatform "github.com/openshift/bgp-cloud-connector/internal/platform/gcp"
)

// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgpconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgpconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgpconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgproutings,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=networks,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=frrk8s.metallb.io,resources=frrconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=infrastructures,verbs=get
// +kubebuilder:rbac:groups=machine.openshift.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

type PlatformBuilderFunc func(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error)

type CUDNBgpConfigReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	PlatformBuilder PlatformBuilderFunc
	// Recorder announces condition changes. Nil is tolerated so a unit test
	// that does not care about events need not supply one.
	Recorder record.EventRecorder
}

func (r *CUDNBgpConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	config := &networkingv1alpha1.CUDNBgpConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	baselineStatus := config.Status.DeepCopy()

	if config.Name != SingletonName {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionNetworkOperatorPatched,
			"InvalidName", fmt.Sprintf("CUDNBgpConfig must be named %q, got %q", SingletonName, config.Name))
	}

	if !config.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, config)
	}

	if !controllerutil.ContainsFinalizer(config, ConfigFinalizerName) {
		controllerutil.AddFinalizer(config, ConfigFinalizerName)
		if err := r.Update(ctx, config); err != nil {
			return ctrl.Result{}, err
		}
	}

	config.Status.Phase = networkingv1alpha1.PhaseConfiguring
	config.Status.ObservedGeneration = config.Generation

	// Phase 1: Patch Network Operator
	log.Info("Phase 1: patching Network operator")
	if err := PatchNetworkOperator(ctx, r.Client); err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionNetworkOperatorPatched,
			"PatchFailed", fmt.Sprintf("failed to patch Network operator: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionNetworkOperatorPatched,
		Status:             metav1.ConditionTrue,
		Reason:             "Patched",
		Message:            "Network operator patched with FRR and routeAdvertisements",
		ObservedGeneration: config.Generation,
	})

	// Phase 2: Wait for FRR
	log.Info("Phase 2: checking FRR readiness")
	ready, err := IsFRRReady(ctx, r.Client)
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionFRRNamespaceReady,
			"CheckFailed", fmt.Sprintf("failed to check FRR readiness: %v", err))
	}
	if !ready {
		if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
			c.Status.Phase = networkingv1alpha1.PhaseConfiguring
			c.Status.ObservedGeneration = c.Generation
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               networkingv1alpha1.ConditionFRRNamespaceReady,
				Status:             metav1.ConditionFalse,
				Reason:             "WaitingForFRR",
				Message:            "Waiting for openshift-frr-k8s namespace and pods",
				ObservedGeneration: c.Generation,
			})
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("FRR not ready, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionFRRNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "FRR namespace and pods are running",
		ObservedGeneration: config.Generation,
	})

	// Phase 3: Settle which nodes are BGP routers. Everything downstream
	// selects on those labels, so this runs before discovery and before the
	// FRR configurations are rendered.
	if config.Spec.AutoLabelRouterNodes != nil {
		labelled, unlabelled, err := SyncRouterLabels(ctx, r.Client, config)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionRouterNodesLabelled,
				"LabelSyncFailed", fmt.Sprintf("failed to sync router node labels: %v", err))
		}
		if labelled > 0 || unlabelled > 0 {
			log.Info("router node labels synced", "labelled", labelled, "unlabelled", unlabelled)
		}
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionRouterNodesLabelled,
			Status:             metav1.ConditionTrue,
			Reason:             "Synced",
			Message:            "Router node labels are in sync with spec.autoLabelRouterNodes",
			ObservedGeneration: config.Generation,
		})
	}

	// Build the cloud platform once if this configuration uses one (Phases 4 and 6).
	var cloud platform.CloudPlatform
	var discoveryResult *platform.DiscoveryResult
	var unmetPrerequisites []string
	if config.Spec.Platform != networkingv1alpha1.PlatformManual {
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			var credErr *platform.CredentialError
			if errors.As(err, &credErr) {
				return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
					"CloudCredentialsInvalid", credErr.Error())
			}
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
				"CloudDiscoveryFailed", fmt.Sprintf("failed to build %s platform: %v", config.Spec.Platform, err))
		}
		cloud = p

		// Prerequisites the operator relies on but does not create. It keeps
		// reconciling either way, so that fixing one is enough and no second
		// action is needed, but it will not claim Ready while the path cannot
		// work.
		unmetPrerequisites, err = cloud.CheckPrerequisites(ctx)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionPrerequisitesSatisfied,
				"CheckFailed", fmt.Sprintf("failed to check cloud prerequisites: %v", err))
		}
		if len(unmetPrerequisites) > 0 {
			log.Info("cloud prerequisites unmet", "count", len(unmetPrerequisites))
			meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
				Type:               networkingv1alpha1.ConditionPrerequisitesSatisfied,
				Status:             metav1.ConditionFalse,
				Reason:             "Unmet",
				Message:            strings.Join(unmetPrerequisites, "; "),
				ObservedGeneration: config.Generation,
			})
		} else {
			meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
				Type:               networkingv1alpha1.ConditionPrerequisitesSatisfied,
				Status:             metav1.ConditionTrue,
				Reason:             "Satisfied",
				Message:            "Cloud prerequisites are in place",
				ObservedGeneration: config.Generation,
			})
		}

		// Phase 4: Discover cloud BGP endpoints
		log.Info("Phase 4: discovering cloud BGP endpoints", "platform", config.Spec.Platform)
		discoveryResult, err = cloud.DiscoverEndpoints(ctx)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
				"CloudDiscoveryFailed", fmt.Sprintf("failed to discover cloud BGP endpoints: %v", err))
		}
		if config.Spec.Platform == networkingv1alpha1.PlatformAWS {
			config.Status.AWS = discoveryResultToStatus(discoveryResult)
		}
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionCloudEndpointsDiscovered,
			Status:             metav1.ConditionTrue,
			Reason:             "Discovered",
			Message:            fmt.Sprintf("Discovered %d peer group(s)", len(discoveryResult.PeerGroups)),
			ObservedGeneration: config.Generation,
		})
	}

	// Phase 5: Apply FRR Configuration per peer group
	log.Info("Phase 5: applying FRR configurations")
	var frrCount int
	if discoveryResult != nil {
		frrCount, err = EnsureFRRConfigurationsFromGroups(ctx, r.Client, config, discoveryResult.PeerGroups)
	} else {
		frrCount = len(config.Spec.BGP.AvailabilityZones)
		err = EnsureFRRConfigurations(ctx, r.Client, config)
	}
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionFRRConfigurationApplied,
			"ApplyFailed", fmt.Sprintf("failed to apply FRR configurations: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionFRRConfigurationApplied,
		Status:             metav1.ConditionTrue,
		Reason:             "Applied",
		Message:            fmt.Sprintf("Applied %d FRRConfiguration(s)", frrCount),
		ObservedGeneration: config.Generation,
	})

	// Phase 6: Reconcile cloud resources (if a platform is configured)
	if cloud != nil {
		log.Info("Phase 6: reconciling cloud resources", "platform", config.Spec.Platform)
		nodes, err := r.listRouterNodes(ctx, config)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudResourcesReconciled,
				"CloudReconcileFailed", fmt.Sprintf("failed to list router nodes: %v", err))
		}
		// Nodes whose Machine is terminating are held and excluded, so that
		// reconciliation tears their peers down before the instance goes away.
		// Platforms that do not implement NodeLifecycle hold nothing.
		lifecycle, hasLifecycle := cloud.(platform.NodeLifecycle)
		var held []platform.RouterNode
		if hasLifecycle {
			held, err = lifecycle.HoldTerminating(ctx, nodes)
			if err != nil {
				return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudResourcesReconciled,
					"CloudReconcileFailed", fmt.Sprintf("failed to hold terminating nodes: %v", err))
			}
			nodes = excludeNodes(nodes, held)
		}

		if err := cloud.ReconcileNodes(ctx, nodes); err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudResourcesReconciled,
				"CloudReconcileFailed", fmt.Sprintf("failed to reconcile cloud resources: %v", err))
		}

		if len(held) > 0 {
			if err := lifecycle.ReleaseTerminating(ctx, held); err != nil {
				return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudResourcesReconciled,
					"CloudReconcileFailed", fmt.Sprintf("failed to release terminating nodes: %v", err))
			}
			log.Info("released terminating router nodes", "count", len(held))
		}

		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionCloudResourcesReconciled,
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            fmt.Sprintf("Reconciled cloud resources for %d router node(s)", len(nodes)),
			ObservedGeneration: config.Generation,
		})
	}

	// Everything the operator owns is reconciled. Ready is still withheld
	// while a prerequisite is missing, because reporting Ready for work we
	// did rather than for a path that functions is how a cluster ends up
	// green with no route to a pod.
	finalPhase := networkingv1alpha1.PhaseReady
	if len(unmetPrerequisites) > 0 {
		finalPhase = networkingv1alpha1.PhaseDegraded
	}
	if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.Phase = finalPhase
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciliation complete", "phase", config.Status.Phase)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func discoveryResultToStatus(dr *platform.DiscoveryResult) *networkingv1alpha1.AWSStatus {
	status := &networkingv1alpha1.AWSStatus{}
	for _, rs := range dr.RouteServers {
		drs := networkingv1alpha1.DiscoveredRouteServer{
			RouteServerID: rs.RouteServerID,
			RemoteASN:     rs.RemoteASN,
		}
		for _, ep := range rs.Endpoints {
			drs.Endpoints = append(drs.Endpoints, networkingv1alpha1.DiscoveredEndpoint{
				EndpointID:       ep.EndpointID,
				AvailabilityZone: ep.AZ,
				Address:          ep.Address,
			})
		}
		status.RouteServers = append(status.RouteServers, drs)
	}
	return status
}

// excludeNodes returns nodes with everything in remove filtered out, matching
// on node name.
func excludeNodes(nodes, remove []platform.RouterNode) []platform.RouterNode {
	if len(remove) == 0 {
		return nodes
	}
	excluded := make(map[string]struct{}, len(remove))
	for _, n := range remove {
		excluded[n.Name] = struct{}{}
	}
	kept := make([]platform.RouterNode, 0, len(nodes))
	for _, n := range nodes {
		if _, skip := excluded[n.Name]; !skip {
			kept = append(kept, n)
		}
	}
	return kept
}

// defaultPlatformBuilder constructs the cloud platform named by
// spec.platform. PlatformManual never reaches here: the controller skips
// platform construction entirely for it.
func defaultPlatformBuilder(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
	switch config.Spec.Platform {
	case networkingv1alpha1.PlatformAWS:
		return buildAWSPlatform(ctx, c, config)
	case networkingv1alpha1.PlatformGCP:
		return buildGCPPlatform(ctx, c, config)
	default:
		return nil, fmt.Errorf("no platform implementation for %q", config.Spec.Platform)
	}
}

func buildAWSPlatform(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
	awsSpec := config.Spec.AWS

	clusterID, err := getInfrastructureName(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading cluster infrastructure name: %w", err)
	}

	cfg := awsplatform.Config{
		Region:            awsSpec.Region,
		RouteServerIDs:    awsSpec.RouteServerIDs,
		LocalASN:          config.Spec.BGP.LocalASN,
		LivenessDetection: string(config.Spec.BGP.LivenessDetection),
		ClusterID:         clusterID,
	}

	return awsplatform.New(ctx, cfg)
}

func buildGCPPlatform(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
	gcpSpec := config.Spec.GCP

	clusterID, err := getInfrastructureName(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading cluster infrastructure name: %w", err)
	}

	return gcpplatform.New(ctx, gcpplatform.Config{
		Project:          gcpSpec.Project,
		Region:           gcpSpec.Region,
		CloudRouterName:  gcpSpec.CloudRouterName,
		NCCHubName:       gcpSpec.NCC.HubName,
		NCCSpokePrefix:   gcpSpec.NCC.SpokePrefix,
		SiteToSite:       gcpSpec.NCC.SiteToSiteDataTransfer,
		NestedVirt:       gcpSpec.IsNestedVirtEnabled(),
		MachineNamespace: gcpSpec.MachineNamespace,
		LocalASN:         config.Spec.BGP.LocalASN,
		ClusterID:        clusterID,
	}, c)
}

func getInfrastructureName(ctx context.Context, c client.Client) (string, error) {
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "Infrastructure",
	})
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, infra); err != nil {
		return "", err
	}
	name, found, err := unstructured.NestedString(infra.Object, "status", "infrastructureName")
	if err != nil || !found || name == "" {
		return "", fmt.Errorf("status.infrastructureName not set on Infrastructure/cluster")
	}
	return name, nil
}

func (r *CUDNBgpConfigReconciler) listRouterNodes(ctx context.Context, config *networkingv1alpha1.CUDNBgpConfig) ([]platform.RouterNode, error) {
	nodeList := &corev1.NodeList{}
	sel := labels.SelectorFromSet(config.Spec.RouterNodeSelector)
	if err := r.List(ctx, nodeList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}

	nodes := make([]platform.RouterNode, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		rn := platform.RouterNode{
			Name:       node.Name,
			ProviderID: node.Spec.ProviderID,
			AZ:         node.Labels["topology.kubernetes.io/zone"],
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				rn.PrivateIP = addr.Address
				break
			}
		}
		if rn.PrivateIP == "" || rn.AZ == "" || rn.ProviderID == "" {
			logf.FromContext(ctx).Info("skipping node with incomplete info",
				"node", node.Name, "ip", rn.PrivateIP, "az", rn.AZ, "providerID", rn.ProviderID)
			continue
		}
		nodes = append(nodes, rn)
	}
	return nodes, nil
}

func (r *CUDNBgpConfigReconciler) reconcileDelete(ctx context.Context, config *networkingv1alpha1.CUDNBgpConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	baselineStatus := config.Status.DeepCopy()

	routingList := &networkingv1alpha1.CUDNBgpRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}
	if len(routingList.Items) > 0 {
		log.Info("deletion blocked: CUDNBgpRouting CRs still exist", "count", len(routingList.Items))
		if err := r.reportDeletionBlocked(ctx, config, *baselineStatus, routingList.Items); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if config.Spec.Platform != networkingv1alpha1.PlatformManual {
		log.Info("cleaning up cloud resources", "platform", config.Spec.Platform)
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			log.Error(err, "unable to build cloud platform for cleanup, skipping cloud resource cleanup")
		} else if _, err := p.DiscoverEndpoints(ctx); err != nil {
			log.Error(err, "unable to discover endpoints for cleanup, skipping cloud resource cleanup")
		} else if err := p.Cleanup(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up cloud resources: %w", err)
		}
	}

	if config.Spec.AutoLabelRouterNodes != nil {
		log.Info("removing router node labels")
		if _, err := RemoveAllRouterLabels(ctx, r.Client, config); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing router node labels: %w", err)
		}
	}

	log.Info("cleaning up FRR configurations")
	if err := DeleteFRRConfigurations(ctx, r.Client); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(config, ConfigFinalizerName)
	if err := r.Update(ctx, config); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("finalizer removed, deletion complete")
	return ctrl.Result{}, nil
}

func (r *CUDNBgpConfigReconciler) setDegraded(
	ctx context.Context,
	config *networkingv1alpha1.CUDNBgpConfig,
	baselineStatus networkingv1alpha1.CUDNBgpConfigStatus,
	condType, reason, message string,
) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(fmt.Errorf("%s: %s", reason, message), "setting degraded status")

	if err := r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.Phase = networkingv1alpha1.PhaseDegraded
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: c.Generation,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func nodeRelevantChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			if !reflect.DeepEqual(oldNode.Labels, newNode.Labels) {
				return true
			}
			if oldNode.Spec.ProviderID != newNode.Spec.ProviderID {
				return true
			}
			return !reflect.DeepEqual(oldNode.Status.Addresses, newNode.Status.Addresses)
		},
	}
}

func (r *CUDNBgpConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	frrCfg := &unstructured.Unstructured{}
	frrCfg.SetGroupVersionKind(FRRConfigurationGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.CUDNBgpConfig{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		), builder.WithPredicates(nodeRelevantChangePredicate())).
		Watches(frrCfg, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				if obj.GetLabels()[LabelManagedBy] != LabelManagedByVal {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		)).
		Named("cudnbgpconfig").
		Complete(r)
}
