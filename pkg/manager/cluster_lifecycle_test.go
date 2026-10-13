package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	prowfake "sigs.k8s.io/prow/pkg/client/clientset/versioned/fake"
	prowjoblister "sigs.k8s.io/prow/pkg/client/listers/prowjobs/v1"
)

func lifecycleProwJob(name, user, mode string, state prowapiv1.ProwJobState, requestedAt time.Time) *prowapiv1.ProwJob {
	inputs, _ := json.Marshal([]JobInput{{Version: "4.18.2"}})
	return &prowapiv1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ci",
			CreationTimestamp: metav1.NewTime(requestedAt),
			Labels:            map[string]string{utils.LaunchLabel: "true"},
			Annotations: map[string]string{
				"ci-chat-bot.openshift.io/user":            user,
				"ci-chat-bot.openshift.io/mode":            mode,
				"ci-chat-bot.openshift.io/jobInputs":       string(inputs),
				"ci-chat-bot.openshift.io/jobParams":       "compact",
				"ci-chat-bot.openshift.io/platform":        "aws",
				"ci-chat-bot.openshift.io/requesterUserID": "developer",
				"ci-chat-bot.openshift.io/expires":         "10800",
			},
		},
		Spec: prowapiv1.ProwJobSpec{Job: "periodic-launch"},
		Status: prowapiv1.ProwJobStatus{
			State: state,
			URL:   "https://prow.example/job/" + name,
		},
	}
}

// newLifecycleManager serves pjs from both a fake Prow client and an informer
// cache. The cache is a snapshot; later client updates do not reach it.
func newLifecycleManager(pjs ...runtime.Object) (*jobManager, *prowfake.Clientset) {
	client := prowfake.NewSimpleClientset(pjs...)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pj := range pjs {
		if err := indexer.Add(pj.DeepCopyObject()); err != nil {
			panic(fmt.Sprintf("could not add ProwJob to lifecycle lister: %v", err))
		}
	}
	m := &jobManager{
		prowClient:    client.ProwV1(),
		prowLister:    prowjoblister.NewProwJobLister(indexer),
		prowNamespace: "ci",
		jobs:          make(map[string]*Job),
		requests:      make(map[string]*JobRequest),
	}
	return m, client
}

func cachedLifecycleJob(name, user string, requestedAt time.Time) *Job {
	return &Job{
		Name:               name,
		Mode:               JobTypeLaunch,
		RequestedBy:        user,
		RequesterUserID:    "developer",
		RequestedAt:        requestedAt,
		ExpiresAt:          requestedAt.Add(3 * time.Hour),
		State:              prowapiv1.PendingState,
		JobParams:          map[string]string{"compact": ""},
		Inputs:             []JobInput{{Version: "4.18.2", Refs: []prowapiv1.Refs{{Org: "openshift", Repo: "installer", Pulls: []prowapiv1.Pull{{Number: 123, Title: "change"}}}}}},
		Credentials:        "apiVersion: v1\nkind: Config\n",
		CredentialsSnippet: "Slack-only credential rendering",
		ConsoleURL:         "https://console.example",
		APIURL:             "https://api.example:6443",
		ConsoleUsername:    "kubeadmin",
		ConsolePassword:    "secret-password",
		AccessInstructions: "Use the returned kubeconfig.",
		RequestedChannel:   "D123",
	}
}

func toolErrorFrom(t *testing.T, err error) ToolError {
	t.Helper()
	var toolErr ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T: %v", err, err)
	}
	return toolErr
}

