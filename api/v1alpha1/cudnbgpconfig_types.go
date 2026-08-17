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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=bfd;"bgp-keepalive"
type LivenessDetectionType string

const (
	LivenessDetectionBFD          LivenessDetectionType = "bfd"
	LivenessDetectionBGPKeepalive LivenessDetectionType = "bgp-keepalive"
)

// +kubebuilder:validation:Enum=Pending;Configuring;Ready;Degraded
type PhaseType string

const (
	PhasePending     PhaseType = "Pending"
	PhaseConfiguring PhaseType = "Configuring"
	PhaseReady       PhaseType = "Ready"
	PhaseDegraded    PhaseType = "Degraded"
)

// PlatformType selects which cloud the operator reconciles BGP peering
// against. It is the discriminator for the cloud-specific block in the spec.
//
// Values are added only alongside a working implementation, so that an
// unsupported cloud is refused at admission rather than accepted and then
// failing at reconcile.
// +kubebuilder:validation:Enum=AWS;Azure;GCP;Manual
type PlatformType string

// AllPlatforms is every value the enum above accepts. The dispatch test walks
// it, so a value added to the marker and forgotten here, or added to both and
// never given a builder, fails rather than surfacing as "no platform
// implementation" at runtime on a live cluster.
var AllPlatforms = []PlatformType{PlatformAWS, PlatformAzure, PlatformGCP, PlatformManual}

const (
	// PlatformAWS discovers BGP neighbours from VPC Route Server endpoints and
	// reconciles Route Server peers and source/dest check. Requires spec.aws.
	PlatformAWS PlatformType = "AWS"
	// PlatformAzure discovers BGP neighbours from an Azure Route Server and
	// reconciles its BGP connections. Requires spec.azure.
	PlatformAzure PlatformType = "Azure"
	// PlatformGCP discovers BGP neighbours from a Cloud Router and reconciles
	// NCC spokes, Cloud Router peers and GCE instance attributes. Requires
	// spec.gcp.
	PlatformGCP PlatformType = "GCP"
	// PlatformManual performs no cloud reconciliation. BGP neighbours are
	// taken from spec.bgp.peerGroups.
	PlatformManual PlatformType = "Manual"
)

const (
	ConditionNetworkOperatorPatched = "NetworkOperatorPatched"
	ConditionFRRNamespaceReady      = "FRRNamespaceReady"
	// The discovery and reconcile conditions are the operator's own, not one
	// provider's. Every cloud it supports reports them, and a condition named
	// AWSEndpointsDiscovered on a cluster with no AWS in it is a report
	// nobody can act on.
	ConditionCloudEndpointsDiscovered = "CloudEndpointsDiscovered"
	ConditionFRRConfigurationApplied  = "FRRConfigurationApplied"
	ConditionCloudResourcesReconciled = "CloudResourcesReconciled"
)

