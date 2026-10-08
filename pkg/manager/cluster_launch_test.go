package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/utils"
	imagefake "github.com/openshift/client-go/image/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	prowfake "sigs.k8s.io/prow/pkg/client/clientset/versioned/fake"
	prowconfig "sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/scheduler/strategy"
)

const launchTestImage = "quay.io/openshift-release-dev/ocp-release:4.18.2"

var prowJobsResource = schema.GroupVersionResource{Group: "prow.k8s.io", Version: "v1", Resource: "prowjobs"}

type staticLaunchProwConfig struct {
	config *prowconfig.Config
}

func (c staticLaunchProwConfig) Config() *prowconfig.Config { return c.config }

type clusterLaunchFixture struct {
	manager     *jobManager
	client      *prowfake.Clientset
	created     atomic.Bool
	createCount atomic.Int32
	createHook  func(k8stesting.Action) (bool, runtime.Object, error)
}

func newClusterLaunchFixture(maxClusters int, objects ...runtime.Object) *clusterLaunchFixture {
	client := prowfake.NewSimpleClientset(objects...)
	loader := staticLaunchProwConfig{config: &prowconfig.Config{
		JobConfig: prowconfig.JobConfig{Periodics: []prowconfig.Periodic{{
			JobBase: prowconfig.JobBase{
				Name:    "periodic-launch",
				Cluster: "build-cluster",
				Labels: map[string]string{
					"job-env":          "aws",
					"job-type":         "launch",
					"config-type":      "modern",
					"job-architecture": "amd64",
				},
				Spec: &corev1.PodSpec{Containers: []corev1.Container{{
					Name: "test",
					Env: []corev1.EnvVar{{
						Name:  "CONFIG_SPEC",
						Value: `{"tests":[{"as":"launch","steps":{"cluster_profile":"default","test":[{"ref":"clusterbot-wait"}]}}]}`,
					}},
				}}},
				UtilityConfig: prowconfig.UtilityConfig{DecorationConfig: &prowapiv1.DecorationConfig{}},
			},
		}}},
	}}
	if maxClusters <= 0 {
		maxClusters = 80
	}
	m := &jobManager{
		requests:         make(map[string]*JobRequest),
		jobs:             make(map[string]*Job),
		launchReady:      true,
		clusterPrefix:    "chat-bot",
		maxClusters:      maxClusters,
		maxAge:           3 * time.Hour,
		prowConfigLoader: loader,
		prowClient:       client.ProwV1(),
		prowNamespace:    "ci",
		clusterClients:   utils.BuildClusterClientConfigMap{"build-cluster": {}},
		prowScheduler:    &strategy.Passthrough{},
		muJob: struct {
			lock    sync.Mutex
			running map[string]struct{}
		}{running: make(map[string]struct{})},
	}
	f := &clusterLaunchFixture{manager: m, client: client}
	client.PrependReactor("create", "prowjobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		f.createCount.Add(1)
		f.created.Store(true)
		if f.createHook != nil {
			return f.createHook(action)
		}
		return false, nil, nil
	})
	client.PrependReactor("get", "prowjobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if f.created.Load() {
			if getAction, ok := action.(k8stesting.GetAction); ok {
				m.tryJob(getAction.GetName())
			}
		}
		return false, nil, nil
	})
	return f
}

func launchTestRequest(user, requestID string, inputs ...string) ClusterLaunchRequest {
	return ClusterLaunchRequest{
		ServicePrincipal: "chai",
		SlackUserID:      user,
		UserName:         "redhat-user",
		RequestID:        requestID,
		Inputs:           inputs,
		Platform:         "aws",
		Architecture:     "amd64",
		Parameters:       map[string]string{"no-spot": ""},
	}
}

func successfulDMRequest(user, requestID string, inputs ...string) (ClusterLaunchRequest, *atomic.Int32) {
	req := launchTestRequest(user, requestID, inputs...)
	var calls atomic.Int32
	req.ResolveDM = func(context.Context) (string, error) {
		calls.Add(1)
		return "D-" + user, nil
	}
	return req, &calls
}

func launchToolError(t *testing.T, err error) *ToolError {
	t.Helper()
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T: %v", err, err)
	}
	return toolErr
}