func TestGetClusterStatusAndCredentialsUsePersistedTerminalState(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-a", "U123", JobTypeLaunch, prowapiv1.SuccessState, now.Add(-time.Minute))
	m, _ := newLifecycleManager(pj)
	m.jobs[pj.Name] = cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))

	summary, err := m.GetClusterStatus(context.Background(), "U123", pj.Name)
	if err != nil {
		t.Fatalf("GetClusterStatus returned error: %v", err)
	}
	if summary.Status != clusterStatusTerminated {
		t.Fatalf("status = %q, want %q", summary.Status, clusterStatusTerminated)
	}
	if summary.ConsoleURL != "" || summary.APIURL != "" || strings.Contains(summary.Failure, "secret-password") {
		t.Fatalf("terminal summary exposed cached access details: %#v", summary)
	}

	_, err = m.GetClusterCredentials(context.Background(), "U123", pj.Name)
	toolErr := toolErrorFrom(t, err)
	if toolErr.Code != ToolErrorCodeCredentialsUnavailable || toolErr.Retryable {
		t.Fatalf("terminal credential error = %#v, want non-retryable CREDENTIALS_UNAVAILABLE", toolErr)
	}

	unknown, err := m.GetClusterStatus(context.Background(), "U456", pj.Name)
	if err == nil {
		t.Fatalf("other owner got status: %#v", unknown)
	}
	otherOwner := toolErrorFrom(t, err)
	_, err = m.GetClusterStatus(context.Background(), "U456", "missing-job")
	missing := toolErrorFrom(t, err)
	if otherOwner.Code != ToolErrorCodeNotFound || missing.Code != ToolErrorCodeNotFound || otherOwner.Message != missing.Message {
		t.Fatalf("ownership errors differ: other=%#v missing=%#v", otherOwner, missing)
	}
}

func TestGetClusterCredentialsReturnsStructuredReadyAccess(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-ready", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-time.Minute))
	m, _ := newLifecycleManager(pj)
	job := cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))
	m.jobs[pj.Name] = job

	credentials, err := m.GetClusterCredentials(context.Background(), "U123", pj.Name)
	if err != nil {
		t.Fatalf("GetClusterCredentials returned error: %v", err)
	}
	if credentials.Kubeconfig != job.Credentials || credentials.ConsoleURL != job.ConsoleURL || credentials.APIURL != job.APIURL || credentials.ConsolePassword != job.ConsolePassword {
		t.Fatalf("structured credentials do not match monitor state: %#v", credentials)
	}

	summary, err := m.GetClusterStatus(context.Background(), "U123", pj.Name)
	if err != nil {
		t.Fatalf("GetClusterStatus returned error: %v", err)
	}
	if summary.Status != clusterStatusReady || summary.ConsoleURL != job.ConsoleURL || summary.APIURL != job.APIURL {
		t.Fatalf("ready summary missing non-secret access URLs: %#v", summary)
	}
	serialized, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("could not serialize summary: %v", err)
	}
	for _, secret := range []string{job.Credentials, job.ConsolePassword, job.CredentialsSnippet} {
		if secret != "" && strings.Contains(string(serialized), secret) {
			t.Fatalf("summary leaked a secret or Slack credential snippet: %q", secret)
		}
	}

	// Returned DTO collections must not alias manager-owned maps or nested refs.
	summary.Parameters["compact"] = "changed"
	summary.Inputs[0][0] = "changed"
	credentials.ConsolePassword = "changed"
	if job.JobParams["compact"] != "" || job.Inputs[0].Version != "4.18.2" || job.ConsolePassword != "secret-password" {
		t.Fatal("returned DTO mutation changed cached job state")
	}
}

func TestGetClusterCredentialsUnavailableRetryability(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		state      prowapiv1.ProwJobState
		marker     bool
		completion bool
		requested  time.Time
		expires    string
		wantStatus string
		wantRetry  bool
	}{
		{name: "restart recovery", state: prowapiv1.PendingState, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusProvisioning, wantRetry: true},
		{name: "failed", state: prowapiv1.FailureState, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusFailed},
		{name: "terminating", state: prowapiv1.AbortedState, marker: true, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusTerminating},
		{name: "aborted without our marker", state: prowapiv1.AbortedState, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusTerminated},
		{name: "completed abort", state: prowapiv1.AbortedState, marker: true, completion: true, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusTerminated},
		{name: "completed failure after destroy request", state: prowapiv1.FailureState, marker: true, completion: true, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusFailed},
		{name: "terminated", state: prowapiv1.SuccessState, requested: now.Add(-time.Minute), expires: "10800", wantStatus: clusterStatusTerminated},
		{name: "expired", state: prowapiv1.PendingState, requested: now.Add(-time.Hour), expires: "1", wantStatus: clusterStatusExpired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pj := lifecycleProwJob("cluster", "U123", JobTypeLaunch, tc.state, tc.requested)
			pj.Annotations["ci-chat-bot.openshift.io/expires"] = tc.expires
			if tc.marker {
				pj.Annotations[annotationTerminationRequested] = terminationMarkerValue
			}
			if tc.completion {
				completedAt := metav1.NewTime(now)
				pj.Status.CompletionTime = &completedAt
			}
			m, _ := newLifecycleManager(pj)
			_, err := m.GetClusterCredentials(context.Background(), "U123", pj.Name)
			toolErr := toolErrorFrom(t, err)
			if toolErr.Code != ToolErrorCodeCredentialsUnavailable || toolErr.Retryable != tc.wantRetry {
				t.Fatalf("credential error = %#v, want retryable=%v", toolErr, tc.wantRetry)
			}
			summary, err := m.GetClusterStatus(context.Background(), "U123", pj.Name)
			if err != nil {
				t.Fatalf("GetClusterStatus returned error: %v", err)
			}
			if summary.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", summary.Status, tc.wantStatus)
			}
		})
	}
}

