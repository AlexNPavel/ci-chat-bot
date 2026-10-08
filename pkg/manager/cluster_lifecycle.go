package manager

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

const (
	clusterStatusProvisioning = "provisioning"
	clusterStatusReady        = "ready"
	clusterStatusFailed       = "failed"
	clusterStatusTerminating  = "terminating"
	clusterStatusTerminated   = "terminated"
	clusterStatusExpired      = "expired"
)

const terminationMarkerValue = "true"

type clusterAccessDetails struct {
	ConsoleURL         string
	APIURL             string
	ConsoleUsername    string
	ConsolePassword    string
	AccessInstructions string
}

// accessDetailsFromSecret turns the existing launch secret and kubeconfig into
// structured access fields. The kubeconfig parser honors current-context, so
// APIURL and a metal proxy are taken from the selected cluster configuration.
func accessDetailsFromSecret(kubeconfig string, secretData map[string][]byte, platform string) (clusterAccessDetails, error) {
	consoleURL := strings.TrimSpace(string(secretData["console.url"]))
	if consoleURL == "" {
		return clusterAccessDetails{}, fmt.Errorf("console URL is missing")
	}

	config, err := loadKubeconfigContents(kubeconfig)
	if err != nil {
		return clusterAccessDetails{}, fmt.Errorf("unable to parse kubeconfig for the API endpoint")
	}
	if config.Host == "" {
		return clusterAccessDetails{}, fmt.Errorf("kubeconfig does not specify an API endpoint")
	}

	rawConfig, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return clusterAccessDetails{}, fmt.Errorf("unable to parse kubeconfig context")
	}
	contextConfig, ok := rawConfig.Contexts[rawConfig.CurrentContext]
	if !ok || contextConfig == nil {
		return clusterAccessDetails{}, fmt.Errorf("kubeconfig current context is missing")
	}
	clusterConfig, ok := rawConfig.Clusters[contextConfig.Cluster]
	if !ok || clusterConfig == nil {
		return clusterAccessDetails{}, fmt.Errorf("kubeconfig current cluster is missing")
	}

	details := clusterAccessDetails{ConsoleURL: consoleURL, APIURL: config.Host}
	if password := strings.TrimSpace(string(secretData["kubeadmin-password"])); password != "" {
		details.ConsoleUsername = "kubeadmin"
		details.ConsolePassword = password
	}

	if platform == "metal" {
		proxyURL, parseErr := url.Parse(clusterConfig.ProxyURL)
		proxyHost := ""
		if parseErr == nil && proxyURL != nil {
			proxyHost = proxyURL.Host
		}
		if proxyHost != "" {
			details.AccessInstructions = fmt.Sprintf(
				"Metal cluster access requires its proxy. Set both `http_proxy` and `https_proxy` to `%s` before using the console or API; the returned kubeconfig also contains this proxy configuration.",
				proxyHost,
			)
		} else {
			details.AccessInstructions = "Metal cluster access requires a proxy. Use the `proxy-url` value in the returned kubeconfig for both `http_proxy` and `https_proxy` before using the console or API."
		}
		if details.ConsoleUsername != "" {
			details.AccessInstructions += " Sign in to the console with the returned console username and password."
		}
		return details, nil
	}

	details.AccessInstructions = "Use the returned kubeconfig for command-line access to the cluster."
	if details.ConsoleUsername != "" {
		details.AccessInstructions += " Sign in to the OpenShift console with the returned console username and password."
	}
	return details, nil
}

type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock serializes operations on one job and returns a release function. The
// reference count keeps the lock map bounded after jobs stop being touched.
func (m *keyedMutex) lock(key string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*keyedLock)
	}
	lock, ok := m.locks[key]
	if !ok {
		lock = &keyedLock{}
		m.locks[key] = lock
	}
	lock.refs++
	m.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}

func lifecycleError(code, message string, retryable bool, jobID string) error {
	return ToolError{Code: code, Message: message, Retryable: retryable, JobID: jobID}
}

func validateLifecycleRequest(user, jobID string) error {
	if strings.TrimSpace(user) == "" {
		return lifecycleError(ToolErrorCodeInvalidArguments, "slack_user_id is required", false, jobID)
	}
	if strings.TrimSpace(jobID) == "" {
		return lifecycleError(ToolErrorCodeInvalidArguments, "job_id is required", false, jobID)
	}
	return nil
}

