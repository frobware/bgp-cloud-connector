package tls

import (
	"context"
	gotls "crypto/tls"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetProfileInfo(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		spec        *configv1.APIServerSpec
		wantOpts    bool
		wantMinTLS  uint16
		wantCiphers bool
	}{
		{
			name: "missing APIServer returns an empty profile",
			spec: nil,
		},
		{
			name: "legacy does not apply the cluster profile",
			spec: &configv1.APIServerSpec{
				TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
			},
		},
		{
			name: "strict applies Intermediate when the APIServer profile is unset",
			spec: &configv1.APIServerSpec{
				TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			},
			wantOpts:    true,
			wantMinTLS:  gotls.VersionTLS12,
			wantCiphers: true,
		},
		{
			name: "strict applies the configured profile",
			spec: &configv1.APIServerSpec{
				TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
				TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
			},
			wantOpts:   true,
			wantMinTLS: gotls.VersionTLS13,
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
			c := builder.Build()

			got, err := GetProfileInfo(context.Background(), c)
			if err != nil {
				t.Fatalf("GetProfileInfo() error = %v", err)
			}

			if tt.wantOpts {
				if len(got.TLSOpts) == 0 {
					t.Fatal("expected TLSOpts when honoring the cluster profile")
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