func TestListClustersIncludesOnlyActiveOrdinaryLaunchesForOwner(t *testing.T) {
	now := time.Now().UTC()
	owned := lifecycleProwJob("owned", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-time.Minute))
	other := lifecycleProwJob("other", "U456", JobTypeLaunch, prowapiv1.PendingState, now.Add(-2*time.Minute))
	workflow := lifecycleProwJob("workflow", "U123", JobTypeWorkflowLaunch, prowapiv1.PendingState, now.Add(-3*time.Minute))
	failed := lifecycleProwJob("failed", "U123", JobTypeLaunch, prowapiv1.FailureState, now.Add(-4*time.Minute))
	terminated := lifecycleProwJob("terminated", "U123", JobTypeLaunch, prowapiv1.SuccessState, now.Add(-5*time.Minute))
	malformed := lifecycleProwJob("malformed", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-6*time.Minute))
	malformed.Annotations["ci-chat-bot.openshift.io/jobInputs"] = "{not json"
	m, client := newLifecycleManager(owned, other, workflow, failed, terminated, malformed)
	m.jobs[owned.Name] = cachedLifecycleJob(owned.Name, "U123", now.Add(-time.Minute))
	client.PrependReactor("list", "prowjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("ListClusters must read the informer cache")
	})

	clusters, err := m.ListClusters(context.Background(), "U123")
	if err != nil {
		t.Fatalf("ListClusters returned error: %v", err)
	}
	if len(clusters) != 1 || clusters[0].JobID != owned.Name {
		t.Fatalf("ListClusters returned %#v, want only %q", clusters, owned.Name)
	}
	if clusters[0].Status != clusterStatusReady || clusters[0].ConsoleURL != "https://console.example" {
		t.Fatalf("ready cluster summary lost cached access: %#v", clusters[0])
	}
}

func TestListClustersIncludesLaunchNotYetInCache(t *testing.T) {
	now := time.Now().UTC()
	m, _ := newLifecycleManager()
	accepted := cachedLifecycleJob("accepted", "U123", now)
	clearJobAccess(accepted)
	accepted.URL = "https://prow.example/job/accepted"
	m.jobs[accepted.Name] = accepted
	submitting := cachedLifecycleJob("submitting", "U456", now)
	clearJobAccess(submitting)
	m.jobs[submitting.Name] = submitting
	m.requests["U456"] = &JobRequest{User: "U456", Name: submitting.Name, SubmissionInProgress: true}
	finished := cachedLifecycleJob("finished", "U123", now.Add(-time.Hour))
	finished.Complete = true
	finished.Failure = "job failed, see logs"
	m.jobs[finished.Name] = finished

	clusters, err := m.ListClusters(context.Background(), "U123")
	if err != nil {
		t.Fatalf("ListClusters returned error: %v", err)
	}
	if len(clusters) != 1 || clusters[0].JobID != accepted.Name || clusters[0].Status != clusterStatusProvisioning || clusters[0].LogsURL != accepted.URL {
		t.Fatalf("ListClusters returned %#v, want the accepted launch as provisioning", clusters)
	}
	clusters, err = m.ListClusters(context.Background(), "U456")
	if err != nil || len(clusters) != 0 {
		t.Fatalf("ListClusters returned %#v, %v; want no launch still being submitted", clusters, err)
	}
}

