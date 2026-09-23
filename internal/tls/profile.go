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

package tls

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"
	"github.com/openshift/library-go/pkg/crypto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// apiServerGetTimeout bounds the startup read of apiservers/cluster so that a
// hung read falls back to the platform default. The health probe binds only
// after this read, so discovery (client-go's 32s default) plus this should end
// before the shipped liveness probe's third failure, around 60s after start.
const apiServerGetTimeout = 10 * time.Second

// Profile holds the cluster TLS security profile and the controller-runtime
// TLSOpts that should be applied to operator TLS servers.
type Profile struct {
	TLSOpts     []func(*tls.Config)
	profileSpec configv1.TLSProfileSpec
	adherence   configv1.TLSAdherencePolicy
	// watch reports whether SetupProfileWatch should install a watcher. It is
	// true whenever the APIServer API is served (so a later object can replace
	// the profile) and false when the API is absent.
	watch bool
}

// GetProfileInfo builds TLSOpts for the operator's TLS servers (metrics, webhook).
//
// Discovery failure is fatal. If the API is not served, TLSOpts use the platform
// default (GetTLSProfileSpec(nil)) and SetupProfileWatch is a no-op. If the API
// is served but Get of apiservers/cluster fails, the same default is used and a
// watcher is registered so a later object can replace it.
//
// When the object is read, TLSOpts apply the cluster profile only if
// ShouldHonorClusterTLSProfile is true.
func GetProfileInfo(ctx context.Context, client ctrlclient.Client, discoveryClient discovery.ServerResourcesInterface) (Profile, error) {
	return getProfileInfo(ctx, client, discoveryClient, apiServerGetTimeout)
}

func getProfileInfo(ctx context.Context, client ctrlclient.Client, discoveryClient discovery.ServerResourcesInterface, getTimeout time.Duration) (Profile, error) {
	log := logr.FromContextOrDiscard(ctx)

	present, err := apiServerAPIPresent(discoveryClient)
	if err != nil {
		return Profile{}, err
	}

	// API not served: platform default, and no object to watch.
	if !present {
		log.Info("OpenShift TLS profile API not available, using platform default")
		return profileFromAPIServer(log, nil, false)
	}

	// API served: read the cluster object, falling back to the platform default
	// if it can't be read. Either way we watch so a later object can replace it.
	getCtx, cancel := context.WithTimeoutCause(ctx, getTimeout,
		fmt.Errorf("APIServer read did not complete within %s", getTimeout))
	defer cancel()
	apiServer := &configv1.APIServer{}
	if err := client.Get(getCtx, types.NamespacedName{Name: openshifttls.APIServerName}, apiServer); err != nil {
		log.Error(err, "unable to get APIServer TLS profile, using platform default")
		return profileFromAPIServer(log, nil, true)
	}

	return profileFromAPIServer(log, apiServer, true)
}

// SetupProfileWatch registers a watcher for tlsSecurityProfile and tlsAdherence
// changes. onChange is invoked when either field changes so the caller can
// cancel the manager context and let the Deployment restart the pod.
// No watcher is registered when the APIServer API is not served.
func (p *Profile) SetupProfileWatch(ctx context.Context, mgr ctrl.Manager, onChange func()) error {
	if !p.watch {
		return nil
	}

	w := p.newProfileWatcher(mgr.GetClient(), logr.FromContextOrDiscard(ctx), onChange)
	if err := w.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to setup TLS profile watcher: %w", err)
	}

	return nil
}

// newProfileWatcher returns the watcher SetupProfileWatch registers, calling
// onChange when a change to apiservers/cluster alters what p applies.
func (p *Profile) newProfileWatcher(client ctrlclient.Client, log logr.Logger, onChange func()) *openshifttls.SecurityProfileWatcher {
	return &openshifttls.SecurityProfileWatcher{
		Client:                    client,
		InitialTLSProfileSpec:     p.profileSpec,
		InitialTLSAdherencePolicy: p.adherence,
		OnProfileChange: func(_ context.Context, oldProfile, newProfile configv1.TLSProfileSpec) {
			if !crypto.ShouldHonorClusterTLSProfile(p.adherence) {
				log.Info("TLS security profile changed but not honored due to adherence policy", "adherence", p.adherence)
				return
			}
			log.Info("TLS security profile changed",
				"oldMinVersion", oldProfile.MinTLSVersion, "oldCiphers", len(oldProfile.Ciphers), "oldGroups", len(oldProfile.Groups),
				"newMinVersion", newProfile.MinTLSVersion, "newCiphers", len(newProfile.Ciphers), "newGroups", len(newProfile.Groups))
			onChange()
		},
		OnAdherencePolicyChange: func(_ context.Context, oldPolicy, newPolicy configv1.TLSAdherencePolicy) {
			if !shouldHonorAdherenceChange(oldPolicy, newPolicy) {
				log.Info("TLS adherence policy changed but honoring is unaffected",
					"old", oldPolicy, "new", newPolicy)
				return
			}
			log.Info("TLS adherence policy changed", "old", oldPolicy, "new", newPolicy)
			onChange()
		},
	}
}

func apiServerAPIPresent(discoveryClient discovery.ServerResourcesInterface) (bool, error) {
	list, err := discoveryClient.ServerResourcesForGroupVersion(configv1.GroupVersion.String())
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("discover %s: %w", configv1.GroupVersion.String(), err)
	}
	for _, r := range list.APIResources {
		if r.Kind == "APIServer" {
			return true, nil
		}
	}
	return false, nil
}

// profileFromAPIServer builds a Profile from apiServer. A nil apiServer uses the
// platform default profile and empty adherence (no ShouldHonor check); a non-nil
// apiServer uses its TLSSecurityProfile and TLSAdherence. watch sets whether
// SetupProfileWatch will install a watcher for this Profile.
func profileFromAPIServer(log logr.Logger, apiServer *configv1.APIServer, watch bool) (Profile, error) {
	var securityProfile *configv1.TLSSecurityProfile
	var adherence configv1.TLSAdherencePolicy
	honor := true // the platform default (nil apiServer) is always applied
	if apiServer != nil {
		securityProfile = apiServer.Spec.TLSSecurityProfile
		adherence = apiServer.Spec.TLSAdherence
		honor = crypto.ShouldHonorClusterTLSProfile(adherence)
	}

	spec, err := openshifttls.GetTLSProfileSpec(securityProfile)
	if err != nil {
		return Profile{}, fmt.Errorf("failed to get TLS profile spec: %w", err)
	}

	profile := Profile{
		profileSpec: spec,
		adherence:   adherence,
		watch:       watch,
	}
	if apiServer != nil {
		if !honor {
			log.Info("Not honoring cluster TLS profile due to adherence policy", "adherence", adherence)
			return profile, nil
		}
		log.Info("Honoring cluster TLS profile", "adherence", adherence)
	}

	tlsConfigFunc, unsupportedCiphers := openshifttls.NewTLSConfigFromProfile(spec)
	for _, cipher := range unsupportedCiphers {
		log.Info("Cipher suite not available in this Go version, skipping", "cipher", cipher)
	}

	profile.TLSOpts = []func(*tls.Config){tlsConfigFunc}
	return profile, nil
}

// shouldHonorAdherenceChange reports whether moving from oldPolicy to
// newPolicy alters whether we honour the cluster profile.
func shouldHonorAdherenceChange(oldPolicy, newPolicy configv1.TLSAdherencePolicy) bool {
	return crypto.ShouldHonorClusterTLSProfile(oldPolicy) != crypto.ShouldHonorClusterTLSProfile(newPolicy)
}