func addDurableLaunch(f *clusterLaunchFixture, req ClusterLaunchRequest, state prowapiv1.ProwJobState, createdAt time.Time) *prowapiv1.ProwJob {
	fingerprint := launchInputFingerprint(req.Inputs, req.Platform, req.Architecture, req.Parameters)
	keyHash := requestKeyHash(req.ServicePrincipal, req.SlackUserID, req.RequestID)
	inputs, _ := json.Marshal([]JobInput{{Version: "4.18.2", Image: launchTestImage}})
	annotations := map[string]string{
		"ci-chat-bot.openshift.io/mode":            JobTypeLaunch,
		"ci-chat-bot.openshift.io/user":            req.SlackUserID,
		"ci-chat-bot.openshift.io/requesterUserID": req.UserName,
		"ci-chat-bot.openshift.io/channel":         "D-" + req.SlackUserID,
		"ci-chat-bot.openshift.io/platform":        "aws",
		"release.openshift.io/architecture":        "amd64",
		"ci-chat-bot.openshift.io/jobInputs":       string(inputs),
		"ci-chat-bot.openshift.io/jobParams":       paramsToString(req.Parameters),
		"ci-chat-bot.openshift.io/expires":         strconv.Itoa(int((3 * time.Hour).Seconds())),
		annotationRequestKeyHash:                   keyHash,
		annotationInputFingerprint:                 fingerprint,
		annotationRequestSource:                    requestSourceMCP,
	}
	name := deterministicProwJobName(f.manager.clusterPrefix, keyHash)
	pj := &prowapiv1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ci",
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels:            map[string]string{utils.LaunchLabel: "true"},
			Annotations:       annotations,
		},
		Spec:   prowapiv1.ProwJobSpec{Job: "periodic-launch"},
		Status: prowapiv1.ProwJobStatus{State: state, URL: ""},
	}
	if state == prowapiv1.SuccessState || state == prowapiv1.FailureState || state == prowapiv1.AbortedState {
		completedAt := metav1.NewTime(createdAt.Add(time.Minute))
		pj.Status.CompletionTime = &completedAt
	}
	if err := f.client.Tracker().Create(prowJobsResource, pj, "ci"); err != nil {
		panic(fmt.Sprintf("failed to seed durable ProwJob: %v", err))
	}
	return pj
}

func TestSubmitClusterAcceptsProwJobBeforeLogsURLAndPersistsMetadata(t *testing.T) {
	f := newClusterLaunchFixture(2)
	req, dmCalls := successfulDMRequest("U123", "request-accepted", launchTestImage)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := f.manager.SubmitCluster(ctx, req)
	if err != nil {
		t.Fatalf("SubmitCluster returned error: %v", err)
	}
	if result.Replayed || result.Cluster.Status != clusterStatusProvisioning || result.Cluster.LogsURL != "" {
		t.Fatalf("SubmitCluster result = %#v; expected an accepted provisioning job without a logs URL", result)
	}
	if dmCalls.Load() != 1 || f.createCount.Load() != 1 {
		t.Fatalf("DM calls = %d, Prow creates = %d; want one each", dmCalls.Load(), f.createCount.Load())
	}
	if result.Cluster.JobID != deterministicProwJobName(f.manager.clusterPrefix, requestKeyHash("chai", "U123", req.RequestID)) {
		t.Fatalf("job ID = %q; expected a deterministic request-scoped ID", result.Cluster.JobID)
	}

	pj, err := f.client.ProwV1().ProwJobs("ci").Get(context.Background(), result.Cluster.JobID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("could not read accepted ProwJob: %v", err)
	}
	wantFingerprint := launchInputFingerprint(req.Inputs, req.Platform, req.Architecture, req.Parameters)
	wantKeyHash := requestKeyHash(req.ServicePrincipal, req.SlackUserID, req.RequestID)
	wantExpiry := strconv.Itoa(int((f.manager.maxAge + 45*time.Minute).Seconds()))
	for annotation, want := range map[string]string{
		"ci-chat-bot.openshift.io/user":            "U123",
		"ci-chat-bot.openshift.io/requesterUserID": "redhat-user",
		"ci-chat-bot.openshift.io/channel":         "D-U123",
		"ci-chat-bot.openshift.io/mode":            JobTypeLaunch,
		annotationRequestKeyHash:                   wantKeyHash,
		annotationInputFingerprint:                 wantFingerprint,
		annotationRequestSource:                    requestSourceMCP,
		"ci-chat-bot.openshift.io/expires":         wantExpiry,
	} {
		if got := pj.Annotations[annotation]; got != want {
			t.Errorf("annotation %q = %q, want %q", annotation, got, want)
		}
	}
	if pj.Labels[utils.LaunchLabel] != "true" {
		t.Errorf("launch label = %q, want true", pj.Labels[utils.LaunchLabel])
	}
	annotationData, _ := json.Marshal(pj.Annotations)
	if strings.Contains(string(annotationData), req.RequestID) {
		t.Errorf("ProwJob annotations contain raw request ID %q", req.RequestID)
	}
}