func TestDestroyClusterTargetsExactJobAndRetriesConflicts(t *testing.T) {
	now := time.Now().UTC()
	old := lifecycleProwJob("old-cluster", "U123", JobTypeLaunch, prowapiv1.TriggeredState, now.Add(-time.Minute))
	newer := lifecycleProwJob("new-cluster", "U123", JobTypeLaunch, prowapiv1.PendingState, now)
	m, client := newLifecycleManager(old, newer)
	m.jobs[old.Name] = cachedLifecycleJob(old.Name, "U123", now.Add(-time.Minute))
	m.jobs[newer.Name] = cachedLifecycleJob(newer.Name, "U123", now)
	m.requests["U123"] = &JobRequest{User: "U123", Name: newer.Name}

	conflicts := 0
	client.PrependReactor("update", "prowjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(prowapiv1.Resource("prowjobs"), old.Name, errors.New("concurrent update"))
		}
		return false, nil, nil
	})

	result, err := m.DestroyCluster(context.Background(), "U123", old.Name)
	if err != nil {
		t.Fatalf("DestroyCluster returned error: %v", err)
	}
	if conflicts != 1 || result.JobID != old.Name || result.Status != clusterStatusTerminating {
		t.Fatalf("result = %#v, conflicts = %d", result, conflicts)
	}
	storedOld, err := client.ProwV1().ProwJobs("ci").Get(context.Background(), old.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("could not read retained old ProwJob: %v", err)
	}
	if storedOld.Status.State != prowapiv1.AbortedState || storedOld.Annotations[annotationTerminationRequested] != terminationMarkerValue {
		t.Fatalf("old ProwJob was not marked for termination: %#v", storedOld.Status)
	}
	storedNew, err := client.ProwV1().ProwJobs("ci").Get(context.Background(), newer.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("could not read newer ProwJob: %v", err)
	}
	if storedNew.Status.State != prowapiv1.PendingState {
		t.Fatalf("newer ProwJob was changed: state=%q", storedNew.Status.State)
	}
	if m.requests["U123"] == nil || m.requests["U123"].Name != newer.Name {
		t.Fatalf("destroying old ID cleared newer reservation: %#v", m.requests["U123"])
	}

	// A second request is safe and retains the ProwJob for status/history.
	again, err := m.DestroyCluster(context.Background(), "U123", old.Name)
	if err != nil {
		t.Fatalf("repeated DestroyCluster returned error: %v", err)
	}
	if again != result {
		t.Fatalf("repeated destroy result = %#v, want %#v", again, result)
	}
}

func TestFinishedJobDoesNotRestoreCredentialsAfterDestroy(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-race", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-time.Minute))
	m, _ := newLifecycleManager(pj)
	stale := cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))
	m.jobs[pj.Name] = cloneJob(stale)
	m.requests["U123"] = &JobRequest{User: "U123", Name: pj.Name}

	if _, err := m.DestroyCluster(context.Background(), "U123", pj.Name); err != nil {
		t.Fatalf("DestroyCluster returned error: %v", err)
	}
	notifications := make(chan struct{}, 1)
	m.SetNotifier(func(Job) { notifications <- struct{}{} })
	m.finishedJob(*stale)

	current := m.jobs[pj.Name]
	if current == nil || !current.TerminationRequested || current.Credentials != "" || current.ConsolePassword != "" || current.CredentialsSnippet != "" {
		t.Fatalf("late monitor restored access after destroy: %#v", current)
	}
	select {
	case <-notifications:
		t.Fatal("late monitor sent a notification after destroy")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestFinishedJobDefersWhenProwStateCannotBeVerified(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-offline", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-time.Minute))
	m, client := newLifecycleManager(pj)
	stale := cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))
	current := cloneJob(stale)
	current.Credentials = ""
	current.ConsolePassword = ""
	current.CredentialsSnippet = ""
	m.jobs[pj.Name] = current

	client.PrependReactor("get", "prowjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("Prow unavailable")
	})
	notifications := make(chan struct{}, 1)
	m.SetNotifier(func(Job) { notifications <- struct{}{} })
	if m.tryFinishedJob(*stale) {
		t.Fatal("unavailable Prow state did not request a retry")
	}

	stored := m.jobs[pj.Name]
	if stored.Credentials != "" || stored.ConsolePassword != "" || stored.CredentialsSnippet != "" {
		t.Fatalf("unverified monitor result replaced the cache: %#v", stored)
	}
	select {
	case <-notifications:
		t.Fatal("monitor notified while Prow state could not be verified")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestConcurrentDestroyCredentialsAndMonitorCompletion(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-concurrent", "U123", JobTypeLaunch, prowapiv1.TriggeredState, now.Add(-time.Minute))
	m, client := newLifecycleManager(pj)
	stale := cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))
	m.jobs[pj.Name] = cloneJob(stale)
	m.requests["U123"] = &JobRequest{User: "U123", Name: pj.Name}

	firstGetEntered := make(chan struct{})
	releaseFirstGet := make(chan struct{})
	var getCalls atomic.Int32
	client.PrependReactor("get", "prowjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if getCalls.Add(1) == 1 {
			close(firstGetEntered)
			<-releaseFirstGet
		}
		return false, nil, nil
	})
	destroyUpdateEntered := make(chan struct{}, 1)
	client.PrependReactor("update", "prowjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		destroyUpdateEntered <- struct{}{}
		return false, nil, nil
	})

	credentialsStarted := make(chan struct{})
	credentialsDone := make(chan struct{})
	go func() {
		close(credentialsStarted)
		_, _ = m.GetClusterCredentials(context.Background(), "U123", pj.Name)
		close(credentialsDone)
	}()
	<-credentialsStarted
	<-firstGetEntered

	destroyStarted := make(chan struct{})
	destroyDone := make(chan struct{})
	go func() {
		close(destroyStarted)
		_, _ = m.DestroyCluster(context.Background(), "U123", pj.Name)
		close(destroyDone)
	}()
	<-destroyStarted
	monitorStarted := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		close(monitorStarted)
		m.finishedJob(*stale)
		close(monitorDone)
	}()
	<-monitorStarted

	select {
	case <-destroyUpdateEntered:
		t.Fatal("destroy updated Prow while credential retrieval held the lifecycle lock")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-destroyDone:
		t.Fatal("destroy completed before the in-flight credential read was released")
	default:
	}
	close(releaseFirstGet)
	<-credentialsDone
	<-destroyDone
	<-monitorDone

	stored := m.jobs[pj.Name]
	if stored == nil || !stored.TerminationRequested || stored.Credentials != "" || stored.ConsolePassword != "" || stored.CredentialsSnippet != "" {
		t.Fatalf("concurrent monitor work restored access after destroy: %#v", stored)
	}
}

