package tls

import (
	"context"
	gotls "crypto/tls"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetProfileInfo(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}

	apiServerGVR := schema.GroupResource{Group: configv1.GroupVersion.Group, Resource: "apiservers"}

	tests := []struct {
		name            string
		spec            *configv1.APIServerSpec
		discoveryClient stubDiscovery
		getErr          error
		wantErr         bool
		wantOpts        bool
		wantWatch       bool
		wantAdherence   configv1.TLSAdherencePolicy
		wantMinTLS      uint16
		wantCiphers     bool
	}{
		{
			name:            "discovery failure is fatal",
			discoveryClient: stubDiscovery{err: fmt.Errorf("connection refused")},
			wantErr:         true,
		},
		{
			name:            "API not served uses the platform default and does not watch",
			discoveryClient: stubDiscovery{err: apierrors.NewNotFound(apiServerGVR, "")},
			wantOpts:        true,
			wantWatch:       false,
			wantAdherence:   configv1.TLSAdherencePolicyStrictAllComponents,
			wantMinTLS:      gotls.VersionTLS12,
			wantCiphers:     true,
		},
		{
			name:            "missing APIServer uses the platform default and still watches",
			discoveryClient: apiPresentDiscovery(),
			wantOpts:        true,
			wantWatch:       true,
			wantAdherence:   configv1.TLSAdherencePolicyStrictAllComponents,
			wantMinTLS:      gotls.VersionTLS12,
			wantCiphers:     true,
		},
		{
			name:            "Get failure uses the platform default and still watches",
			discoveryClient: apiPresentDiscovery(),
			getErr:          fmt.Errorf("service unavailable"),
			wantOpts:        true,
			wantWatch:       true,
			wantAdherence:   configv1.TLSAdherencePolicyStrictAllComponents,
			wantMinTLS:      gotls.VersionTLS12,
			wantCiphers:     true,
		},
		{
			name: "legacy does not apply the cluster profile",
			spec: &configv1.APIServerSpec{
				TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
			},
			discoveryClient: apiPresentDiscovery(),
			wantWatch:       true,
			wantAdherence:   configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
		},
		{
			name: "strict applies Intermediate when the APIServer profile is unset",
			spec: &configv1.APIServerSpec{
				TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			},
			discoveryClient: apiPresentDiscovery(),
			wantOpts:        true,
			wantWatch:       true,
			wantAdherence:   configv1.TLSAdherencePolicyStrictAllComponents,
			wantMinTLS:      gotls.VersionTLS12,
			wantCiphers:     true,
		},
		{
			name: "strict applies the configured profile",
			spec: &configv1.APIServerSpec{
				TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
				TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
			},
			discoveryClient: apiPresentDiscovery(),
			wantOpts:        true,
			wantWatch:       true,
			wantAdherence:   configv1.TLSAdherencePolicyStrictAllComponents,
			wantMinTLS:      gotls.VersionTLS13,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.spec != nil {
				builder = builder.WithObjects(&configv1.APIServer{
					ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
					Spec:       *tt.spec,
				})
			}
			var c client.Client = builder.Build()
			if tt.getErr != nil {
				c = getErrorClient{Client: c, err: tt.getErr}
			}

			got, err := GetProfileInfo(context.Background(), c, tt.discoveryClient)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("GetProfileInfo() error = %v", err)
			}

			if got.watch != tt.wantWatch {
				t.Fatalf("watch = %v, want %v", got.watch, tt.wantWatch)
			}
			if got.adherence != tt.wantAdherence {
				t.Fatalf("adherence = %q, want %q", got.adherence, tt.wantAdherence)
			}

			if !tt.wantWatch {
				if err := got.SetupProfileWatch(context.Background(), nil, nil); err != nil {
					t.Fatalf("SetupProfileWatch skipped path returned %v", err)
				}
			}

			if tt.wantOpts {
				if len(got.TLSOpts) == 0 {
					t.Fatal("expected TLSOpts when using the cluster or platform default profile")
				}
				cfg := &gotls.Config{}
				for _, opt := range got.TLSOpts {
					opt(cfg)
				}
				if cfg.MinVersion != tt.wantMinTLS {
					t.Fatalf("MinVersion = %v, want %v", cfg.MinVersion, tt.wantMinTLS)
				}
				if tt.wantCiphers && len(cfg.CipherSuites) == 0 {
					t.Fatal("expected CipherSuites to be set")
				}
				return
			}

			if len(got.TLSOpts) != 0 {
				t.Fatalf("expected empty TLSOpts, got %d", len(got.TLSOpts))
			}
		})
	}
}

// The API only lets tlsAdherence hold LegacyAdheringComponentsOnly or
// StrictAllComponents, and refuses to remove it once set, so NoOpinion is
// reachable only as an absent field. That makes absent -> Legacy the one
// transition that changes the policy without changing what we serve.
func TestShouldHonorAdherenceChange(t *testing.T) {
	const (
		noOpinion = configv1.TLSAdherencePolicy("")
		legacy    = configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly
		strict    = configv1.TLSAdherencePolicyStrictAllComponents
	)

	tests := []struct {
		name string
		from configv1.TLSAdherencePolicy
		to   configv1.TLSAdherencePolicy
		want bool
	}{
		{"unset to legacy honours neither", noOpinion, legacy, false},
		{"legacy to unset honours neither", legacy, noOpinion, false},
		{"unset to strict starts honouring", noOpinion, strict, true},
		{"legacy to strict starts honouring", legacy, strict, true},
		{"strict to legacy stops honouring", strict, legacy, true},
		{"strict to unset stops honouring", strict, noOpinion, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldHonorAdherenceChange(tt.from, tt.to); got != tt.want {
				t.Fatalf("shouldHonorAdherenceChange(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func apiPresentDiscovery() stubDiscovery {
	return stubDiscovery{
		list: &metav1.APIResourceList{
			APIResources: []metav1.APIResource{{Kind: "APIServer"}},
		},
	}
}

type stubDiscovery struct {
	list *metav1.APIResourceList
	err  error
}

func (s stubDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return s.list, s.err
}

func (s stubDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, nil, nil
}

func (s stubDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return nil, nil
}

func (s stubDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return nil, nil
}

type getErrorClient struct {
	client.Client
	err error
}

func (c getErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.err
}