func notFound(jobID string) error {
	return lifecycleError(ToolErrorCodeNotFound, "cluster not found", false, jobID)
}

func backendUnavailable(jobID string, _ error) error {
	return lifecycleError(ToolErrorCodeBackendUnavailable, "cluster state is temporarily unavailable", true, jobID)
}

// cloneJob makes a detached snapshot before returning state outside the
// manager lock. JobInput contains Prow refs with nested slices, and the job
// parameters map is mutable, so both need deep copies.
func cloneJob(job *Job) *Job {
	if job == nil {
		return nil
	}
	copy := *job
	copy.JobParams = maps.Clone(job.JobParams)
	copy.Inputs = make([]JobInput, len(job.Inputs))
	for i, input := range job.Inputs {
		copy.Inputs[i] = input
		copy.Inputs[i].Refs = make([]prowapiv1.Refs, len(input.Refs))
		for j := range input.Refs {
			copy.Inputs[i].Refs[j] = *input.Refs[j].DeepCopy()
		}
	}
	if job.CompletedAt != nil {
		completedAt := *job.CompletedAt
		copy.CompletedAt = &completedAt
	}
	return &copy
}

func (m *jobManager) cachedJobSnapshot(jobID string) *Job {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return cloneJob(m.jobs[jobID])
}

func (m *jobManager) getOwnedProwJob(ctx context.Context, user, jobID string) (*prowapiv1.ProwJob, error) {
	if err := validateLifecycleRequest(user, jobID); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.prowClient == nil {
		return nil, backendUnavailable(jobID, fmt.Errorf("prow client is unavailable"))
	}
	pj, err := m.prowClient.ProwJobs(m.prowNamespace).Get(ctx, jobID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, notFound(jobID)
	}
	if err != nil {
		return nil, backendUnavailable(jobID, err)
	}
	if pj.Annotations["ci-chat-bot.openshift.io/user"] != user ||
		pj.Annotations["ci-chat-bot.openshift.io/mode"] != JobTypeLaunch ||
		pj.Labels[utils.LaunchLabel] != "true" {
		return nil, notFound(jobID)
	}
	return pj, nil
}

func isTerminalProwState(state prowapiv1.ProwJobState) bool {
	switch state {
	case prowapiv1.AbortedState, prowapiv1.ErrorState, prowapiv1.FailureState, prowapiv1.SuccessState:
		return true
	default:
		return false
	}
}

func lifecycleStatus(job *Job, pj *prowapiv1.ProwJob, now time.Time) string {
	if job != nil && job.TerminationRequested {
		switch pj.Status.State {
		case prowapiv1.AbortedState:
			if pj.Status.CompletionTime != nil {
				return clusterStatusTerminated
			}
			return clusterStatusTerminating
		case prowapiv1.SuccessState:
			return clusterStatusTerminated
		case prowapiv1.FailureState, prowapiv1.ErrorState:
			return clusterStatusFailed
		default:
			return clusterStatusTerminating
		}
	}
	switch pj.Status.State {
	case prowapiv1.AbortedState:
		return clusterStatusTerminated
	case prowapiv1.ErrorState, prowapiv1.FailureState:
		return clusterStatusFailed
	case prowapiv1.SuccessState:
		return clusterStatusTerminated
	}
	if job != nil && !job.ExpiresAt.IsZero() && !now.Before(job.ExpiresAt) {
		return clusterStatusExpired
	}
	if job != nil && job.Failure != "" {
		return clusterStatusFailed
	}
	if job != nil && job.Credentials != "" {
		return clusterStatusReady
	}
	return clusterStatusProvisioning
}

func mergeCachedJobAccess(job, cached *Job) {
	if job == nil || cached == nil || job.Name != cached.Name || job.Mode != cached.Mode || job.RequestedBy != cached.RequestedBy {
		return
	}
	job.Credentials = cached.Credentials
	job.CredentialsSnippet = cached.CredentialsSnippet
	job.ConsoleURL = cached.ConsoleURL
	job.APIURL = cached.APIURL
	job.ConsoleUsername = cached.ConsoleUsername
	job.ConsolePassword = cached.ConsolePassword
	job.AccessInstructions = cached.AccessInstructions
	job.Failure = cached.Failure
	job.StartDuration = cached.StartDuration
	job.TerminationRequested = job.TerminationRequested || cached.TerminationRequested
	if job.CompletedAt == nil && cached.CompletedAt != nil {
		completedAt := *cached.CompletedAt
		job.CompletedAt = &completedAt
	}
	job.Complete = job.Complete || cached.Complete
}

