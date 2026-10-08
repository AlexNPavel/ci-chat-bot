package manager

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/blang/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
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
		{name: "version after a PR does not pick HyperShift", inputs: [][]string{{"openshift/installer#123", "4.20"}}, wantPlatform: "aws", wantArch: "amd64"},
		{name: "leading supported version before a PR defaults to HyperShift", inputs: [][]string{{"4.20", "openshift/installer#123"}}, wantPlatform: "hypershift-hosted", wantArch: "multi"},
		{name: "multiple input groups default to AWS", inputs: [][]string{{"4.20"}, {"4.20"}}, wantPlatform: "aws", wantArch: "amd64"},
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

func TestSubmitClusterReplaysSameRequestAfterDefaultChange(t *testing.T) {
	oldRelease := CurrentRelease
	HypershiftSupportedVersions.Mu.Lock()
	oldVersions := HypershiftSupportedVersions.Versions.Clone()
	CurrentRelease = semver.Version{Major: 4, Minor: 23}
	HypershiftSupportedVersions.Versions = sets.New("4.23")
	HypershiftSupportedVersions.Mu.Unlock()
	t.Cleanup(func() {
		HypershiftSupportedVersions.Mu.Lock()
		CurrentRelease = oldRelease
		HypershiftSupportedVersions.Versions = oldVersions
		HypershiftSupportedVersions.Mu.Unlock()
	})

	req, dmCalls := successfulDMRequest("U123", "request-default-change", "nightly")
	req.Platform = ""
	req.Architecture = ""
	f := newClusterLaunchFixture(2)

	// Seed the launch as it was accepted while nightly defaulted to HyperShift.
	seeded := addDurableLaunch(f, req, prowapiv1.PendingState, time.Now().Add(-time.Minute))
	seeded.Annotations[annotationInputFingerprint] = launchInputFingerprint(req.Inputs, "hypershift-hosted", "multi", req.Parameters)
	seeded.Annotations[annotationRawInputFingerprint] = launchInputFingerprint(req.Inputs, "", "", req.Parameters)
	if err := f.client.Tracker().Update(prowJobsResource, seeded, "ci"); err != nil {
		t.Fatalf("could not update seeded ProwJob: %v", err)
	}

	// The same arguments now default to AWS/amd64.
	HypershiftSupportedVersions.Mu.Lock()
	HypershiftSupportedVersions.Versions = sets.New("4.20")
	HypershiftSupportedVersions.Mu.Unlock()

	result, err := f.manager.SubmitCluster(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitCluster replay returned error: %v", err)
	}
	if !result.Replayed || result.Cluster.JobID != seeded.Name {
		t.Fatalf("replay result = %#v, want the accepted launch %q", result, seeded.Name)
	}
	if dmCalls.Load() != 0 || f.createCount.Load() != 0 {
		t.Fatalf("replay made DM=%d Prow creates=%d; want neither", dmCalls.Load(), f.createCount.Load())
	}
}