func TestGetLaunchJobDeepCopiesMutableFields(t *testing.T) {
	m := &jobManager{
		jobs: map[string]*Job{"cluster": cachedLifecycleJob("cluster", "U123", time.Now())},
		requests: map[string]*JobRequest{
			"U123": {User: "U123", Name: "cluster"},
		},
	}
	job, err := m.GetLaunchJob("U123")
	if err != nil {
		t.Fatalf("GetLaunchJob returned error: %v", err)
	}
	job.JobParams["compact"] = "changed"
	job.Inputs[0].Version = "changed"
	job.Inputs[0].Refs[0].Pulls[0].Title = "changed"
	job.CompletedAt = new(time.Time)
	if m.jobs["cluster"].JobParams["compact"] != "" ||
		m.jobs["cluster"].Inputs[0].Version != "4.18.2" ||
		m.jobs["cluster"].Inputs[0].Refs[0].Pulls[0].Title != "change" ||
		m.jobs["cluster"].CompletedAt != nil {
		t.Fatal("GetLaunchJob returned mutable fields that alias the manager cache")
	}

	userCluster := m.GetUserCluster("U123")
	if userCluster == nil {
		t.Fatal("GetUserCluster returned no active cluster")
	}
	userCluster.JobParams["compact"] = "modal mutation"
	userCluster.Inputs[0].Refs[0].Pulls[0].Title = "modal mutation"
	if m.jobs["cluster"].JobParams["compact"] != "" || m.jobs["cluster"].Inputs[0].Refs[0].Pulls[0].Title != "change" {
		t.Fatal("GetUserCluster returned mutable fields that alias the manager cache")
	}
}