func (m *jobManager) snapshotForProwJob(pj *prowapiv1.ProwJob) (*Job, error) {
	job, err := clusterJobFromProwJob(pj)
	if err != nil {
		return nil, lifecycleError(ToolErrorCodeInternal, "cluster metadata could not be read", false, pj.Name)
	}
	mergeCachedJobAccess(job, m.cachedJobSnapshot(pj.Name))
	job.State = pj.Status.State
	job.URL = pj.Status.URL
	job.TerminationRequested = job.TerminationRequested || pj.Annotations[annotationTerminationRequested] == terminationMarkerValue
	if pj.Status.CompletionTime != nil {
		completedAt := pj.Status.CompletionTime.Time
		job.CompletedAt = &completedAt
		job.Complete = true
	}
	if job.TerminationRequested || isTerminalProwState(pj.Status.State) {
		clearJobAccess(job)
	}
	return job, nil
}

func reconcileMonitorJobWithProw(job *Job, pj *prowapiv1.ProwJob) {
	if job == nil || pj == nil {
		return
	}
	job.State = pj.Status.State
	job.URL = pj.Status.URL
	job.TerminationRequested = job.TerminationRequested || pj.Annotations[annotationTerminationRequested] == terminationMarkerValue
	if pj.Status.CompletionTime != nil {
		completedAt := pj.Status.CompletionTime.Time
		job.CompletedAt = &completedAt
		job.Complete = true
	}
	if job.TerminationRequested || isTerminalProwState(pj.Status.State) {
		job.Credentials = ""
		job.CredentialsSnippet = ""
		job.ConsoleURL = ""
		job.APIURL = ""
		job.ConsoleUsername = ""
		job.ConsolePassword = ""
		job.AccessInstructions = ""
	}
	if job.TerminationRequested {
		job.RequestedChannel = ""
		job.Failure = "deletion requested"
	}
}

func clearJobAccess(job *Job) {
	if job == nil {
		return
	}
	job.Credentials = ""
	job.CredentialsSnippet = ""
	job.ConsoleURL = ""
	job.APIURL = ""
	job.ConsoleUsername = ""
	job.ConsolePassword = ""
	job.AccessInstructions = ""
}

