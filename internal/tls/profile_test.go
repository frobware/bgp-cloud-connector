package tls

import (
	"context"
	gotls "crypto/tls"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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
			wantMinTLS:      gotls.VersionTLS12,
			wantCiphers:     true,
		},
		{
			name:            "missing APIServer uses the platform default and still watches",
			discoveryClient: apiPresentDiscovery(),
			wantOpts:        true,
			wantWatch:       true,
			wantMinTLS:      gotls.VersionTLS12,
			wantCiphers:     true,
		},
		{
			name:            "Get failure uses the platform default and still watches",
			discoveryClient: apiPresentDiscovery(),
			getErr:          fmt.Errorf("service unavailable"),
			wantOpts:        true,
			wantWatch:       true,
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
		},
		{
			name: "strict applies Intermediate when the APIServer profile is unset",
			spec: &configv1.APIServerSpec{
				TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			},
			discoveryClient: apiPresentDiscovery(),
			wantOpts:        true,
			wantWatch:       true,
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

func TestGetProfileInfoFallsBackWhenGetHangs(t *testing.T) {
	type result struct {
		profile Profile
		err     error
	}
	var logged strings.Builder
	log := funcr.New(func(_, args string) { logged.WriteString(args + "\n") }, funcr.Options{})

	done := make(chan result, 1)
	go func() {
		p, err := getProfileInfo(logr.NewContext(context.Background(), log), hangingGetClient{}, apiPresentDiscovery(), 100*time.Millisecond)
		done <- result{p, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("GetProfileInfo() error = %v", r.err)
		}
		if !r.profile.watch || len(r.profile.TLSOpts) == 0 {
			t.Fatalf("expected the platform default with a watch, got watch=%v opts=%d", r.profile.watch, len(r.profile.TLSOpts))
		}
		if want := "APIServer read did not complete within 100ms"; !strings.Contains(logged.String(), want) {
			t.Fatalf("log does not name the deadline %q:\n%s", want, logged.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetProfileInfo did not return while the APIServer Get hung")
	}
}

// hangingGetClient blocks every Get until its context is done, as an API
// server that accepts the request and never answers. It returns the context's
// cause, as net/http does when a request's context ends.
type hangingGetClient struct {
	client.Client
}

func (hangingGetClient) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	<-ctx.Done()
	return context.Cause(ctx)
}

// TestProfileWatcherConvergesAfterFallback starts from a Profile built either
// from a failed read (the fallback) or from the object itself, reconciles the
// object once, and checks whether the pod would restart. A restart is wanted
// exactly when a clean start from the object would serve different TLS.
func TestProfileWatcherConvergesAfterFallback(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	old := &configv1.TLSSecurityProfile{Type: configv1.TLSProfileOldType, Old: &configv1.OldTLSProfile{}}

	tests := []struct {
		name        string
		fallback    bool
		spec        configv1.APIServerSpec
		wantRestart bool
	}{
		{name: "fallback, adherence absent", fallback: true, wantRestart: true},
		{name: "fallback, Legacy", fallback: true,
			spec: configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly}, wantRestart: true},
		{name: "fallback, Legacy with Old profile", fallback: true,
			spec: configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly, TLSSecurityProfile: old}, wantRestart: true},
		{name: "fallback, Strict with default profile", fallback: true,
			spec: configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents}},
		{name: "fallback, Strict with Old profile", fallback: true,
			spec: configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents, TLSSecurityProfile: old}, wantRestart: true},
		{name: "fallback, unknown adherence with default profile", fallback: true,
			spec: configv1.APIServerSpec{TLSAdherence: "SomeFuturePolicy"}},
		{name: "clean start, adherence absent"},
		{name: "clean start, Legacy with Old profile",
			spec: configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly, TLSSecurityProfile: old}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&configv1.APIServer{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec:       tt.spec,
			}).Build()

			var startup client.Client = c
			if tt.fallback {
				startup = getErrorClient{Client: c, err: fmt.Errorf("service unavailable")}
			}
			p, err := GetProfileInfo(context.Background(), startup, apiPresentDiscovery())
			if err != nil {
				t.Fatalf("GetProfileInfo() error = %v", err)
			}

			restarted := false
			w := p.newProfileWatcher(c, logr.Discard(), func() { restarted = true })
			if _, err := w.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if restarted != tt.wantRestart {
				t.Fatalf("restarted = %v, want %v", restarted, tt.wantRestart)
			}
		})
	}
}
