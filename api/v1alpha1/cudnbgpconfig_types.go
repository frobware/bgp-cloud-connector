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

const (
	ConditionNetworkOperatorPatched   = "NetworkOperatorPatched"
	ConditionFRRNamespaceReady        = "FRRNamespaceReady"
	ConditionRouterNodesLabelled      = "RouterNodesLabelled"
	ConditionCloudEndpointsDiscovered = "CloudEndpointsDiscovered"
	ConditionFRRConfigurationApplied  = "FRRConfigurationApplied"
	ConditionCloudResourcesReconciled = "CloudResourcesReconciled"
	ConditionPrerequisitesSatisfied   = "PrerequisitesSatisfied"
	ConditionSuspended                = "Suspended"

	// ConditionReady summarises every other condition into the single answer
	// to "is this working", which the API conventions ask for in place of a
	// phase.
	//
	// It is derived rather than set by any one step, so nothing reports Ready
	// directly. Reading it is the supported way to ask; the conditions it
	// summarises say which part is wrong when it is False.
	ConditionReady = "Ready"
)

// PlatformType selects which cloud the operator reconciles BGP peering
// against. It is the discriminator for the cloud-specific block in the spec.
// Values are added only alongside a working implementation, so that an
// unsupported cloud is rejected at admission rather than at runtime.
// +kubebuilder:validation:Enum=AWS;Azure;GCP;Manual
type PlatformType string

// AllPlatforms is every value the enum above accepts. The dispatch test walks
// it, so a value added to the marker and forgotten here, or added to both and
// never given a builder, fails rather than surfacing as "no platform
// implementation" at runtime on a live cluster.
var AllPlatforms = []PlatformType{PlatformAWS, PlatformAzure, PlatformGCP, PlatformManual}

const (
	// PlatformAWS discovers BGP neighbours from Route Server endpoints and
	// reconciles Route Server peers and source/dest check. Requires spec.aws.
	PlatformAWS PlatformType = "AWS"
	// PlatformAzure discovers BGP neighbours from an Azure Route Server and
	// reconciles its BGP connections. Requires spec.azure.
	PlatformAzure PlatformType = "Azure"
	// PlatformGCP discovers BGP neighbours from the Cloud Router and
	// reconciles NCC spokes, Cloud Router peers and GCE instance attributes.
	// Requires spec.gcp.
	PlatformGCP PlatformType = "GCP"
	// PlatformManual performs no cloud reconciliation. BGP neighbours are
	// taken from spec.bgp.peerGroups.
	PlatformManual PlatformType = "Manual"
)