func TestAccessDetailsUseCurrentKubeconfigContextAndMetalProxy(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: old
  cluster:
    server: https://api.old.example:6443
- name: active
  cluster:
    server: https://api.active.example:6443
    proxy-url: http://192.0.2.10:8213/
contexts:
- name: old
  context:
    cluster: old
    user: old
- name: active-context
  context:
    cluster: active
    user: active
current-context: active-context
users:
- name: old
  user:
    token: old-token
- name: active
  user:
    token: active-token
`
	details, err := accessDetailsFromSecret(kubeconfig, map[string][]byte{
		"console.url":        []byte("https://console.active.example\n"),
		"kubeadmin-password": []byte("console-password\n"),
	}, "metal")
	if err != nil {
		t.Fatalf("accessDetailsFromSecret returned error: %v", err)
	}
	if details.APIURL != "https://api.active.example:6443" || details.ConsoleURL != "https://console.active.example" {
		t.Fatalf("access details used the wrong endpoint: %#v", details)
	}
	if details.ConsoleUsername != "kubeadmin" || details.ConsolePassword != "console-password" {
		t.Fatalf("console credentials were not normalized: %#v", details)
	}
	if !strings.Contains(details.AccessInstructions, "http_proxy` and `https_proxy") || !strings.Contains(details.AccessInstructions, "192.0.2.10:8213") {
		t.Fatalf("metal proxy instructions missing: %q", details.AccessInstructions)
	}
	if strings.Contains(details.AccessInstructions, details.ConsolePassword) {
		t.Fatalf("access instructions embedded the secret password: %q", details.AccessInstructions)
	}
}

func TestAccessDetailsRejectMalformedKubeconfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kubeconfig string
	}{
		{name: "malformed YAML", kubeconfig: "apiVersion: [secret-marker-that-must-not-leak"},
		{name: "missing current context", kubeconfig: "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := accessDetailsFromSecret(tc.kubeconfig, map[string][]byte{"console.url": []byte("https://console.example")}, "aws")
			if err == nil {
				t.Fatal("expected malformed kubeconfig to fail")
			}
			for _, secret := range []string{"active-token", "secret-marker-that-must-not-leak"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error revealed credential data: %v", err)
				}
			}
		})
	}
	_, err := accessDetailsFromSecret(`apiVersion: v1
kind: Config
clusters:
- name: active
  cluster:
    server: https://api.example:6443
contexts:
- name: active
  context:
    cluster: active
    user: active
current-context: active
users:
- name: active
  user:
    token: active-token
`, nil, "aws")
	if err == nil {
		t.Fatal("missing console URL should fail")
	}
	if strings.Contains(err.Error(), "active-token") {
		t.Fatalf("error revealed credential data: %v", err)
	}
}

func TestAccessDetailsForMetalWithoutProxyGivesKubeconfigInstruction(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: active
  cluster:
    server: https://api.example:6443
contexts:
- name: active
  context:
    cluster: active
    user: active
current-context: active
users:
- name: active
  user:
    token: active-token
`
	details, err := accessDetailsFromSecret(kubeconfig, map[string][]byte{"console.url": []byte("https://console.example")}, "metal")
	if err != nil {
		t.Fatalf("accessDetailsFromSecret returned error: %v", err)
	}
	if !strings.Contains(details.AccessInstructions, "proxy-url") || !strings.Contains(details.AccessInstructions, "http_proxy") {
		t.Fatalf("metal fallback instructions missing: %q", details.AccessInstructions)
	}
}

func TestFinishedJobRetriesVerification(t *testing.T) {
	for _, terminate := range []bool{false, true} {
		name := "ready"
		if terminate {
			name = "destroyed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			pj := lifecycleProwJob("cluster-retry", "U123", JobTypeLaunch, prowapiv1.PendingState, now)
			m, client := newLifecycleManager(pj)
			result := cachedLifecycleJob(pj.Name, "U123", now)
			current := cloneJob(result)
			clearJobAccess(current)
			current.RequestedChannel = ""
			m.jobs[pj.Name] = current
			var reads atomic.Int32
			client.PrependReactor("get", "prowjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				if reads.Add(1) == 1 {
					return true, nil, errors.New("temporary Prow outage")
				}
				verified := pj.DeepCopy()
				if terminate {
					verified.Annotations[annotationTerminationRequested] = terminationMarkerValue
					verified.Status.State = prowapiv1.AbortedState
				}
				return true, verified, nil
			})
			notifications := make(chan Job, 2)
			m.SetNotifier(func(job Job) { notifications <- job })
			m.finishedJob(*result)
			if reads.Load() != 2 {
				t.Fatalf("verification reads = %d, want 2", reads.Load())
			}
			stored := m.cachedJobSnapshot(pj.Name)
			if terminate {
				if stored.Credentials != "" || !stored.TerminationRequested {
					t.Fatal("retry published access after termination")
				}
				select {
				case <-notifications:
					t.Fatal("retry notified after termination")
				case <-time.After(25 * time.Millisecond):
				}
				return
			}
			if stored.Credentials != result.Credentials || stored.ConsolePassword != result.ConsolePassword {
				t.Fatal("retry lost the completed monitor result")
			}
			select {
			case notification := <-notifications:
				if notification.Credentials != result.Credentials || notification.RequestedChannel != "D123" {
					t.Fatal("retry lost the notification intent or credentials")
				}
			case <-time.After(time.Second):
				t.Fatal("retry did not notify")
			}
		})
	}
}

