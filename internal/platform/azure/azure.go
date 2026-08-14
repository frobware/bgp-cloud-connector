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

// Package azure implements CloudPlatform against Azure Route Server.
//
// Azure Route Server is a Virtual Hub. The addresses the router nodes peer
// with are the hub's own VirtualRouterIPs, a redundant pair, and the remote
// ASN is its VirtualRouterAsn, which Azure fixes at 65515. The peers
// themselves are VirtualHubBgpConnections, one per router node.
//
// One thing differs from the other clouds and it is visible in the peering
// plan rather than behind a method: the hub is not on the node's link, so
// every neighbour asks for eBGP multihop.
//
// The per-node work is the same concept as on the other two clouds even though
// all three name it differently -- AWS clears SourceDestCheck, GCP sets
// canIpForward, Azure sets enableIPForwarding on the node's network interface.
// See nodes.go, which carries the polarity table and the reason the name is
// misleading.
package azure

import (
	"context"
	"fmt"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// Config identifies the Route Server this operator manages.
type Config struct {
	SubscriptionID  string
	ResourceGroup   string
	RouteServerName string
	LocalASN        int64
	// ClusterID names the peers this operator owns. Azure peer names are
	// per-hub, so without it two clusters sharing a Route Server would fight
	// over the same names.
	ClusterID string
}

// RouteServerTopology is what a Route Server tells us about itself: the
// addresses it peers from and the ASN it peers as.
type RouteServerTopology struct {
	ASN       int64
	Addresses []string
}

// Peering is one BGP connection on the Route Server.
type Peering struct {
	Name              string
	PeerIP            string
	PeerASN           int64
	ProvisioningState string
	ConnectionState   string
}

// routeServerAPI is the slice of Azure this platform uses. It exists so the
// tests can drive the reconcile logic without reaching Azure, in the same
// shape as the AWS and GCP platforms.
type routeServerAPI interface {
	GetTopology(ctx context.Context) (*RouteServerTopology, error)
	ListPeerings(ctx context.Context) ([]Peering, error)
	CreatePeering(ctx context.Context, name, peerIP string, peerASN int64) error
	DeletePeering(ctx context.Context, name string) error
}

type Platform struct {
	cfg  Config
	rs   routeServerAPI
	nics nicAPI
}

// New builds a platform against the real Azure API, verifying the credential
// by reading the Route Server. Azure has no cheap identity probe equivalent to
// sts:GetCallerIdentity, so the first real call doubles as one.
func New(ctx context.Context, cfg Config) (*Platform, error) {
	client, err := newRouteServerClient(cfg)
	if err != nil {
		return nil, &platform.CredentialError{
			Msg: fmt.Sprintf("Azure credential verification failed: %v", err),
		}
	}
	nics, err := newNICClient(cfg)
	if err != nil {
		return nil, &platform.CredentialError{
			Msg: fmt.Sprintf("Azure credential verification failed: %v", err),
		}
	}
	p := &Platform{cfg: cfg, rs: client, nics: nics}
	if _, err := p.rs.GetTopology(ctx); err != nil {
		return nil, &platform.CredentialError{
			Msg: fmt.Sprintf("could not read Route Server %q in resource group %q: %v",
				cfg.RouteServerName, cfg.ResourceGroup, err),
		}
	}
	return p, nil
}

// NewWithClients builds a platform against supplied clients, for tests.
func NewWithClients(cfg Config, rs routeServerAPI, nics nicAPI) *Platform {
	return &Platform{cfg: cfg, rs: rs, nics: nics}
}