func (m *jobManager) refreshMonitorJobState(job *Job) bool {
	if job == nil || m.prowClient == nil {
		return job == nil || job.Mode != JobTypeLaunch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pj, err := m.prowClient.ProwJobs(m.prowNamespace).Get(ctx, job.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && job.Mode == JobTypeLaunch {
			job.State = prowapiv1.ErrorState
			job.Complete = true
			job.Failure = "cluster launch job is no longer available"
			job.ExpiresAt = time.Now().Add(15 * time.Minute)
			clearJobAccess(job)
			return true
		}
		return job.Mode != JobTypeLaunch
	}
	if pj.Annotations["ci-chat-bot.openshift.io/user"] != job.RequestedBy ||
		pj.Annotations["ci-chat-bot.openshift.io/mode"] != job.Mode ||
		(job.Mode == JobTypeLaunch && pj.Labels[utils.LaunchLabel] != "true") {
		if job.Mode == JobTypeLaunch {
			job.State = prowapiv1.ErrorState
			job.Complete = true
			job.Failure = "cluster launch ownership could not be verified"
			job.RequestedChannel = ""
			clearJobAccess(job)
			return true
		}
		return false
	}
	reconcileMonitorJobWithProw(job, pj)
	return true
}

func clusterInputs(job *Job) [][]string {
	if job == nil || len(job.Inputs) == 0 {
		return nil
	}
	inputs := make([][]string, 0, len(job.Inputs))
	for _, input := range job.Inputs {
		current := make([]string, 0, 1+len(input.Refs))
		switch {
		case input.Version != "":
			current = append(current, input.Version)
		case input.Image != "":
			current = append(current, input.Image)
		case input.RunImage != "":
			current = append(current, input.RunImage)
		}
		for _, ref := range input.Refs {
			for _, pull := range ref.Pulls {
				current = append(current, fmt.Sprintf("%s/%s#%d", ref.Org, ref.Repo, pull.Number))
			}
		}
		if len(current) > 0 {
			inputs = append(inputs, current)
		}
	}
	return inputs
}

func clusterSummary(job *Job, pj *prowapiv1.ProwJob, now time.Time) ClusterSummary {
	status := lifecycleStatus(job, pj, now)
	summary := ClusterSummary{
		JobID:        job.Name,
		SlackUserID:  job.RequestedBy,
		UserName:     job.RequesterUserID,
		Status:       status,
		Inputs:       clusterInputs(job),
		Platform:     job.Platform,
		Architecture: job.Architecture,
		Parameters:   maps.Clone(job.JobParams),
		RequestedAt:  job.RequestedAt,
		ExpiresAt:    job.ExpiresAt,
		CompletedAt:  job.CompletedAt,
		LogsURL:      pj.Status.URL,
	}
	if job.CompletedAt != nil {
		completedAt := *job.CompletedAt
		summary.CompletedAt = &completedAt
	}
	if status == clusterStatusReady {
		summary.ConsoleURL = job.ConsoleURL
		summary.APIURL = job.APIURL
	}
	if status == clusterStatusFailed {
		summary.Failure = "The cluster launch did not complete successfully."
	}
	return summary
}

// GetClusterStatus returns an owner-verified status snapshot. Prow is read on
// every request so stale in-memory credentials can never hide a terminal job.
func (m *jobManager) GetClusterStatus(ctx context.Context, user, jobID string) (ClusterSummary, error) {
	if err := validateLifecycleRequest(user, jobID); err != nil {
		return ClusterSummary{}, err
	}
	pj, err := m.getOwnedProwJob(ctx, user, jobID)
	if err != nil {
		return ClusterSummary{}, err
	}
	job, err := m.snapshotForProwJob(pj)
	if err != nil {
		return ClusterSummary{}, err
	}
	return clusterSummary(job, pj, time.Now()), nil
}

// GetClusterCredentials only returns credentials reconstructed by the existing
// monitor and held in the in-memory cache. A restart therefore reports them as
// unavailable until that monitor has passed its readiness checks again.
func (m *jobManager) GetClusterCredentials(ctx context.Context, user, jobID string) (ClusterCredentials, error) {
	if err := validateLifecycleRequest(user, jobID); err != nil {
		return ClusterCredentials{}, err
	}
	unlock := m.lifecycleLocks.lock(jobID)
	defer unlock()

	pj, err := m.getOwnedProwJob(ctx, user, jobID)
	if err != nil {
		return ClusterCredentials{}, err
	}
	job, err := m.snapshotForProwJob(pj)
	if err != nil {
		return ClusterCredentials{}, err
	}
	status := lifecycleStatus(job, pj, time.Now())
	if status != clusterStatusReady || job.Credentials == "" {
		retryable := status == clusterStatusProvisioning || status == clusterStatusReady
		return ClusterCredentials{}, lifecycleError(ToolErrorCodeCredentialsUnavailable, "cluster credentials are not available", retryable, jobID)
	}
	return ClusterCredentials{
		JobID:              jobID,
		Kubeconfig:         job.Credentials,
		ConsoleURL:         job.ConsoleURL,
		APIURL:             job.APIURL,
		ConsoleUsername:    job.ConsoleUsername,
		ConsolePassword:    job.ConsolePassword,
		AccessInstructions: job.AccessInstructions,
	}, nil
}

// ListClusters returns active ordinary Prow launches owned by user. Prow is
// the source of truth for membership and terminal state; cached credentials are
// copied only after ownership has been checked.
func (m *jobManager) ListClusters(ctx context.Context, user string) ([]ClusterSummary, error) {
	if strings.TrimSpace(user) == "" {
		return nil, lifecycleError(ToolErrorCodeInvalidArguments, "slack_user_id is required", false, "")
	}
	if m.prowClient == nil {
		return nil, backendUnavailable("", fmt.Errorf("prow client is unavailable"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	selector := labels.Set{utils.LaunchLabel: "true"}.AsSelector().String()
	pjList, err := m.prowClient.ProwJobs(m.prowNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, backendUnavailable("", err)
	}
	clusters := make([]ClusterSummary, 0)
	now := time.Now()
	for i := range pjList.Items {
		pj := &pjList.Items[i]
		if pj.Annotations["ci-chat-bot.openshift.io/user"] != user ||
			pj.Annotations["ci-chat-bot.openshift.io/mode"] != JobTypeLaunch ||
			pj.Labels[utils.LaunchLabel] != "true" {
			continue
		}
		job, err := m.snapshotForProwJob(pj)
		if err != nil {
			return nil, err
		}
		status := lifecycleStatus(job, pj, now)
		if status != clusterStatusProvisioning && status != clusterStatusReady && status != clusterStatusTerminating {
			continue
		}
		clusters = append(clusters, clusterSummary(job, pj, now))
	}
	sort.Slice(clusters, func(i, j int) bool {
		if !clusters[i].RequestedAt.Equal(clusters[j].RequestedAt) {
			return clusters[i].RequestedAt.After(clusters[j].RequestedAt)
		}
		return clusters[i].JobID < clusters[j].JobID
	})
	return clusters, nil
}

func (m *jobManager) requestProwJobTermination(ctx context.Context, user, jobID string) (*prowapiv1.ProwJob, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.prowClient == nil {
		return nil, backendUnavailable(jobID, fmt.Errorf("prow client is unavailable"))
	}
	var updated *prowapiv1.ProwJob
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pj, err := m.prowClient.ProwJobs(m.prowNamespace).Get(ctx, jobID, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return notFound(jobID)
		}
		if err != nil {
			return err
		}
		if pj.Annotations["ci-chat-bot.openshift.io/user"] != user ||
			pj.Annotations["ci-chat-bot.openshift.io/mode"] != JobTypeLaunch {
			return notFound(jobID)
		}
		if pj.Annotations[annotationTerminationRequested] == terminationMarkerValue &&
			(pj.Status.State == prowapiv1.AbortedState || isTerminalProwState(pj.Status.State)) {
			updated = pj
			return nil
		}

		candidate := pj.DeepCopy()
		if candidate.Annotations == nil {
			candidate.Annotations = make(map[string]string)
		}
		candidate.Annotations[annotationTerminationRequested] = terminationMarkerValue
		if !isTerminalProwState(candidate.Status.State) {
			candidate.Status.State = prowapiv1.AbortedState
		}
		updated, err = m.prowClient.ProwJobs(m.prowNamespace).Update(ctx, candidate, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, notFound(jobID)
		}
		if _, ok := err.(ToolError); ok {
			return nil, err
		}
		return nil, backendUnavailable(jobID, err)
	}
	return updated, nil
}

// DestroyCluster requests cancellation for exactly jobID. It retains the
// ProwJob, records a durable marker, and clears only a matching user
// reservation so a later cluster cannot be affected by an old ID.
func (m *jobManager) DestroyCluster(ctx context.Context, user, jobID string) (TerminationResult, error) {
	if err := validateLifecycleRequest(user, jobID); err != nil {
		return TerminationResult{}, err
	}
	unlock := m.lifecycleLocks.lock(jobID)
	defer unlock()

	if _, err := m.getOwnedProwJob(ctx, user, jobID); err != nil {
		return TerminationResult{}, err
	}
	pj, err := m.requestProwJobTermination(ctx, user, jobID)
	if err != nil {
		return TerminationResult{}, err
	}

	m.lock.Lock()
	if current := m.jobs[jobID]; current != nil && current.RequestedBy == user && current.Mode == JobTypeLaunch {
		updated := cloneJob(current)
		updated.State = pj.Status.State
		updated.URL = pj.Status.URL
		updated.TerminationRequested = true
		updated.Complete = true
		updated.RequestedChannel = ""
		updated.Credentials = ""
		updated.CredentialsSnippet = ""
		updated.ConsolePassword = ""
		updated.ConsoleUsername = ""
		updated.AccessInstructions = ""
		updated.Failure = "deletion requested"
		if pj.Status.CompletionTime != nil {
			completedAt := pj.Status.CompletionTime.Time
			updated.CompletedAt = &completedAt
		}
		m.jobs[jobID] = updated
	}
	if request := m.requests[user]; request != nil && request.Name == jobID {
		delete(m.requests, user)
	}
	m.lock.Unlock()

	job, err := m.snapshotForProwJob(pj)
	if err != nil {
		// The termination has already been persisted. Return its state even if
		// older launch metadata is malformed.
		job = &Job{Name: jobID, RequestedBy: user, Mode: JobTypeLaunch, TerminationRequested: true}
	}
	status := lifecycleStatus(job, pj, time.Now())
	return TerminationResult{JobID: jobID, Status: status}, nil
}