func phantomLaunchJob(name, user string, requestedAt time.Time) *Job {
	job := cachedLifecycleJob(name, user, requestedAt)
	clearJobAccess(job)
	job.RequestSource = requestSourceSlack
	job.BuildCluster = "build-cluster"
	return job
}

func TestDestroyClusterReleasesReservationWithoutProwJob(t *testing.T) {
	now := time.Now().UTC()
	m, _ := newLifecycleManager()
	m.launchReady = true
	m.maxClusters = 10
	m.maxAge = 3 * time.Hour
	job := phantomLaunchJob("phantom", "U123", now.Add(-time.Minute))
	m.jobs[job.Name] = job
	m.requests["U123"] = &JobRequest{User: "U123", Name: job.Name, Type: JobTypeInstall, RequestSource: requestSourceSlack, RequestedAt: job.RequestedAt}

	result, err := m.DestroyCluster(context.Background(), "U123", job.Name)
	if err != nil {
		t.Fatalf("DestroyCluster returned error: %v", err)
	}
	if result.JobID != job.Name || result.Status != clusterStatusTerminated {
		t.Fatalf("result = %#v, want terminated %q", result, job.Name)
	}
	if _, ok := m.requests["U123"]; ok {
		t.Fatalf("reservation was not released: %#v", m.requests["U123"])
	}
	stored := m.jobs[job.Name]
	if !stored.TerminationRequested || !stored.Complete || stored.Failure != "deletion requested" {
		t.Fatalf("cached job not marked for deletion: %#v", stored)
	}

	next := &JobRequest{User: "U123", Name: "next", Type: JobTypeInstall, RequestSource: requestSourceSlack, RequestedAt: now}
	reserved, message, err := m.reserveClusterSubmission(next, &Job{Name: "next", Mode: JobTypeLaunch, RequestedBy: "U123", State: prowapiv1.PendingState})
	if err != nil || message != "" || !reserved {
		t.Fatalf("new launch was not admitted: reserved=%v message=%q err=%v", reserved, message, err)
	}
}

func TestDestroyClusterWithoutProwJobOrLocalRecordIsNotFound(t *testing.T) {
	m, _ := newLifecycleManager()
	_, err := m.DestroyCluster(context.Background(), "U123", "missing")
	if toolErr := toolErrorFrom(t, err); toolErr.Code != ToolErrorCodeNotFound {
		t.Fatalf("error = %#v, want NOT_FOUND", toolErr)
	}

	// Another user's local record must not be released either.
	m.jobs["other"] = phantomLaunchJob("other", "U456", time.Now())
	m.requests["U456"] = &JobRequest{User: "U456", Name: "other", RequestSource: requestSourceSlack}
	_, err = m.DestroyCluster(context.Background(), "U123", "other")
	if toolErr := toolErrorFrom(t, err); toolErr.Code != ToolErrorCodeNotFound {
		t.Fatalf("error = %#v, want NOT_FOUND", toolErr)
	}
	if m.requests["U456"] == nil || m.jobs["other"].TerminationRequested {
		t.Fatal("destroy by another user changed the reservation")
	}
}

func TestDestroyClusterWithoutProwJobWaitsForSubmission(t *testing.T) {
	m, _ := newLifecycleManager()
	job := phantomLaunchJob("submitting", "U123", time.Now())
	m.jobs[job.Name] = job
	m.requests["U123"] = &JobRequest{User: "U123", Name: job.Name, RequestSource: requestSourceSlack, SubmissionInProgress: true}

	_, err := m.DestroyCluster(context.Background(), "U123", job.Name)
	if toolErr := toolErrorFrom(t, err); toolErr.Code != ToolErrorCodeLaunchInitialization || !toolErr.Retryable {
		t.Fatalf("error = %#v, want retryable LAUNCH_INITIALIZATION", toolErr)
	}
	if m.requests["U123"] == nil || m.jobs[job.Name].TerminationRequested {
		t.Fatal("in-progress submission was released")
	}
}