func TestNormalizeJobRequestOptionsRejectsInvalidLaunchOptions(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		jobType JobType
	}{
		{name: "unknown parameter", params: map[string]string{"not-a-real-option": ""}},
		{name: "test parameter is not allowed for launches", params: map[string]string{"test": "e2e"}},
		{name: "comma in a launch parameter value", params: map[string]string{"compact": "a,b"}},
		{name: "comma in a workflow launch parameter value", params: map[string]string{"CUSTOM_ENV": "a,b"}, jobType: JobTypeWorkflowLaunch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobType := tt.jobType
			if jobType == "" {
				jobType = JobTypeInstall
			}
			req := &JobRequest{
				Inputs:      [][]string{{"4.18.2"}},
				Type:        jobType,
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

// fakePullRequestGitHub answers the REST pull request lookup and GetRef. Any
// other client method, including GraphQL queries, panics.
type fakePullRequestGitHub struct {
	github.Client
	pullRequest    github.PullRequest
	pullRequestErr error
	baseTip        string
	refs           []string
	pulls          []string
}

func (f *fakePullRequestGitHub) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	f.pulls = append(f.pulls, fmt.Sprintf("%s/%s#%d", org, repo, number))
	if f.pullRequestErr != nil {
		return nil, f.pullRequestErr
	}
	pr := f.pullRequest
	return &pr, nil
}

func (f *fakePullRequestGitHub) GetRefWithContext(_ context.Context, org, repo, ref string) (string, error) {
	f.refs = append(f.refs, org+"/"+repo+":"+ref)
	return f.baseTip, nil
}

// blockingPullRequestGitHub blocks GetPullRequest until release is closed.
type blockingPullRequestGitHub struct {
	github.Client
	entered chan struct{}
	release chan struct{}
}

func (f *blockingPullRequestGitHub) GetPullRequest(string, string, int) (*github.PullRequest, error) {
	close(f.entered)
	<-f.release
	return &github.PullRequest{}, nil
}

func TestGetPullRequestWithContextAbandonsSlowCall(t *testing.T) {
	client := &blockingPullRequestGitHub{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(client.release)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := getPullRequestWithContext(ctx, client, "openshift", "installer", 123)
		result <- err
	}()
	<-client.entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("getPullRequestWithContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("getPullRequestWithContext() did not return after its context was canceled")
	}
}

func TestResolveAsPullRequestUsesBaseBranchTip(t *testing.T) {
	client := &fakePullRequestGitHub{
		pullRequest: github.PullRequest{
			Base: github.PullRequestBranch{Ref: "main", SHA: "stale-pr-base"},
			Head: github.PullRequestBranch{SHA: "head-sha"},
			User: github.User{Login: "author"},
		},
		baseTip: "branch-tip",
	}
	m := &jobManager{githubClient: client}

	refs, err := m.resolveAsPullRequestWithContext(context.Background(), "openshift/installer#123")
	if err != nil {
		t.Fatalf("resolveAsPullRequestWithContext() error = %v", err)
	}
	if refs.BaseSHA != "branch-tip" {
		t.Fatalf("BaseSHA = %q, want the base-branch tip", refs.BaseSHA)
	}
	if refs.BaseRef != "main" || len(refs.Pulls) != 1 || refs.Pulls[0].SHA != "head-sha" || refs.Pulls[0].Author != "author" {
		t.Fatalf("refs = %#v", refs)
	}
	if len(client.pulls) != 1 || client.pulls[0] != "openshift/installer#123" {
		t.Fatalf("GetPullRequest calls = %v", client.pulls)
	}
	if len(client.refs) != 1 || client.refs[0] != "openshift/installer:heads/main" {
		t.Fatalf("GetRef calls = %v", client.refs)
	}
}

func TestResolveAsPullRequestPreservesGitHubNotFound(t *testing.T) {
	client := &fakePullRequestGitHub{pullRequestErr: github.NewNotFound()}
	m := &jobManager{githubClient: client}

	_, err := m.resolveAsPullRequestWithContext(context.Background(), "openshift/installer#999999")
	if err == nil || !github.IsNotFound(err) {
		t.Fatalf("resolveAsPullRequestWithContext() error = %v, want a wrapped GitHub not-found", err)
	}
}

func TestClassifyLaunchPreparationErrorTreatsGitHubNotFoundAsInvalidArguments(t *testing.T) {
	err := fmt.Errorf("unable to lookup pull request openshift/installer#999999: %w", github.NewNotFound())

	got := classifyLaunchPreparationError(context.Background(), err, ToolErrorCodeLaunchInitialization)
	if got.Code != ToolErrorCodeInvalidArguments || got.Retryable {
		t.Fatalf("classifyLaunchPreparationError() = %#v, want non-retryable INVALID_ARGUMENTS", got)
	}
	if got.JobID != "" {
		t.Fatalf("classifyLaunchPreparationError() job ID = %q, want empty because no ProwJob exists", got.JobID)
	}
}

func TestClassifyLaunchPreparationError(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name          string
		ctx           context.Context
		err           error
		fallback      string
		wantCode      string
		wantRetryable bool
	}{
		{name: "canceled request", ctx: canceled, err: errors.New("anything"), fallback: ToolErrorCodeInvalidArguments, wantCode: ToolErrorCodeLaunchInitialization, wantRetryable: true},
		{name: "deadline", ctx: context.Background(), err: fmt.Errorf("load config: %w", context.DeadlineExceeded), fallback: ToolErrorCodeLaunchInitialization, wantCode: ToolErrorCodeBackendUnavailable, wantRetryable: true},
		{name: "wrapped API timeout", ctx: context.Background(), err: fmt.Errorf("the launch job definition could not be loaded: %w", apierrors.NewTimeoutError("slow", 1)), fallback: ToolErrorCodeLaunchInitialization, wantCode: ToolErrorCodeBackendUnavailable, wantRetryable: true},
		{name: "server error", ctx: context.Background(), err: fmt.Errorf("could not resolve config: %w", apierrors.NewInternalError(errors.New("boom"))), fallback: ToolErrorCodeLaunchInitialization, wantCode: ToolErrorCodeBackendUnavailable, wantRetryable: true},
		{name: "known dependency message", ctx: context.Background(), err: errors.New("unable to lookup registry URL for job"), fallback: ToolErrorCodeLaunchInitialization, wantCode: ToolErrorCodeBackendUnavailable, wantRetryable: true},
		{name: "missing configuration", ctx: context.Background(), err: errors.New("there is no defined configuration for the organization o with repo r and branch b"), fallback: ToolErrorCodeLaunchInitialization, wantCode: ToolErrorCodeLaunchInitialization},
		{name: "invalid input", ctx: context.Background(), err: errors.New("configuration error, unable to find prow job"), fallback: ToolErrorCodeInvalidArguments, wantCode: ToolErrorCodeInvalidArguments},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLaunchPreparationError(tt.ctx, tt.err, tt.fallback)
			if got.Code != tt.wantCode || got.Retryable != tt.wantRetryable || got.JobID != "" {
				t.Fatalf("classifyLaunchPreparationError() = %#v, want code %s retryable %v and no job ID", got, tt.wantCode, tt.wantRetryable)
			}
		})
	}
}
