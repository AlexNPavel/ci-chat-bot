package manager

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	prowfake "sigs.k8s.io/prow/pkg/client/clientset/versioned/fake"
	prowjoblister "sigs.k8s.io/prow/pkg/client/listers/prowjobs/v1"
)

func syncTestProwJob(name, user string, createdAt time.Time) *prowapiv1.ProwJob {
	inputs, _ := json.Marshal([]JobInput{{Version: "4.18.2"}})
	return &prowapiv1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ci",
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels:            map[string]string{utils.LaunchLabel: "true"},
			Annotations: map[string]string{
				"ci-chat-bot.openshift.io/mode":               JobTypeLaunch,
				"ci-chat-bot.openshift.io/user":               user,
				"ci-chat-bot.openshift.io/requesterUserID":    "redhat-user",
				"ci-chat-bot.openshift.io/jobInputs":          string(inputs),
				"ci-chat-bot.openshift.io/jobParams":          "compact",
				"ci-chat-bot.openshift.io/platform":           "aws",
				"ci-chat-bot.openshift.io/channel":            "D123",
				"ci-chat-bot.openshift.io/expires":            "10800",
				"ci-chat-bot.openshift.io/buildCluster":       "build-cluster",
				"ci-chat-bot.openshift.io/IsOperator":         "true",
				"ci-chat-bot.openshift.io/HasIndex":           "true",
				"ci-chat-bot.openshift.io/OperatorBundleName": "example-operator",
				annotationRequestKeyHash:                      "request-hash",
				annotationInputFingerprint:                    "input-fingerprint",
				annotationRequestSource:                       requestSourceMCP,
			},
		},
		Spec:   prowapiv1.ProwJobSpec{Job: "periodic-launch", Cluster: "build-cluster"},
		Status: prowapiv1.ProwJobStatus{State: prowapiv1.PendingState},
	}
}

func newSyncTestManager(t *testing.T, pjs ...*prowapiv1.ProwJob) *jobManager {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pj := range pjs {
		if err := indexer.Add(pj); err != nil {
			t.Fatalf("could not add ProwJob to sync lister: %v", err)
		}
	}
	return &jobManager{
		jobs:          make(map[string]*Job),
		requests:      make(map[string]*JobRequest),
		maxAge:        3 * time.Hour,
		prowNamespace: "ci",
		prowLister:    prowjoblister.NewProwJobLister(indexer),
	}
}

func TestSyncPreservesLaunchMetadataAndReadinessFailure(t *testing.T) {
	now := time.Now().UTC()
	pj := syncTestProwJob("cluster-recovered", "U123", now.Add(-time.Minute))
	m := newSyncTestManager(t, pj)
	m.jobs[pj.Name] = &Job{
		Name:          pj.Name,
		Mode:          JobTypeLaunch,
		RequestedBy:   "U123",
		State:         prowapiv1.PendingState,
		Complete:      true,
		Failure:       "operator readiness check failed",
		StartDuration: 4 * time.Minute,
		ExpiresAt:     now.Add(time.Hour),
	}

	if err := m.sync(); err != nil {
		t.Fatalf("sync() error = %v", err)
	}

	got := m.jobs[pj.Name]
	if got == nil {
		t.Fatal("sync() did not reconstruct the ProwJob")
	}
	if got.RequestKeyHash != "request-hash" || got.InputFingerprint != "input-fingerprint" || got.RequestSource != requestSourceMCP {
		t.Fatalf("request metadata = (%q, %q, %q), want durable values", got.RequestKeyHash, got.InputFingerprint, got.RequestSource)
	}
	if !got.Operator.Is || !got.Operator.HasIndex || got.Operator.BundleName != "example-operator" {
		t.Fatalf("operator metadata = %#v, want recovered launch readiness metadata", got.Operator)
	}
	if !got.Complete || got.Failure != "operator readiness check failed" || got.StartDuration != 4*time.Minute {
		t.Fatalf("cached readiness state was lost: complete=%v failure=%q startDuration=%s", got.Complete, got.Failure, got.StartDuration)
	}
	if request := m.requests["U123"]; request == nil || request.RequestKeyHash != "request-hash" || request.RequestSource != requestSourceMCP {
		t.Fatalf("recovered request metadata = %#v", request)
	}
	if !launchRequestFailure(got) || clusterLaunchActive(got) {
		t.Fatalf("completed launch without credentials should be inactive: %#v", got)
	}
}

