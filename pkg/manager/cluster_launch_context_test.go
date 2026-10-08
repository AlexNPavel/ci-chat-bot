package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blang/semver"
	"k8s.io/apimachinery/pkg/util/sets"
)

type contextRecordingConfigResolver struct {
	legacyCalls  int
	contextCalls int
	contextErr   error
}

func (r *contextRecordingConfigResolver) Resolve(string, string, string, string) ([]byte, bool, error) {
	r.legacyCalls++
	return nil, false, nil
}

func (r *contextRecordingConfigResolver) ResolveWithContext(ctx context.Context, _, _, _, _ string) ([]byte, bool, error) {
	r.contextCalls++
	r.contextErr = ctx.Err()
	return []byte("config"), true, nil
}

func TestDefaultClusterLaunchPlatformArchitectureUsesSupportedVersionThreshold(t *testing.T) {
	oldRelease := CurrentRelease
	HypershiftSupportedVersions.Mu.Lock()
	oldVersions := HypershiftSupportedVersions.Versions.Clone()
	CurrentRelease = semver.Version{Major: 4, Minor: 23}
	HypershiftSupportedVersions.Versions = sets.New("4.20", "4.23")
	HypershiftSupportedVersions.Mu.Unlock()
	t.Cleanup(func() {
		HypershiftSupportedVersions.Mu.Lock()
		CurrentRelease = oldRelease
		HypershiftSupportedVersions.Versions = oldVersions
		HypershiftSupportedVersions.Mu.Unlock()
	})

	tests := []struct {
		name         string
		inputs       [][]string
		platform     string
		architecture string
		wantPlatform string
		wantArch     string
		wantErr      bool
	}{
		{name: "supported explicit version defaults to HyperShift", inputs: [][]string{{"4.20.0-0.nightly"}}, wantPlatform: "hypershift-hosted", wantArch: "multi"},
		{name: "nightly uses current supported release threshold", inputs: [][]string{{"nightly"}}, wantPlatform: "hypershift-hosted", wantArch: "multi"},
		{name: "unsupported older version defaults to AWS", inputs: [][]string{{"4.18.2"}}, wantPlatform: "aws", wantArch: "amd64"},
		{name: "explicit AWS remains unchanged", inputs: [][]string{{"4.23.0-0.nightly"}}, platform: "aws", architecture: "amd64", wantPlatform: "aws", wantArch: "amd64"},
		{name: "HyperShift rejects non-multiarch", inputs: [][]string{{"4.23.0-0.nightly"}}, platform: "hypershift-hosted", architecture: "amd64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, architecture, err := DefaultClusterLaunchPlatformArchitecture(tt.inputs, tt.platform, tt.architecture, JobTypeInstall)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DefaultClusterLaunchPlatformArchitecture() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if platform != tt.wantPlatform || architecture != tt.wantArch {
				t.Fatalf("defaults = (%q, %q), want (%q, %q)", platform, architecture, tt.wantPlatform, tt.wantArch)
			}
		})
	}
}

func TestNormalizeJobRequestOptionsRejectsInvalidLaunchOptions(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
	}{
		{name: "unknown parameter", params: map[string]string{"not-a-real-option": ""}},
		{name: "test parameter is not allowed for launches", params: map[string]string{"test": "e2e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &JobRequest{
				Inputs:      [][]string{{"4.18.2"}},
				Type:        JobTypeInstall,
				JobParams:   tt.params,
				RequestedAt: time.Now(),
			}
			if err := normalizeJobRequestOptions(req); err == nil {
				t.Fatal("normalizeJobRequestOptions() succeeded for an invalid option")
			}
		})
	}
}

func TestCanceledLaunchResolutionDoesNotStartDependencies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	manager := &jobManager{}
	if _, _, _, err := manager.resolveImageOrVersion(ctx, "4.18.2", "", "amd64"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveImageOrVersion() error = %v, want context.Canceled", err)
	}
	if _, err := manager.resolveAsPullRequestWithContext(ctx, "openshift/installer#123"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveAsPullRequestWithContext() error = %v, want context.Canceled", err)
	}

	resolver := &contextRecordingConfigResolver{}
	if _, _, err := resolveConfigWithContext(ctx, resolver, "org", "repo", "branch", "variant"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveConfigWithContext() error = %v, want context.Canceled", err)
	}
	if resolver.legacyCalls != 0 || resolver.contextCalls != 0 {
		t.Fatalf("canceled configuration resolution called the dependency: %#v", resolver)
	}
}

func TestResolveConfigWithContextUsesContextAwareResolverOnce(t *testing.T) {
	resolver := &contextRecordingConfigResolver{}
	config, found, err := resolveConfigWithContext(context.Background(), resolver, "org", "repo", "branch", "variant")
	if err != nil {
		t.Fatalf("resolveConfigWithContext() error = %v", err)
	}
	if !found || string(config) != "config" || resolver.contextCalls != 1 || resolver.legacyCalls != 0 || resolver.contextErr != nil {
		t.Fatalf("resolver result/calls = (%q, %v, %#v), want one context-aware call", config, found, resolver)
	}
}

func TestBusyProwWorkerNeverConfirmsUntestedCreate(t *testing.T) {
	fixture := newClusterLaunchFixture(1)
	request, _ := successfulDMRequest("U-worker", "request-worker", launchTestImage)
	jobID := deterministicProwJobName(fixture.manager.clusterPrefix, requestKeyHash(request.ServicePrincipal, request.SlackUserID, request.RequestID))
	if !fixture.manager.tryJob(jobID) {
		t.Fatal("could not occupy the worker key")
	}
	defer fixture.manager.finishJob(jobID)

	_, err := fixture.manager.SubmitCluster(context.Background(), request)
	if err == nil {
		t.Fatal("SubmitCluster returned success although it could not create or read the ProwJob")
	}
	toolErr := launchToolError(t, err)
	if toolErr.Code != ToolErrorCodeLaunchInitialization || !toolErr.Retryable || toolErr.JobID != jobID {
		t.Fatalf("SubmitCluster error = %#v, want retryable LAUNCH_INITIALIZATION for %q", toolErr, jobID)
	}
	if fixture.createCount.Load() != 0 {
		t.Fatalf("Prow Create called %d times while another worker owned the key", fixture.createCount.Load())
	}
}
