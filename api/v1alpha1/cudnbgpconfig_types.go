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

const (
	ConditionNetworkOperatorPatched   = "NetworkOperatorPatched"
	ConditionFRRNamespaceReady        = "FRRNamespaceReady"
	ConditionRouterNodesLabelled      = "RouterNodesLabelled"
	ConditionCloudEndpointsDiscovered = "CloudEndpointsDiscovered"
	ConditionFRRConfigurationApplied  = "FRRConfigurationApplied"
	ConditionCloudResourcesReconciled = "CloudResourcesReconciled"
	ConditionPrerequisitesSatisfied   = "PrerequisitesSatisfied"
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
	// MachineNamespace holds the OpenShift Machine objects whose preTerminate
	// hooks gate instance deletion until BGP peers are withdrawn.
	// +optional
	// +kubebuilder:default="openshift-machine-api"
	MachineNamespace string `json:"machineNamespace,omitempty"`
}

type DiscoveredEndpoint struct {
	EndpointID       string `json:"endpointID"`
	AvailabilityZone string `json:"availabilityZone"`
	Address          string `json:"address"`
}

type DiscoveredRouteServer struct {
	RouteServerID string               `json:"routeServerID"`
	RemoteASN     int64                `json:"remoteASN"`
	Endpoints     []DiscoveredEndpoint `json:"endpoints,omitempty"`
}

type AWSStatus struct {
	RouteServers []DiscoveredRouteServer `json:"routeServers,omitempty"`
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
	AWS                  *AWSConfig                `json:"aws,omitempty"`
	Azure                *AzureConfig              `json:"azure,omitempty"`
	GCP                  *GCPConfig                `json:"gcp,omitempty"`
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

type CUDNBgpConfigStatus struct {
	Phase              PhaseType          `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	AWS                *AWSStatus         `json:"aws,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Platform",type="string",JSONPath=".spec.platform"
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

// IsNestedVirtEnabled reports whether nested virtualisation should be enabled
// on GCP router instances, defaulting to true when unset.
func (c *GCPConfig) IsNestedVirtEnabled() bool {
	return c.EnableNestedVirtualization == nil || *c.EnableNestedVirtualization
}