func TestSyncExpiresJobsAndOnlyClearsTheirMatchingReservation(t *testing.T) {
	now := time.Now().UTC()
	m := newSyncTestManager(t)
	m.jobs["expired-job"] = &Job{
		Name:        "expired-job",
		Mode:        JobTypeLaunch,
		RequestedBy: "U-expired",
		ExpiresAt:   now.Add(-time.Minute),
	}
	m.jobs["expired-other"] = &Job{
		Name:        "expired-other",
		Mode:        JobTypeLaunch,
		RequestedBy: "U-mismatch",
		ExpiresAt:   now.Add(-time.Minute),
	}
	m.requests["U-expired"] = &JobRequest{User: "U-expired", Name: "expired-job", RequestedAt: now.Add(-time.Hour), Type: JobTypeInstall}
	m.requests["U-mismatch"] = &JobRequest{User: "U-mismatch", Name: "newer-job", RequestedAt: now, Type: JobTypeInstall}
	m.requests["U-stale"] = &JobRequest{User: "U-stale", Name: "missing-job", RequestedAt: now.Add(-4 * time.Hour), Type: JobTypeInstall}

	if err := m.sync(); err != nil {
		t.Fatalf("sync() error = %v", err)
	}
	if _, exists := m.jobs["expired-job"]; exists {
		t.Fatal("sync() retained an expired job")
	}
	if _, exists := m.requests["U-expired"]; exists {
		t.Fatal("sync() retained the reservation matching an expired job")
	}
	if _, exists := m.jobs["expired-other"]; exists {
		t.Fatal("sync() retained the second expired job")
	}
	if request := m.requests["U-mismatch"]; request == nil || request.Name != "newer-job" {
		t.Fatalf("sync() cleared a nonmatching reservation: %#v", request)
	}
	if _, exists := m.requests["U-stale"]; exists {
		t.Fatal("sync() retained a stale request with no durable job")
	}
}

func TestCompleteLaunchWithoutStateDoesNotRemainActive(t *testing.T) {
	job := &Job{Mode: JobTypeLaunch, Complete: true, ExpiresAt: time.Now().Add(time.Hour)}
	if !launchRequestFailure(job) || clusterLaunchActive(job) {
		t.Fatalf("completed job without a Prow state must release admission: %#v", job)
	}
}

func TestSyncCountsRecoveredActiveLaunchBeforeAdmission(t *testing.T) {
	now := time.Now().UTC()
	pj := syncTestProwJob("recovered-active", "U123", now.Add(-time.Minute))
	m := newSyncTestManager(t, pj)
	m.maxClusters = 1
	m.launchReady = true
	m.jobs[pj.Name] = &Job{
		Name:        pj.Name,
		Mode:        JobTypeLaunch,
		RequestedBy: "U123",
		State:       prowapiv1.PendingState,
		Complete:    true,
		Credentials: "apiVersion: v1\nkind: Config\n",
		ExpiresAt:   now.Add(time.Hour),
	}

	if err := m.sync(); err != nil {
		t.Fatalf("sync() error = %v", err)
	}
	if count, _ := activeClusterCountLocked(m); count != 1 {
		t.Fatalf("active recovered launch count = %d, want 1", count)
	}

	req := &JobRequest{
		User:             "U456",
		Type:             JobTypeInstall,
		RequestedAt:      now,
		Name:             "second-launch",
		RequestKeyHash:   "another-request",
		InputFingerprint: "another-input",
		RequestSource:    requestSourceMCP,
	}
	provisional := &Job{
		Name:        req.Name,
		Mode:        JobTypeLaunch,
		RequestedBy: req.User,
		RequestedAt: req.RequestedAt,
		ExpiresAt:   now.Add(time.Hour),
	}
	if _, _, err := m.reserveClusterSubmission(req, provisional); err == nil {
		t.Fatal("admission succeeded while recovered active launch consumed the only slot")
	} else {
		var toolErr *ToolError
		if !errors.As(err, &toolErr) || toolErr.Code != ToolErrorCodeCapacityExhausted {
			t.Fatalf("admission error = %T %v, want CAPACITY_EXHAUSTED", err, err)
		}
	}
}

func TestSubmitClusterRejectsUntilInitialRecoveryCompletes(t *testing.T) {
	fixture := newClusterLaunchFixture(1)
	fixture.manager.launchReady = false
	request, dmCalls := successfulDMRequest("U-not-ready", "request-not-ready", launchTestImage)

	_, err := fixture.manager.SubmitCluster(context.Background(), request)
	if err == nil {
		t.Fatal("SubmitCluster accepted a launch before initial recovery completed")
	}
	toolErr := launchToolError(t, err)
	if toolErr.Code != ToolErrorCodeLaunchInitialization || !toolErr.Retryable {
		t.Fatalf("readiness error = %#v, want retryable LAUNCH_INITIALIZATION", toolErr)
	}
	if dmCalls.Load() != 0 || fixture.createCount.Load() != 0 {
		t.Fatalf("pre-recovery submission opened %d DMs and created %d ProwJobs", dmCalls.Load(), fixture.createCount.Load())
	}
}