func TestSubmitClusterReplaysTerminalProwJobAfterRestartBeforeDMOrResolution(t *testing.T) {
	req, dmCalls := successfulDMRequest("U123", "request-replay", "nightly")
	f := newClusterLaunchFixture(2)
	seeded := addDurableLaunch(f, req, prowapiv1.SuccessState, time.Now().Add(-2*time.Minute))

	// A missing release-image client proves recovery happens before dynamic
	// input resolution. The replay must also avoid opening a second Slack DM.
	imageClient := imagefake.NewClientset()
	var imageLookups atomic.Int32
	imageClient.PrependReactor("get", "imagestreams", func(k8stesting.Action) (bool, runtime.Object, error) {
		imageLookups.Add(1)
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "imagestreams"}, "release")
	})
	f.manager.imageClient = imageClient
	result, err := f.manager.SubmitCluster(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitCluster replay returned error: %v", err)
	}
	if !result.Replayed || result.Cluster.JobID != seeded.Name || result.Cluster.Status != clusterStatusTerminated {
		t.Fatalf("replayed terminal result = %#v", result)
	}
	if dmCalls.Load() != 0 || imageLookups.Load() != 0 || f.createCount.Load() != 0 {
		t.Fatalf("replay did unexpected work: DM calls=%d image lookups=%d creates=%d", dmCalls.Load(), imageLookups.Load(), f.createCount.Load())
	}
}

func TestSubmitClusterScopesRequestIDByPrincipalAndSlackUser(t *testing.T) {
	for _, tc := range []struct {
		name            string
		seedPrincipal   string
		seedUser        string
		submitPrincipal string
		submitUser      string
	}{
		{name: "service principal", seedPrincipal: "chai", seedUser: "U123", submitPrincipal: "another-service", submitUser: "U123"},
		{name: "Slack user", seedPrincipal: "chai", seedUser: "U123", submitPrincipal: "chai", submitUser: "U456"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterLaunchFixture(2)
			original := launchTestRequest(tc.seedUser, "request-scoped", launchTestImage)
			original.ServicePrincipal = tc.seedPrincipal
			seeded := addDurableLaunch(f, original, prowapiv1.SuccessState, time.Now().Add(-2*time.Minute))

			request, dmCalls := successfulDMRequest(tc.submitUser, "request-scoped", launchTestImage)
			request.ServicePrincipal = tc.submitPrincipal
			result, err := f.manager.SubmitCluster(context.Background(), request)
			if err != nil {
				t.Fatalf("SubmitCluster returned error: %v", err)
			}
			if result.Replayed || result.Cluster.JobID == seeded.Name {
				t.Fatalf("request ID crossed its %s scope: %#v, seeded job %q", tc.name, result, seeded.Name)
			}
			if dmCalls.Load() != 1 || f.createCount.Load() != 1 {
				t.Fatalf("scoped request did not create independently: DM=%d Prow creates=%d", dmCalls.Load(), f.createCount.Load())
			}
		})
	}
}

func TestSubmitClusterRejectsConflictingArgumentsForExistingRequestID(t *testing.T) {
	f := newClusterLaunchFixture(2)
	first, dmCalls := successfulDMRequest("U123", "request-conflict", launchTestImage)
	if _, err := f.manager.SubmitCluster(context.Background(), first); err != nil {
		t.Fatalf("initial SubmitCluster returned error: %v", err)
	}
	second, _ := successfulDMRequest("U123", "request-conflict", "quay.io/openshift-release-dev/ocp-release:4.19.0")
	_, err := f.manager.SubmitCluster(context.Background(), second)
	toolErr := launchToolError(t, err)
	if toolErr.Code != ToolErrorCodeRequestConflict || toolErr.Retryable {
		t.Fatalf("conflicting request error = %#v", toolErr)
	}
	if dmCalls.Load() != 1 || f.createCount.Load() != 1 {
		t.Fatalf("conflict should not open another DM or create a ProwJob: DM calls=%d creates=%d", dmCalls.Load(), f.createCount.Load())
	}
}