type BGPNeighbor struct {
	Address string `json:"address"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	RemoteASN int64 `json:"remoteASN"`
	// EBGPMultiHop allows the session to be established with a peer that is
	// not on this node's link.
	//
	// An Azure Route Server needs it, because it sits in its own subnet rather
	// than on the node's; an AWS Route Server endpoint and a GCP Cloud Router
	// interface do not. Declared here rather than inferred, because whether a
	// neighbour is on the link is a property of the cloud's topology and not
	// something this operator can work out.
	// +optional
	EBGPMultiHop bool `json:"ebgpMultiHop,omitempty"`
}

// PeerGroup is a set of router nodes sharing a neighbour set, and becomes one
// FRRConfiguration.
//
// It was AvailabilityZone, which is a concept only AWS has here: its Route
// Server endpoints are per subnet and therefore per zone, so a node peers with
// the ones in its own. Nothing about the type requires that, and a cloud whose
// neighbours are regional has one group rather than one per zone.
type PeerGroup struct {
	NodeSelector map[string]string `json:"nodeSelector"`
	// +kubebuilder:validation:MinItems=1
	Neighbors []BGPNeighbor `json:"neighbors"`
}

// AWSConfig names the VPC Route Servers to discover. A VPC can hold several
// and their endpoints are per subnet, which is why AWS is the cloud that
// produces more than one peer group.
type AWSConfig struct {
	Region string `json:"region"`
	// +kubebuilder:validation:MinItems=1
	RouteServerIDs []string `json:"routeServerIDs"`
}

// NCCConfig identifies the Network Connectivity Center hub the router nodes
// attach to as spokes. On GCP, membership is a precondition of peering rather
// than part of it: a VM cannot peer with a Cloud Router until it belongs to a
// router appliance spoke.
type NCCConfig struct {
	// +kubebuilder:validation:MinLength=1
	HubName string `json:"hubName"`
	// SpokePrefix names the spokes this operator manages. Spokes are numbered
	// from it, because a spoke holds a limited number of instances.
	// +kubebuilder:validation:MinLength=1
	SpokePrefix string `json:"spokePrefix"`
	// SiteToSiteDataTransfer enables NCC site-to-site data transfer on the
	// managed spokes.
	// +optional
	SiteToSiteDataTransfer bool `json:"siteToSiteDataTransfer,omitempty"`
}

// GCPConfig names the Cloud Router carrying the BGP, and the hub and spoke
// that make a node eligible to peer with it.
type GCPConfig struct {
	// +kubebuilder:validation:MinLength=1
	Project string `json:"project"`
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// CloudRouterName is the Cloud Router the router nodes peer with; its
	// interface addresses become the BGP neighbours. It must not be the
	// installer's Cloud NAT router, which has no interfaces and carries the
	// cluster's egress.
	// +kubebuilder:validation:MinLength=1
	CloudRouterName string    `json:"cloudRouterName"`
	NCC             NCCConfig `json:"ncc"`
	// EnableNestedVirtualization turns on nested virtualisation on the router
	// instances, which KubeVirt needs. Enabling it restarts the instance.
	// +optional
	// +kubebuilder:default=true
	EnableNestedVirtualization *bool `json:"enableNestedVirtualization,omitempty"`
}

// AzureConfig names the Route Server. Its addresses and ASN are read from it
// rather than declared: there is exactly one per virtual network, so there is
// nothing to enumerate, and Azure fixes the far-side ASN at 65515 with no flag
// to change it.
type AzureConfig struct {
	// +kubebuilder:validation:MinLength=1
	SubscriptionID string `json:"subscriptionID"`
	// +kubebuilder:validation:MinLength=1
	ResourceGroup string `json:"resourceGroup"`
	// +kubebuilder:validation:MinLength=1
	RouteServerName string `json:"routeServerName"`
}

// PeerGroupStatus reports one group of the peering plan the operator
// discovered, and therefore what FRR was configured to peer with.
//
// It replaced status.aws, which reported Route Server ids, endpoint ids and
// their availability zones. Only AWS could populate that, so any other cloud
// would reconcile its peerings and then report nothing at all about them. The
// alternative was a sibling status block per cloud, which is worse.
type PeerGroupStatus struct {
	// Key names the group in cloud-meaningful terms: an availability zone on
	// AWS, and whatever names the single regional endpoint elsewhere.
	Key string `json:"key"`
	// NodeSelector narrows spec.routerNodeSelector to this group. Empty means
	// every router node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Neighbors are the addresses the router nodes in this group peer with.
	// +optional
	Neighbors []BGPNeighbor `json:"neighbors,omitempty"`
}

type BGPConfig struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	LocalASN int64 `json:"localASN"`
	// +kubebuilder:default="bgp-keepalive"
	LivenessDetection LivenessDetectionType `json:"livenessDetection,omitempty"`
	// +optional
	PeerGroups []PeerGroup `json:"peerGroups,omitempty"`
}

// The cloud block and the platform have to agree in both directions: naming a
// platform without its block leaves the operator nothing to work from, and a
// block without its platform is configuration that will never be read. Saying
// it in CEL means the API server refuses it, rather than the operator
// discovering it at reconcile and reporting Degraded.
//
// peerGroups is the counterpart for Manual, which has no cloud to discover
// from and so must declare its neighbours, and must not declare them on any
// other platform, where they would silently lose to what was discovered.
//
// This replaces a pair of rules phrased around spec.aws being present or
// absent, which said the same thing for one cloud and had no way to say it for
// a second.
// +kubebuilder:validation:XValidation:rule="(self.platform == 'AWS') == has(self.aws)",message="spec.aws must be set when spec.platform is AWS, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="(self.platform == 'Azure') == has(self.azure)",message="spec.azure must be set when spec.platform is Azure, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="(self.platform == 'GCP') == has(self.gcp)",message="spec.gcp must be set when spec.platform is GCP, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="self.platform != 'Manual' || (has(self.bgp.peerGroups) && size(self.bgp.peerGroups) > 0)",message="spec.bgp.peerGroups is required when spec.platform is Manual"
// +kubebuilder:validation:XValidation:rule="self.platform == 'Manual' || !has(self.bgp.peerGroups) || size(self.bgp.peerGroups) == 0",message="spec.bgp.peerGroups may only be set when spec.platform is Manual"
type CUDNBgpConfigSpec struct {
	Platform           PlatformType      `json:"platform"`
	BGP                BGPConfig         `json:"bgp"`
	RouterNodeSelector map[string]string `json:"routerNodeSelector"`
	// +optional
	AWS *AWSConfig `json:"aws,omitempty"`
	// +optional
	GCP *GCPConfig `json:"gcp,omitempty"`
	// +optional
	Azure *AzureConfig `json:"azure,omitempty"`
}

type CUDNBgpConfigStatus struct {
	Phase              PhaseType          `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	// PeerGroups is the peering plan the operator arrived at and rendered into
	// FRRConfigurations. Every cloud populates it.
	// +optional
	PeerGroups []PeerGroupStatus `json:"peerGroups,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CUDNBgpConfig is the singleton cluster-scoped BGP infrastructure configuration.
type CUDNBgpConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CUDNBgpConfigSpec   `json:"spec,omitempty"`
	Status CUDNBgpConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type CUDNBgpConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CUDNBgpConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CUDNBgpConfig{}, &CUDNBgpConfigList{})
}