func TestFinishedJobPreservesNotificationAcrossSync(t *testing.T) {
	now := time.Now().UTC()
	pj := syncTestProwJob("cluster-notification", "U123", now)
	// waitForJob clears this annotation before returning its completed result.
	pj.Annotations["ci-chat-bot.openshift.io/channel"] = ""
	m := newSyncTestManager(t, pj)
	m.prowClient = prowfake.NewSimpleClientset(pj).ProwV1()
	m.muJob.running = map[string]struct{}{pj.Name: {}}
	result := cachedLifecycleJob(pj.Name, "U123", now)
	current := cloneJob(result)
	clearJobAccess(current)
	m.jobs[pj.Name] = current
	notifications := make(chan Job, 3)
	m.SetNotifier(func(job Job) { notifications <- job })
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	m.finishedJob(*result)
	select {
	case notification := <-notifications:
		if notification.RequestedChannel != "D123" || notification.Credentials != result.Credentials {
			t.Fatal("sync lost pending credentials notification")
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not notify after sync cleared the cached channel")
	}
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	m.finishedJob(*result)
	select {
	case <-notifications:
		t.Fatal("sync lost delivery tracking and caused a duplicate notification")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestSyncCompletedJobRetention(t *testing.T) {
	for _, mode := range []string{JobTypeLaunch, JobTypeTest, JobTypeBuild} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			old := syncTestProwJob("old-job", "U123", now.Add(-time.Hour))
			old.Annotations["ci-chat-bot.openshift.io/mode"] = mode
			old.Status.State = prowapiv1.SuccessState
			old.Status.CompletionTime = &metav1.Time{Time: now.Add(-16 * time.Minute)}
			recent := old.DeepCopy()
			recent.Name = "recent-job"
			recent.Status.CompletionTime = &metav1.Time{Time: now.Add(-14 * time.Minute)}
			m := newSyncTestManager(t, old, recent)
			for _, pj := range []*prowapiv1.ProwJob{old, recent} {
				job, err := clusterJobFromProwJob(pj)
				if err != nil {
					t.Fatal(err)
				}
				if pj.Name == old.Name {
					// The cache may not have observed completion during an outage.
					job.CompletedAt = nil
					job.Complete = false
					job.State = prowapiv1.PendingState
				}
				m.jobs[pj.Name] = job
			}
			if err := m.sync(); err != nil {
				t.Fatal(err)
			}
			if m.cachedJobSnapshot(old.Name) != nil {
				t.Fatal("completed job retained quota beyond 15 minutes")
			}
			stored := m.cachedJobSnapshot(recent.Name)
			if stored == nil || !stored.ExpiresAt.Equal(recent.CreationTimestamp.Add(3*time.Hour)) {
				t.Fatal("recent completed job was evicted or its reported lifetime changed")
			}
			// Expired completed jobs must also stay out on subsequent recovery.
			if err := m.sync(); err != nil {
				t.Fatal(err)
			}
			if m.cachedJobSnapshot(old.Name) != nil {
				t.Fatal("sync reintroduced an evicted completed job")
			}
		})
	}
}

func TestAcceptedLaunchNormalizesMonitorState(t *testing.T) {
	for _, state := range []prowapiv1.ProwJobState{"", prowapiv1.TriggeredState, prowapiv1.SchedulingState, prowapiv1.PendingState, prowapiv1.SuccessState} {
		t.Run(string(state), func(t *testing.T) {
			pj := syncTestProwJob("accepted-job", "U123", time.Now().UTC())
			pj.Status.State = state
			m := newSyncTestManager(t)
			// Reserve the worker so the test can inspect its initial state.
			m.muJob.running = map[string]struct{}{pj.Name: {}}
			planned, err := clusterJobFromProwJob(pj)
			if err != nil {
				t.Fatal(err)
			}
			_, worker, err := m.recordAcceptedClusterJob(nil, planned, pj)
			if err != nil {
				t.Fatal(err)
			}
			terminal := isTerminalProwState(state)
			if worker.IsComplete() != terminal || m.jobIsComplete(worker) != terminal {
				t.Fatalf("accepted state %q incorrectly completed or stopped the monitor", state)
			}
			if !terminal && worker.State != prowapiv1.PendingState {
				t.Fatalf("monitor state = %q, want pending", worker.State)
			}
		})
	}
}