func TestTerminateJobForUserReleasesReservationWithoutProwJob(t *testing.T) {
	m, _ := newLifecycleManager()
	job := phantomLaunchJob("phantom", "U123", time.Now().Add(-time.Minute))
	m.jobs[job.Name] = job
	m.requests["U123"] = &JobRequest{User: "U123", Name: job.Name, Type: JobTypeInstall, RequestSource: requestSourceSlack}

	message, err := m.TerminateJobForUser("U123")
	if err != nil {
		t.Fatalf("TerminateJobForUser returned error: %v", err)
	}
	if !strings.Contains(message, "you may now launch another") {
		t.Fatalf("message = %q", message)
	}
	if _, ok := m.requests["U123"]; ok {
		t.Fatal("reservation was not released")
	}
}

func TestDestroyClusterClearsAccessAndShortensExpiry(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-ready", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-time.Minute))
	m, _ := newLifecycleManager(pj)
	m.jobs[pj.Name] = cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))

	if _, err := m.DestroyCluster(context.Background(), "U123", pj.Name); err != nil {
		t.Fatalf("DestroyCluster returned error: %v", err)
	}
	stored := m.jobs[pj.Name]
	if stored.Credentials != "" || stored.CredentialsSnippet != "" || stored.ConsoleURL != "" || stored.APIURL != "" ||
		stored.ConsoleUsername != "" || stored.ConsolePassword != "" || stored.AccessInstructions != "" || stored.RequestedChannel != "" {
		t.Fatalf("access fields were not cleared: %#v", stored)
	}
	if remaining := time.Until(stored.ExpiresAt); remaining < 14*time.Minute || remaining > 15*time.Minute {
		t.Fatalf("expiry is %s away, want about 15m", remaining)
	}
}

func TestApplyAccessDetailsKeepsSlackCredentialsWhenStructuredParseFails(t *testing.T) {
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://api.example:6443\n"
	job := &Job{Name: "cluster", Platform: "aws"}
	err := applyAccessDetails(job, kubeconfig, map[string][]byte{
		"console.url":        []byte("https://console.example\n"),
		"kubeadmin-password": []byte("secret\n"),
	})
	if err != nil {
		t.Fatalf("applyAccessDetails returned error: %v", err)
	}
	if job.Credentials != kubeconfig {
		t.Fatal("kubeconfig was dropped")
	}
	if !strings.HasPrefix(job.CredentialsSnippet, "https://console.example") || !strings.Contains(job.CredentialsSnippet, "`secret`") {
		t.Fatalf("snippet = %q", job.CredentialsSnippet)
	}
	if job.ConsoleURL != "" || job.APIURL != "" || job.ConsoleUsername != "" || job.ConsolePassword != "" || job.AccessInstructions != "" {
		t.Fatalf("structured fields set from an unparsable kubeconfig: %#v", job)
	}

	missing := &Job{Name: "cluster"}
	if err := applyAccessDetails(missing, kubeconfig, map[string][]byte{}); err == nil {
		t.Fatal("missing console URL did not return an error")
	}
	if missing.Credentials != kubeconfig {
		t.Fatal("kubeconfig was dropped when the console URL was missing")
	}
}

func TestGetClusterCredentialsWithoutStructuredAccess(t *testing.T) {
	now := time.Now().UTC()
	pj := lifecycleProwJob("cluster-ready", "U123", JobTypeLaunch, prowapiv1.PendingState, now.Add(-time.Minute))
	m, _ := newLifecycleManager(pj)
	job := cachedLifecycleJob(pj.Name, "U123", now.Add(-time.Minute))
	job.ConsoleURL, job.APIURL, job.ConsoleUsername, job.ConsolePassword, job.AccessInstructions = "", "", "", "", ""
	m.jobs[pj.Name] = job

	_, err := m.GetClusterCredentials(context.Background(), "U123", pj.Name)
	toolErr := toolErrorFrom(t, err)
	if toolErr.Code != ToolErrorCodeCredentialsUnavailable || toolErr.Retryable || !strings.Contains(toolErr.Message, "access details are unavailable") {
		t.Fatalf("error = %#v, want non-retryable access-unavailable error", toolErr)
	}
}