func TestSubmitClusterReconcilesAlreadyExistsAndAmbiguousCreateErrors(t *testing.T) {
	tests := []struct {
		name      string
		createErr error
	}{
		{name: "already exists", createErr: apierrors.NewAlreadyExists(prowJobsResource.GroupResource(), "pending")},
		{name: "connection reset", createErr: errors.New("connection reset by peer")},
		{name: "ambiguous timeout", createErr: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterLaunchFixture(2)
			req, dmCalls := successfulDMRequest("U123", "request-"+stringsToName(tc.name), launchTestImage)
			f.createHook = func(action k8stesting.Action) (bool, runtime.Object, error) {
				createAction := action.(k8stesting.CreateAction)
				obj := createAction.GetObject().DeepCopyObject()
				if err := f.client.Tracker().Create(prowJobsResource, obj, "ci"); err != nil {
					return true, nil, err
				}
				return true, nil, tc.createErr
			}

			result, err := f.manager.SubmitCluster(context.Background(), req)
			if err != nil {
				t.Fatalf("SubmitCluster did not reconcile %s: %v", tc.name, err)
			}
			if !result.Replayed || result.Cluster.JobID == "" {
				t.Fatalf("reconciled result = %#v; expected the durable job to be replayed", result)
			}
			if dmCalls.Load() != 1 || f.createCount.Load() != 1 {
				t.Fatalf("unexpected calls after reconciliation: DM=%d create=%d", dmCalls.Load(), f.createCount.Load())
			}
		})
	}
}

func stringsToName(value string) string {
	result := make([]rune, 0, len(value))
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			result = append(result, r)
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}