type BGPNeighbor struct {
	Address string `json:"address"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	RemoteASN int64 `json:"remoteASN"`
	// EBGPMultiHop allows the session to be established with a peer that is
	// not on this node's link. Azure Route Server needs it; AWS Route Server
	// and GCP Cloud Router do not.
	// +optional
	EBGPMultiHop bool `json:"ebgpMultiHop,omitempty"`
}

type PeerGroup struct {
	NodeSelector map[string]string `json:"nodeSelector"`
	// +kubebuilder:validation:MinItems=1
	Neighbors []BGPNeighbor `json:"neighbors"`
}

type AWSConfig struct {
	Region string `json:"region"`
	// +kubebuilder:validation:MinItems=1
	RouteServerIDs []string `json:"routeServerIDs"`
}

// NCCConfig identifies the Network Connectivity Center hub the router nodes
// attach to as spokes.
type NCCConfig struct {
	// +kubebuilder:validation:MinLength=1
	HubName string `json:"hubName"`
	// SpokePrefix names the spokes this operator manages. Spokes are numbered
	// from it, because a hub spoke holds a limited number of instances.
	// +kubebuilder:validation:MinLength=1
	SpokePrefix string `json:"spokePrefix"`
	// SiteToSiteDataTransfer enables NCC site-to-site data transfer on the
	// managed spokes.
	// +optional
	SiteToSiteDataTransfer bool `json:"siteToSiteDataTransfer,omitempty"`
}

type GCPConfig struct {
	// +kubebuilder:validation:MinLength=1
	Project string `json:"project"`
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// CloudRouterName is the Cloud Router the router nodes peer with. Its
	// interface addresses become the BGP neighbours.
	// +kubebuilder:validation:MinLength=1
	CloudRouterName string    `json:"cloudRouterName"`
	NCC             NCCConfig `json:"ncc"`
	// EnableNestedVirtualization turns on nested virtualisation on the router
	// instances, which KubeVirt needs. Enabling it restarts the instance.
	// +optional
	// +kubebuilder:default=true
	EnableNestedVirtualization *bool `json:"enableNestedVirtualization,omitempty"`
}

// PeerGroupStatus reports one group of the peering plan the operator
// discovered, and therefore what FRR was configured to peer with. Every cloud
// populates it, which is why it replaced the AWS-shaped status.aws: that block
// reported Route Server endpoints and had no GCP or Azure equivalent, so on
// those clouds the operator reconciled peerings and said nothing about them.
type PeerGroupStatus struct {
	// Key names the group in cloud-meaningful terms: an availability zone on
	// AWS, the Cloud Router or Route Server name on GCP and Azure.
	Key string `json:"key"`
	// NodeSelector is the selector narrowing spec.routerNodeSelector to this
	// group. Empty means every router node, which is what the single-group
	// clouds emit.
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

// AutoLabelRouterNodesSpec makes the operator maintain
// spec.routerNodeSelector on the nodes that should be BGP routers, instead of
// requiring something else to have labelled them. Without it a node added by
// scaling a MachineSet is silently not a router, because the MachineSet
// template carries no such label.
//
// It is opt-in: leaving it unset means the operator only ever reads Node
// objects, which is the right behaviour where the labels are already managed
// deliberately.
type AutoLabelRouterNodesSpec struct {
	// Eligible selects the nodes that should be BGP routers.
	// +kubebuilder:validation:MinProperties=1
	Eligible map[string]string `json:"eligible"`
	// Exclude removes a node from eligibility if it carries any of these
	// label keys, whatever their value. Typically infra nodes.
	// +optional
	Exclude map[string]string `json:"exclude,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(self.platform == 'AWS') == has(self.aws)",message="spec.aws must be set when spec.platform is AWS, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="(self.platform == 'Azure') == has(self.azure)",message="spec.azure must be set when spec.platform is Azure, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="(self.platform == 'GCP') == has(self.gcp)",message="spec.gcp must be set when spec.platform is GCP, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="self.platform != 'Manual' || (has(self.bgp.peerGroups) && size(self.bgp.peerGroups) > 0)",message="spec.bgp.peerGroups is required when spec.platform is Manual"
// +kubebuilder:validation:XValidation:rule="self.platform == 'Manual' || !has(self.bgp.peerGroups) || size(self.bgp.peerGroups) == 0",message="spec.bgp.peerGroups may only be set when spec.platform is Manual"
type CUDNBgpConfigSpec struct {
	// Platform selects which cloud the operator reconciles against and which
	// cloud-specific block below must be populated.
	// +kubebuilder:validation:Required
	Platform PlatformType `json:"platform"`
	BGP      BGPConfig    `json:"bgp"`
	// RouterNodeSelector identifies the BGP router nodes. When
	// AutoLabelRouterNodes is set the operator maintains these labels;
	// otherwise something else must apply them.
	RouterNodeSelector map[string]string `json:"routerNodeSelector"`
	// +optional
	AutoLabelRouterNodes *AutoLabelRouterNodesSpec `json:"autoLabelRouterNodes,omitempty"`
	// Suspended releases everything the operator created in the cloud and
	// stops reconciling, keeping the configuration. It is not a deletion:
	// the finalizer stays and clearing the field brings everything back,
	// where deleting the CR would mean writing it again to resume.
	//
	// The Network operator patch is deliberately not reverted, for the same
	// reason teardown does not revert it: undoing
	// additionalRoutingCapabilities restarts OVN-Kubernetes on every node.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
	// RequireReadyNodes drops nodes that are not Ready from the router set,
	// so the cloud stops sending traffic to a node that cannot forward it.
	//
	// It is off by default because turning it on churns cloud peerings
	// whenever a node reboots: the peering is removed as the node goes
	// NotReady and recreated when it returns. Whether that trade is worth
	// making depends on how long your nodes stay NotReady, which is not
	// something this operator can know.
	// +optional
	RequireReadyNodes bool         `json:"requireReadyNodes,omitempty"`
	AWS               *AWSConfig   `json:"aws,omitempty"`
	Azure             *AzureConfig `json:"azure,omitempty"`
	GCP               *GCPConfig   `json:"gcp,omitempty"`
}

// AzureConfig identifies the Azure Route Server the router nodes peer with.
// Azure models a Route Server as a Virtual Hub, and its own addresses and ASN
// are read from it rather than configured here.
type AzureConfig struct {
	// +kubebuilder:validation:MinLength=1
	SubscriptionID string `json:"subscriptionID"`
	// +kubebuilder:validation:MinLength=1
	ResourceGroup string `json:"resourceGroup"`
	// RouteServerName is the Azure Route Server whose BGP connections this
	// operator manages. Its VirtualRouterIPs become the BGP neighbours.
	// +kubebuilder:validation:MinLength=1
	RouteServerName string `json:"routeServerName"`
}

// PeerStatus is one cloud-side peering as the cloud reports it, which is the
// only part of this status that is not the operator quoting its own intent
// back. status.peerGroups says what was discovered and what FRR was told to
// do; both can be perfect while no session exists.
type PeerStatus struct {
	// Name is the cloud's own name for the peering resource.
	Name string `json:"name"`
	// Node is the router node the peering is for, where the cloud records
	// enough to say so. Absent rather than guessed: an Azure BGP connection,
	// for one, records an address and nothing about whose it is.
	// +optional
	Node string `json:"node,omitempty"`
	// Address is the node address the cloud peers with.
	Address string `json:"address"`
	// ASN is the ASN the cloud expects the node to speak, which is
	// spec.bgp.localASN. Reported because a session that will not come up
	// after someone edited it in the console is otherwise indistinguishable
	// from one that was never configured.
	// +optional
	ASN int64 `json:"asn,omitempty"`
	// State is the cloud's view of the peering resource: whether it finished
	// being created. Empty on GCP, where a Cloud Router peer is a field
	// inside the router and has no lifecycle of its own.
	// +optional
	State string `json:"state,omitempty"`
	// SessionState is the cloud's view of the BGP session, which is the half
	// that answers whether it is up.
	// +optional
	SessionState string `json:"sessionState,omitempty"`
}

type CUDNBgpConfigStatus struct {
	Conditions         []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	// PeerGroups is the discovered peering plan: what the operator found in
	// the cloud and rendered into FRRConfigurations. Empty under
	// platform: Manual, where the plan is declared in spec.bgp.peerGroups
	// rather than discovered.
	// +optional
	PeerGroups []PeerGroupStatus `json:"peerGroups,omitempty"`
	// Peers is what the cloud says about the peerings this operator owns.
	// Reported, never acted on: a session that will not establish is not
	// something the operator can fix by writing the peering again, and the
	// alternative to reporting it is three different cloud consoles.
	// +optional
	Peers []PeerStatus `json:"peers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Platform",type="string",JSONPath=".spec.platform"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].reason"
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

// IsNestedVirtEnabled reports whether nested virtualisation should be enabled
// on GCP router instances, defaulting to true when unset.
func (c *GCPConfig) IsNestedVirtEnabled() bool {
	return c.EnableNestedVirtualization == nil || *c.EnableNestedVirtualization
}
