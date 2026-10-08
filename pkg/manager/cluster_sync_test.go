package manager

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
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