func TestSubmitClusterConcurrentIdenticalRequestsCreateAtMostOneProwJob(t *testing.T) {
	f := newClusterLaunchFixture(2)
	request, dmCalls := successfulDMRequest("U123", "request-concurrent-same", launchTestImage)
	dmEntered := make(chan struct{}, 2)
	releaseDM := make(chan struct{})
	request.ResolveDM = func(context.Context) (string, error) {
		dmCalls.Add(1)
		dmEntered <- struct{}{}
		<-releaseDM
		return "D-U123", nil
	}

	start := make(chan struct{})
	type outcome struct {
		result LaunchResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			<-start
			result, err := f.manager.SubmitCluster(context.Background(), request)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	<-dmEntered

	var first outcome
	select {
	case first = <-outcomes:
	case <-time.After(5 * time.Second):
		close(releaseDM)
		workers.Wait()
		t.Fatal("second identical request did not return while the first was resolving its DM")
	}
	if first.err == nil {
		t.Fatal("concurrent identical submission unexpectedly created two in-flight launch attempts")
	}
	toolErr := launchToolError(t, first.err)
	if toolErr.Code != ToolErrorCodeLaunchInitialization || !toolErr.Retryable {
		t.Fatalf("concurrent duplicate error = %#v", toolErr)
	}
	close(releaseDM)
	second := <-outcomes
	workers.Wait()
	if second.err != nil || second.result.Cluster.JobID == "" || second.result.Replayed {
		t.Fatalf("first accepted submission = result %#v, error %v", second.result, second.err)
	}
	if dmCalls.Load() != 1 || f.createCount.Load() != 1 {
		t.Fatalf("identical concurrent requests made DM=%d Prow creates=%d; want one each", dmCalls.Load(), f.createCount.Load())
	}
}

func TestSubmitClusterConcurrentUsersRespectCapacityWithoutStaleReservation(t *testing.T) {
	f := newClusterLaunchFixture(1)
	dmEntered := make(chan struct{}, 2)
	releaseDM := make(chan struct{})
	var dmCalls atomic.Int32
	requestFor := func(user, requestID string) ClusterLaunchRequest {
		req := launchTestRequest(user, requestID, launchTestImage)
		req.ResolveDM = func(context.Context) (string, error) {
			dmCalls.Add(1)
			dmEntered <- struct{}{}
			<-releaseDM
			return "D-" + user, nil
		}
		return req
	}

	start := make(chan struct{})
	type outcome struct {
		user   string
		result LaunchResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for _, request := range []ClusterLaunchRequest{
		requestFor("U123", "request-capacity-a"),
		requestFor("U456", "request-capacity-b"),
	} {
		go func() {
			defer workers.Done()
			<-start
			result, err := f.manager.SubmitCluster(context.Background(), request)
			outcomes <- outcome{user: request.SlackUserID, result: result, err: err}
		}()
	}
	close(start)
	<-dmEntered

	var rejected outcome
	select {
	case rejected = <-outcomes:
	case <-time.After(5 * time.Second):
		close(releaseDM)
		workers.Wait()
		t.Fatal("capacity rejection did not occur while the winning request was waiting for its DM")
	}
	if rejected.err == nil {
		t.Fatal("both concurrent users were admitted with capacity one")
	}
	rejectedError := launchToolError(t, rejected.err)
	if rejectedError.Code != ToolErrorCodeCapacityExhausted || !rejectedError.Retryable {
		t.Fatalf("capacity error = %#v", rejectedError)
	}
	close(releaseDM)
	accepted := <-outcomes
	workers.Wait()
	if accepted.err != nil || accepted.result.Cluster.JobID == "" {
		t.Fatalf("winning capacity request failed: result=%#v error=%v", accepted.result, accepted.err)
	}
	if dmCalls.Load() != 1 || f.createCount.Load() != 1 {
		t.Fatalf("capacity race made DM=%d Prow creates=%d; want one each", dmCalls.Load(), f.createCount.Load())
	}
	f.manager.lock.RLock()
	defer f.manager.lock.RUnlock()
	if f.manager.requests[rejected.user] != nil {
		t.Fatalf("rejected user %s retained a stale reservation: %#v", rejected.user, f.manager.requests[rejected.user])
	}
	if current := f.manager.requests[accepted.user]; current == nil || current.Name != accepted.result.Cluster.JobID {
		t.Fatalf("accepted reservation missing for %s: %#v", accepted.user, current)
	}
}

func TestSubmitClusterSharesAdmissionWithSlackAndRejectsSecondUserRequest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seedOwner string
		requester string
		wantCode  string
	}{
		{name: "Slack reservation blocks MCP", seedOwner: "U123", requester: "U123", wantCode: ToolErrorCodeActiveClusterExists},
		{name: "another user's reservation does not block per-user admission", seedOwner: "U456", requester: "U123", wantCode: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterLaunchFixture(5)
			seedName := "slack-active-" + tc.seedOwner
			seed := cachedLifecycleJob(seedName, tc.seedOwner, time.Now())
			seed.Credentials = ""
			seed.CredentialsSnippet = ""
			f.manager.jobs[seedName] = seed
			f.manager.requests[tc.seedOwner] = &JobRequest{User: tc.seedOwner, Name: seedName, Type: JobTypeInstall, RequestedAt: time.Now()}

			req, dmCalls := successfulDMRequest(tc.requester, "request-shared-admission-"+tc.requester+tc.seedOwner, launchTestImage)
			result, err := f.manager.SubmitCluster(context.Background(), req)
			if tc.wantCode != "" {
				toolErr := launchToolError(t, err)
				if toolErr.Code != tc.wantCode || dmCalls.Load() != 0 || f.createCount.Load() != 0 {
					t.Fatalf("shared admission error=%#v dm=%d creates=%d", toolErr, dmCalls.Load(), f.createCount.Load())
				}
				if f.manager.requests[tc.requester] == nil || f.manager.requests[tc.requester].Name != seedName {
					t.Fatalf("existing shared reservation was modified: %#v", f.manager.requests[tc.requester])
				}
				return
			}
			if err != nil || result.Cluster.JobID == "" || f.createCount.Load() != 1 {
				t.Fatalf("other user's reservation blocked launch: result=%#v error=%v creates=%d", result, err, f.createCount.Load())
			}
		})
	}

	// A second request ID from the same user is also rejected after an MCP
	// launch has been accepted, matching the Slack per-user limit.
	f := newClusterLaunchFixture(5)
	first, dmCalls := successfulDMRequest("U123", "request-user-slot-one", launchTestImage)
	if _, err := f.manager.SubmitCluster(context.Background(), first); err != nil {
		t.Fatalf("first launch returned error: %v", err)
	}
	second, _ := successfulDMRequest("U123", "request-user-slot-two", launchTestImage)
	_, err := f.manager.SubmitCluster(context.Background(), second)
	toolErr := launchToolError(t, err)
	if toolErr.Code != ToolErrorCodeActiveClusterExists || dmCalls.Load() != 1 || f.createCount.Load() != 1 {
		t.Fatalf("second user launch was not blocked: error=%#v dm=%d creates=%d", toolErr, dmCalls.Load(), f.createCount.Load())
	}
}

func TestSubmitClusterDMFailureRollsBackOnlyItsReservation(t *testing.T) {
	f := newClusterLaunchFixture(2)
	req := launchTestRequest("U123", "request-dm-failure", launchTestImage)
	req.ResolveDM = func(context.Context) (string, error) { return "", errors.New("Slack unavailable") }
	_, err := f.manager.SubmitCluster(context.Background(), req)
	toolErr := launchToolError(t, err)
	if toolErr.Code != ToolErrorCodeBackendUnavailable || !toolErr.Retryable {
		t.Fatalf("DM failure = %#v", toolErr)
	}
	if f.manager.requests["U123"] != nil || len(f.manager.jobs) != 0 || f.createCount.Load() != 0 {
		t.Fatalf("failed pre-submission request left state: requests=%#v jobs=%#v creates=%d", f.manager.requests, f.manager.jobs, f.createCount.Load())
	}
}