// gatedProwJobLister hands each List call to the test, which decides whether
// it fails.
type gatedProwJobLister struct {
	prowjoblister.ProwJobLister
	calls chan chan error
}

func (l *gatedProwJobLister) ProwJobs(namespace string) prowjoblister.ProwJobNamespaceLister {
	return gatedProwJobNamespaceLister{ProwJobNamespaceLister: l.ProwJobLister.ProwJobs(namespace), parent: l}
}

type gatedProwJobNamespaceLister struct {
	prowjoblister.ProwJobNamespaceLister
	parent *gatedProwJobLister
}

func (l gatedProwJobNamespaceLister) List(selector labels.Selector) ([]*prowapiv1.ProwJob, error) {
	result := make(chan error)
	l.parent.calls <- result
	if err := <-result; err != nil {
		return nil, err
	}
	return l.ProwJobNamespaceLister.List(selector)
}

// notFoundConfigMaps reports every ConfigMap as missing.
type notFoundConfigMaps struct {
	typedcorev1.ConfigMapInterface
}

func (notFoundConfigMaps) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	return nil, apierrors.NewNotFound(corev1.Resource("configmaps"), name)
}

func (m *jobManager) isLaunchReady() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.launchReady
}

func TestRecoverLaunchesKeepsLaunchesBlockedUntilRecoverySucceeds(t *testing.T) {
	m := newSyncTestManager(t)
	lister := &gatedProwJobLister{ProwJobLister: m.prowLister, calls: make(chan chan error)}
	m.prowLister = lister

	recovered := make(chan struct{})
	go m.recoverLaunches(wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 10}, func() { close(recovered) })

	for attempt := 1; attempt <= 3; attempt++ {
		result := <-lister.calls
		if err := m.launchRecoveryError(""); err == nil {
			t.Fatalf("attempt %d: launches were admitted before recovery succeeded", attempt)
		}
		if attempt < 3 {
			result <- errors.New("informer cache unavailable")
		} else {
			result <- nil
		}
	}
	select {
	case <-recovered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not complete after sync succeeded")
	}
	if err := m.launchRecoveryError(""); err != nil {
		t.Fatalf("launch blocked after recovery: %v", err)
	}
}

func TestStartReturnsWithoutHypershiftConfigMap(t *testing.T) {
	m := newSyncTestManager(t)
	m.hiveConfigMapClient = notFoundConfigMaps{}
	if err := m.updateHypershiftSupportedVersions(); !apierrors.IsNotFound(err) {
		t.Fatalf("updateHypershiftSupportedVersions() error = %v, want NotFound", err)
	}
	var loopsStarted atomic.Bool
	m.startBackgroundLoops = func() { loopsStarted.Store(true) }

	if err := m.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !loopsStarted.Load() {
		t.Fatal("Start() did not start the background loops")
	}
	deadline := time.After(5 * time.Second)
	for !m.isLaunchReady() {
		select {
		case <-deadline:
			t.Fatal("launch recovery did not complete")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSyncMonitorsPendingMCEJob(t *testing.T) {
	now := time.Now().UTC()
	pending := syncTestProwJob("mce-pending", "U123", now.Add(-time.Minute))
	pending.Annotations["ci-chat-bot.openshift.io/managedClusterName"] = "managed-a"
	succeeded := syncTestProwJob("mce-succeeded", "U456", now.Add(-time.Minute))
	succeeded.Annotations["ci-chat-bot.openshift.io/managedClusterName"] = "managed-b"
	succeeded.Status.State = prowapiv1.SuccessState
	m := newSyncTestManager(t, pending, succeeded)

	actions, err := m.reconcileProwJobs()
	if err != nil {
		t.Fatalf("reconcileProwJobs() error = %v", err)
	}
	if len(actions.monitor) != 1 || actions.monitor[0].Name != pending.Name || actions.monitor[0].ManagedClusterName != "managed-a" {
		t.Fatalf("monitored jobs = %#v, want only the pending MCE job", actions.monitor)
	}
	if len(actions.finished) != 0 {
		t.Fatalf("finished jobs = %#v, want none for a successful MCE job", actions.finished)
	}
	mceNames := map[string]bool{}
	for _, entry := range actions.mce {
		mceNames[entry.job.Name] = true
	}
	if len(mceNames) != 2 || !mceNames[pending.Name] || !mceNames[succeeded.Name] {
		t.Fatalf("MCE follow-up jobs = %v, want both MCE jobs", mceNames)
	}
}
