package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/blang/semver"
	"github.com/openshift/ci-chat-bot/pkg/prow"
	"github.com/openshift/ci-chat-bot/pkg/utils"
	githubv4 "github.com/shurcooL/githubv4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

const requestSourceMCP = "mcp"
const requestSourceSlack = "slack"

type prowJobCreateError struct {
	err error
}

func (e prowJobCreateError) Error() string { return e.err.Error() }
func (e prowJobCreateError) Unwrap() error { return e.err }

type prowJobAcceptedError struct {
	err error
}

func (e prowJobAcceptedError) Error() string { return e.err.Error() }
func (e prowJobAcceptedError) Unwrap() error { return e.err }

var invalidProwNameCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

type canonicalClusterLaunchInput struct {
	Inputs       []string          `json:"inputs"`
	Platform     string            `json:"platform"`
	Architecture string            `json:"architecture"`
	Parameters   map[string]string `json:"parameters"`
}

// requestKeyHash returns a stable hash scoped to the trusted service principal,
// Slack user, and caller-supplied request ID. Raw request IDs are never persisted.
func requestKeyHash(servicePrincipal, slackUserID, requestID string) string {
	sum := sha256.Sum256([]byte(servicePrincipal + "\x00" + slackUserID + "\x00" + requestID))
	return hex.EncodeToString(sum[:])
}

// ScopedRequestKeyHash computes the durable request key used by the manager
// and by transport audit records.
func ScopedRequestKeyHash(servicePrincipal, slackUserID, requestID string) string {
	return requestKeyHash(servicePrincipal, slackUserID, requestID)
}

// launchInputFingerprint hashes the requested arguments before input resolution
// or platform defaulting can make two different requests look alike.
func launchInputFingerprint(inputs []string, platform, architecture string, parameters map[string]string) string {
	if parameters == nil {
		parameters = map[string]string{}
	}
	canonical := canonicalClusterLaunchInput{
		Inputs:       slices.Clone(inputs),
		Platform:     platform,
		Architecture: architecture,
		Parameters:   maps.Clone(parameters),
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// deterministicProwJobName maps a scoped request hash to a short DNS-label-safe
// name while retaining enough bits to make accidental collisions negligible.
func deterministicProwJobName(prefix, keyHash string) string {
	prefix = strings.ToLower(strings.Trim(prefix, "-"))
	prefix = invalidProwNameCharacters.ReplaceAllString(prefix, "-")
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "chat-bot"
	}
	if len(keyHash) < 40 {
		keyHash = fmt.Sprintf("%040s", keyHash)
	}
	suffix := "mcp-" + keyHash[:40]
	maxPrefixLength := 63 - len(suffix) - 1
	if len(prefix) > maxPrefixLength {
		prefix = strings.TrimRight(prefix[:maxPrefixLength], "-")
	}
	if prefix == "" {
		prefix = "cb"
	}
	return prefix + "-" + suffix
}

// DefaultClusterLaunchPlatformArchitecture applies the same defaults to Slack
// and structured launches. The raw input group is inspected before release
// aliases such as nightly are resolved.
func DefaultClusterLaunchPlatformArchitecture(inputs [][]string, platform, architecture string, jobType JobType) (string, string, error) {
	if platform != "" && !slices.Contains(SupportedPlatforms, platform) {
		return "", "", fmt.Errorf("unknown platform: %s", platform)
	}
	if architecture != "" && !slices.Contains(SupportedArchitectures, architecture) {
		return "", "", fmt.Errorf("unknown architecture: %s", architecture)
	}

	if platform == "" {
		switch architecture {
		case "", "multi":
			platform = "aws"
			if jobType == JobTypeInstall || jobType == JobTypeLaunch {
				if defaultLaunchUsesHypershift(inputs) {
					platform = "hypershift-hosted"
				}
			}
		case "amd64", "arm64":
			platform = "aws"
		default:
			return "", "", fmt.Errorf("unknown architecture: %s", architecture)
		}
	}
	if architecture == "" {
		architecture = "amd64"
		if platform == "hypershift-hosted" {
			architecture = "multi"
		}
	}
	if platform == "hypershift-hosted" && architecture != "multi" {
		return "", "", fmt.Errorf("The hypershift-hosted platform requires a multiarch image. See: https://docs.ci.openshift.org/docs/architecture/ci-operator/#testing-with-a-cluster-from-hypershift") //nolint:staticcheck
	}
	return platform, architecture, nil
}

func defaultLaunchUsesHypershift(inputs [][]string) bool {
	HypershiftSupportedVersions.Mu.RLock()
	defer HypershiftSupportedVersions.Mu.RUnlock()

	for _, input := range inputs {
		for _, item := range input {
			for version := range HypershiftSupportedVersions.Versions {
				if strings.HasPrefix(item, version) {
					return true
				}
			}
		}
	}
	current := fmt.Sprintf("%d.%d", CurrentRelease.Major, CurrentRelease.Minor)
	if !HypershiftSupportedVersions.Versions.Has(current) {
		return false
	}
	for _, input := range inputs {
		for _, item := range input {
			if item == "nightly" || item == "ci" || item == "prerelease" {
				return true
			}
		}
	}
	return len(inputs) == 0
}

func normalizeClusterLaunchOptions(inputs [][]string, platform, architecture string, parameters map[string]string, jobType JobType) (string, string, map[string]string, error) {
	params := maps.Clone(parameters)
	if params == nil {
		params = make(map[string]string)
	}
	for parameter := range params {
		if !slices.Contains(SupportedParameters, parameter) {
			return "", "", nil, fmt.Errorf("unrecognized option %q", parameter)
		}
	}
	if jobType == JobTypeInstall || jobType == JobTypeLaunch {
		if _, exists := params["test"]; exists {
			return "", "", nil, fmt.Errorf("TestUpgrade arguments may not be passed from the launch command")
		}
	}
	var err error
	platform, architecture, err = DefaultClusterLaunchPlatformArchitecture(inputs, platform, architecture, jobType)
	if err != nil {
		return "", "", nil, err
	}
	return platform, architecture, params, nil
}

// NormalizeClusterLaunchOptions validates typed launch options and applies the
// same platform and architecture defaults used by Slack and structured callers.
func NormalizeClusterLaunchOptions(inputs [][]string, platform, architecture string, parameters map[string]string, jobType JobType) (string, string, map[string]string, error) {
	return normalizeClusterLaunchOptions(inputs, platform, architecture, parameters, jobType)
}

func cloneJobRequest(req *JobRequest) *JobRequest {
	if req == nil {
		return nil
	}
	cloned := *req
	cloned.Inputs = make([][]string, len(req.Inputs))
	for i := range req.Inputs {
		cloned.Inputs[i] = slices.Clone(req.Inputs[i])
	}
	cloned.JobParams = maps.Clone(req.JobParams)
	return &cloned
}

func clusterJobFromProwJob(pj *prowapiv1.ProwJob) (*Job, error) {
	if pj == nil {
		return nil, fmt.Errorf("ProwJob is nil")
	}
	annotations := pj.Annotations
	var inputs []JobInput
	if raw := annotations["ci-chat-bot.openshift.io/jobInputs"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
			return nil, fmt.Errorf("unable to deserialize launch inputs from ProwJob %q: %w", pj.Name, err)
		}
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("ProwJob %q has no launch input metadata", pj.Name)
	}
	params, err := utils.ParamsFromAnnotation(annotations["ci-chat-bot.openshift.io/jobParams"])
	if err != nil {
		return nil, fmt.Errorf("unable to deserialize launch parameters from ProwJob %q: %w", pj.Name, err)
	}
	architecture := annotations["release.openshift.io/architecture"]
	if architecture == "" {
		architecture = "amd64"
	}
	job := &Job{
		Name:                 pj.Name,
		State:                pj.Status.State,
		URL:                  pj.Status.URL,
		OriginalMessage:      annotations["ci-chat-bot.openshift.io/originalMessage"],
		Mode:                 annotations["ci-chat-bot.openshift.io/mode"],
		JobName:              pj.Spec.Job,
		Platform:             annotations["ci-chat-bot.openshift.io/platform"],
		JobParams:            params,
		Inputs:               inputs,
		RequestedBy:          annotations["ci-chat-bot.openshift.io/user"],
		RequestedChannel:     annotations["ci-chat-bot.openshift.io/channel"],
		RequestedAt:          pj.CreationTimestamp.Time,
		RequesterUserID:      annotations["ci-chat-bot.openshift.io/requesterUserID"],
		Architecture:         architecture,
		BuildCluster:         annotations["release.openshift.io/buildCluster"],
		ManagedClusterName:   annotations["ci-chat-bot.openshift.io/managedClusterName"],
		RequestKeyHash:       annotations[annotationRequestKeyHash],
		InputFingerprint:     annotations[annotationInputFingerprint],
		RequestSource:        annotations[annotationRequestSource],
		TerminationRequested: annotations[annotationTerminationRequested] == terminationMarkerValue,
		Operator: OperatorInfo{
			Is:         annotations["ci-chat-bot.openshift.io/IsOperator"] == "true",
			HasIndex:   annotations["ci-chat-bot.openshift.io/HasIndex"] == "true",
			BundleName: annotations["ci-chat-bot.openshift.io/OperatorBundleName"],
		},
	}
	if job.RequestedAt.IsZero() {
		job.RequestedAt = time.Now()
	}
	if job.BuildCluster == "" {
		job.BuildCluster = annotations["ci-chat-bot.openshift.io/buildCluster"]
	}
	if job.RequestSource == "" {
		job.RequestSource = "slack"
	}
	if pj.Status.CompletionTime != nil {
		completedAt := pj.Status.CompletionTime.Time
		job.CompletedAt = &completedAt
		job.Complete = true
	}
	if expiry := annotations["ci-chat-bot.openshift.io/expires"]; expiry != "" {
		if seconds, err := time.ParseDuration(expiry + "s"); err == nil && seconds > 0 {
			job.ExpiresAt = job.RequestedAt.Add(seconds)
		}
	}
	if job.ExpiresAt.IsZero() {
		job.ExpiresAt = job.RequestedAt.Add(3 * time.Hour)
		if job.Complete && pj.Status.CompletionTime != nil {
			job.ExpiresAt = pj.Status.CompletionTime.Add(15 * time.Minute)
		}
	}
	switch pj.Status.State {
	case prowapiv1.FailureState, prowapiv1.ErrorState, prowapiv1.AbortedState:
		job.Failure = "job failed, see logs"
	}
	return job, nil
}

func validateDurableClusterJob(pj *prowapiv1.ProwJob, slackUserID, requestKey, fingerprint string) (*Job, error) {
	if pj == nil {
		return nil, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true}
	}
	job, err := clusterJobFromProwJob(pj)
	if err != nil {
		return nil, &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID is already associated with a different launch request.", Retryable: false, JobID: pj.Name}
	}
	if pj.Labels[utils.LaunchLabel] != "true" || job.RequestedBy != slackUserID || job.RequestKeyHash != requestKey || job.InputFingerprint != fingerprint || job.RequestSource != requestSourceMCP {
		return nil, &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID is already associated with a different launch request.", Retryable: false, JobID: pj.Name}
	}
	if job.Mode != JobTypeLaunch {
		return nil, &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID does not belong to an ordinary cluster launch.", Retryable: false, JobID: pj.Name}
	}
	return job, nil
}

func isAmbiguousProwCreateError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "connection reset") || strings.Contains(message, "unexpected eof") || strings.Contains(message, "connection aborted") {
		return true
	}
	var statusErr *apierrors.StatusError
	return errors.As(err, &statusErr) && statusErr.ErrStatus.Code >= 500
}

func directProwJobReadContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 10*time.Second)
}

func directProwJobRead(m *jobManager, ctx context.Context, name string) (*prowapiv1.ProwJob, error) {
	if m.prowClient == nil {
		return nil, &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "Prow is not available to inspect the launch.", Retryable: true, JobID: name}
	}
	return m.prowClient.ProwJobs(m.prowNamespace).Get(ctx, name, metav1.GetOptions{})
}

func isRequestConflictError(err error) bool {
	var toolErr *ToolError
	return errors.As(err, &toolErr) && toolErr.Code == ToolErrorCodeRequestConflict
}

type contextConfigResolver interface {
	ResolveWithContext(ctx context.Context, org, repo, branch, variant string) ([]byte, bool, error)
}

func resolveConfigWithContext(ctx context.Context, resolver ConfigResolver, org, repo, branch, variant string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if contextual, ok := resolver.(contextConfigResolver); ok {
		return contextual.ResolveWithContext(ctx, org, repo, branch, variant)
	}
	if resolver == nil {
		return nil, false, fmt.Errorf("configuration resolver is unavailable")
	}
	result, found, err := resolver.Resolve(org, repo, branch, variant)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	return result, found, err
}

func validateLaunchInputs(req *JobRequest) error {
	if len(req.Inputs) == 0 {
		return fmt.Errorf("the `image_or_version_or_prs` parameter must be specified")
	}
	for _, input := range req.Inputs {
		if !containsValidVersion(input) {
			return fmt.Errorf("each use of the `image_or_version_or_prs` parameter must specify a valid OpenShift version.\n\n`%s` has no valid OpenShift version", strings.Join(input, ","))
		}
	}
	return nil
}

func normalizeJobRequestOptions(req *JobRequest) error {
	if req == nil {
		return fmt.Errorf("launch request is nil")
	}
	platform, architecture, params, err := normalizeClusterLaunchOptions(req.Inputs, req.Platform, req.Architecture, req.JobParams, req.Type)
	if err != nil {
		return err
	}
	req.Platform = platform
	req.Architecture = architecture
	req.JobParams = params
	return nil
}

func prepareProwLaunch(ctx context.Context, m *jobManager, req *JobRequest) (*Job, error) {
	if err := validateLaunchInputs(req); err != nil {
		return nil, err
	}
	if err := normalizeJobRequestOptions(req); err != nil {
		return nil, err
	}
	job, err := m.resolveToJobWithContext(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	jobType := JobTypeLaunch
	if req.Type == JobTypeWorkflowUpgrade {
		jobType = JobTypeUpgrade
	}
	selector := labels.Set{"job-env": req.Platform, "job-type": string(jobType), "job-architecture": req.Architecture}
	var prowJob *prowapiv1.ProwJob
	if len(job.Inputs[0].Version) > 0 {
		if version, parseErr := semver.ParseTolerant(job.Inputs[0].Version); parseErr == nil {
			withRelease := labels.Merge(selector, labels.Set{"job-release": fmt.Sprintf("%d.%d", version.Major, version.Minor)})
			prowJob, _ = prow.JobForLabels(m.prowConfigLoader, labels.SelectorFromSet(withRelease))
		}
	}
	if prowJob == nil {
		architectureLabel := req.Architecture
		if architectureLabel == "multi" {
			architectureLabel = "amd64"
		}
		modernSelector := labels.Set{"job-env": req.Platform, "job-type": JobTypeLaunch, "config-type": "modern", "job-architecture": architectureLabel}
		prowJob, _ = prow.JobForLabels(m.prowConfigLoader, labels.SelectorFromSet(modernSelector))
		if prowJob != nil {
			if sourceEnv, _, ok := firstEnvVar(prowJob.Spec.PodSpec, "UNRESOLVED_CONFIG"); ok {
				configHasVariant, _, variantErr := configContainsVariant(req.JobParams, req.Platform, sourceEnv.Value, job.Mode)
				if variantErr != nil {
					return nil, variantErr
				}
				if !configHasVariant {
					prowJob = nil
				}
			}
		}
	}
	if prowJob == nil {
		return nil, fmt.Errorf("configuration error, unable to find prow job matching %s with parameters=%v", selector, paramsToString(job.JobParams))
	}
	job.JobName = prowJob.Spec.Job
	job.BuildCluster, err = m.scheduleWithContext(ctx, prowJob)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		klog.Error(err.Error())
		job.BuildCluster = prowJob.Spec.Cluster
	}
	if req.Architecture == "amd64" {
		job.CloudProfileSet = platformProfileSets[req.Platform]
	}
	return job, nil
}

func isLaunchRequestType(jobType JobType) bool {
	return jobType == JobTypeInstall || jobType == JobTypeWorkflowLaunch
}

func isClusterLaunchMode(mode string) bool {
	return mode == JobTypeLaunch || mode == JobTypeWorkflowLaunch
}

func launchModeForRequest(jobType JobType) string {
	if jobType == JobTypeWorkflowLaunch {
		return JobTypeWorkflowLaunch
	}
	return JobTypeLaunch
}

func launchRequestFailure(job *Job) bool {
	if job == nil {
		return false
	}
	if job.TerminationRequested || (job.Complete && job.Credentials == "") || (!job.ExpiresAt.IsZero() && !time.Now().Before(job.ExpiresAt)) {
		return true
	}
	if isTerminalProwState(job.State) {
		return true
	}
	return job.Failure != ""
}

func clusterLaunchActive(job *Job) bool {
	return job != nil && isClusterLaunchMode(job.Mode) && (!job.Complete || job.Credentials != "") && job.Failure == "" && !job.TerminationRequested && !isTerminalProwState(job.State) && (job.ExpiresAt.IsZero() || time.Now().Before(job.ExpiresAt))
}

func (m *jobManager) launchRecoveryError(jobID string) error {
	m.lock.RLock()
	blocked := !m.launchReady
	m.lock.RUnlock()
	if blocked {
		return &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Cluster recovery is still in progress; retry shortly.", Retryable: true, JobID: jobID}
	}
	return nil
}

// reserveClusterSubmission applies user and global admission under one lock so
// Slack and structured launches share the same reservation map and capacity.
func (m *jobManager) reserveClusterSubmission(req *JobRequest, job *Job) (reserved bool, existingMessage string, err error) {
	if req == nil || job == nil {
		return false, "", fmt.Errorf("cluster launch request is incomplete")
	}
	m.lock.Lock()
	defer m.lock.Unlock()

	if !m.launchReady {
		return false, "", &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Cluster recovery is still in progress; retry shortly.", Retryable: true, JobID: job.Name}
	}

	activeCount, activeNames := activeClusterCountLocked(m)
	if current := m.requests[req.User]; current != nil && req.RequestKeyHash != "" && current.RequestKeyHash == req.RequestKeyHash {
		if current.InputFingerprint != req.InputFingerprint || current.RequestSource != req.RequestSource {
			return false, "", &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID is already associated with a different launch request.", Retryable: false, JobID: job.Name}
		}
		if current.SubmissionInProgress {
			return false, "", &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true, JobID: job.Name}
		}
		currentJob := m.jobs[job.Name]
		if currentJob == nil || launchRequestFailure(currentJob) || (!currentJob.ExpiresAt.IsZero() && !time.Now().Before(currentJob.ExpiresAt)) {
			// The old ProwJob may have aged out of retention. Drop the stale local
			// reservation and repeat normal admission before reusing its name.
			delete(m.jobs, job.Name)
			delete(m.requests, req.User)
			activeCount, _ = activeClusterCountLocked(m)
		} else {
			if _, counted := activeNames[job.Name]; counted && activeCount-1 >= m.maxClusters {
				return false, "", clusterCapacityAdmissionError(req, job.Name, m.jobs)
			}
			requestCopy := cloneJobRequest(req)
			requestCopy.SubmissionInProgress = true
			m.requests[req.User] = requestCopy
			return true, "", nil
		}
	}

	if current := m.requests[req.User]; current != nil {
		currentJob := m.jobs[current.Name]
		if launchRequestFailure(currentJob) || (currentJob == nil && !current.SubmissionInProgress && current.RequestedAt.Add(m.maxAge).Before(time.Now())) {
			if current.Name != "" {
				delete(m.jobs, current.Name)
			}
			delete(m.requests, req.User)
			activeCount, _ = activeClusterCountLocked(m)
		} else if isLaunchRequestType(current.Type) || isClusterLaunchMode(currentJobMode(currentJob)) {
			if currentJob != nil && len(currentJob.Credentials) > 0 && req.RequestSource == requestSourceSlack {
				return false, "your cluster is already running, see your credentials again with the 'auth' command", nil
			}
			return false, "", m.activeClusterAdmissionError(req, job.Name, current)
		}
	}

	for _, currentJob := range m.jobs {
		if currentJob == nil || currentJob.RequestedBy != req.User || !isClusterLaunchMode(currentJob.Mode) || launchRequestFailure(currentJob) {
			continue
		}
		if len(currentJob.Credentials) > 0 && req.RequestSource == requestSourceSlack {
			return false, "your cluster is already running, see your credentials again with the 'auth' command", nil
		}
		if currentJob.Name == job.Name && currentJob.RequestKeyHash == req.RequestKeyHash && currentJob.InputFingerprint == req.InputFingerprint {
			continue
		}
		return false, "", m.activeClusterAdmissionError(req, job.Name, nil)
	}

	if activeCount >= m.maxClusters {
		return false, "", clusterCapacityAdmissionError(req, job.Name, m.jobs)
	}

	requestCopy := cloneJobRequest(req)
	requestCopy.SubmissionInProgress = true
	m.requests[req.User] = requestCopy
	m.jobs[job.Name] = cloneJob(job)
	return true, "", nil
}

func activeClusterCountLocked(m *jobManager) (int, map[string]struct{}) {
	activeCount := 0
	activeNames := make(map[string]struct{})
	for name, currentJob := range m.jobs {
		if clusterLaunchActive(currentJob) {
			activeCount++
			activeNames[name] = struct{}{}
		}
	}
	now := time.Now()
	for _, current := range m.requests {
		if current == nil || !isLaunchRequestType(current.Type) || current.Name == "" {
			continue
		}
		if _, counted := activeNames[current.Name]; !counted && (current.SubmissionInProgress || (m.jobs[current.Name] == nil && current.RequestedAt.Add(m.maxAge).After(now))) {
			activeCount++
		}
	}
	return activeCount, activeNames
}

func currentJobMode(job *Job) string {
	if job == nil {
		return ""
	}
	return job.Mode
}

func (m *jobManager) activeClusterAdmissionError(req *JobRequest, jobID string, existing *JobRequest) error {
	if req.RequestSource == requestSourceMCP {
		return &ToolError{Code: ToolErrorCodeActiveClusterExists, Message: "You already have an active cluster launch.", Retryable: false, JobID: jobID}
	}
	if existing == nil || existing.Name == "" {
		return fmt.Errorf("you have already requested a cluster and it should be ready in ~ %d minutes", m.estimateCompletion(time.Time{})/time.Minute)
	}
	return fmt.Errorf("you have already requested a cluster and it should be ready in ~ %d minutes", m.estimateCompletion(existing.RequestedAt)/time.Minute)
}

func clusterCapacityAdmissionError(req *JobRequest, jobID string, jobs map[string]*Job) error {
	if req.RequestSource == requestSourceMCP {
		return &ToolError{Code: ToolErrorCodeCapacityExhausted, Message: "No cluster capacity is currently available.", Retryable: true, JobID: jobID}
	}
	var waitUntil time.Time
	for _, current := range jobs {
		if !clusterLaunchActive(current) {
			continue
		}
		if waitUntil.Before(current.ExpiresAt) {
			waitUntil = current.ExpiresAt
		}
	}
	minutes := time.Until(waitUntil).Minutes()
	if minutes < 1 {
		return fmt.Errorf("no clusters are currently available, unable to estimate when next cluster will be free")
	}
	return fmt.Errorf("no clusters are currently available, next slot available in %d minutes", int(math.Ceil(minutes)))
}

func (m *jobManager) updateClusterReservation(req *JobRequest, job *Job, inProgress bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	current := m.requests[req.User]
	if !matchesClusterReservation(current, req) {
		return
	}
	current.SubmissionInProgress = inProgress
	if job != nil {
		m.jobs[job.Name] = cloneJob(job)
	}
}

func (m *jobManager) rollbackClusterReservation(req *JobRequest) {
	if req == nil {
		return
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	if current := m.requests[req.User]; matchesClusterReservation(current, req) {
		delete(m.requests, req.User)
	}
	if current := m.jobs[req.Name]; current != nil && current.RequestedBy == req.User && current.RequestKeyHash == req.RequestKeyHash && current.InputFingerprint == req.InputFingerprint && current.RequestSource == req.RequestSource {
		delete(m.jobs, req.Name)
	}
}

func matchesClusterReservation(current, req *JobRequest) bool {
	if current == nil || req == nil || current.Name != req.Name || current.User != req.User || current.RequestSource != req.RequestSource {
		return false
	}
	return current.RequestKeyHash == req.RequestKeyHash && current.InputFingerprint == req.InputFingerprint
}

func (m *jobManager) lookupDurableClusterRequest(ctx context.Context, req *JobRequest) (LaunchResult, bool, error) {
	readCtx, cancel := directProwJobReadContext(ctx)
	defer cancel()
	pj, err := directProwJobRead(m, readCtx, req.Name)
	if apierrors.IsNotFound(err) {
		return LaunchResult{}, false, nil
	}
	if err != nil {
		return LaunchResult{}, false, &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "Prow could not verify the launch request.", Retryable: true, JobID: req.Name}
	}
	job, err := validateDurableClusterJob(pj, req.User, req.RequestKeyHash, req.InputFingerprint)
	if err != nil {
		return LaunchResult{}, false, err
	}
	summary, _, err := m.recordAcceptedClusterJob(req, job, pj)
	if err != nil {
		return LaunchResult{}, false, err
	}
	return LaunchResult{Cluster: summary, Replayed: true}, true, nil
}

func (m *jobManager) recordAcceptedClusterJob(req *JobRequest, planned *Job, pj *prowapiv1.ProwJob) (ClusterSummary, *Job, error) {
	accepted := cloneJob(planned)
	if pj != nil {
		fromProw, err := m.snapshotForProwJob(pj)
		if err != nil {
			return ClusterSummary{}, nil, err
		}
		if fromProw.RequestedAt.IsZero() {
			fromProw.RequestedAt = planned.RequestedAt
		}
		if fromProw.ExpiresAt.IsZero() || (pj.CreationTimestamp.IsZero() && fromProw.ExpiresAt.Before(time.Now())) {
			fromProw.ExpiresAt = planned.ExpiresAt
		}
		accepted = fromProw
	}
	if accepted == nil {
		return ClusterSummary{}, nil, fmt.Errorf("accepted cluster job is missing")
	}
	if accepted.RequestedAt.IsZero() {
		accepted.RequestedAt = time.Now()
	}
	if accepted.ExpiresAt.IsZero() {
		accepted.ExpiresAt = accepted.RequestedAt.Add(m.maxAge)
	}
	if pj == nil {
		accepted.State = prowapiv1.PendingState
	}

	m.lock.Lock()
	if current := m.jobs[accepted.Name]; current != nil && current.RequestedBy == accepted.RequestedBy && current.RequestKeyHash == accepted.RequestKeyHash && current.InputFingerprint == accepted.InputFingerprint {
		mergeCachedJobAccess(accepted, current)
		if current.StartDuration > accepted.StartDuration {
			accepted.StartDuration = current.StartDuration
		}
	}
	if req != nil {
		if current := m.requests[req.User]; current == nil || current.Name == accepted.Name || matchesClusterReservation(current, req) {
			m.requests[req.User] = jobRequestFromClusterJob(accepted)
		}
	}
	if accepted.TerminationRequested || isTerminalProwState(accepted.State) || (!accepted.ExpiresAt.IsZero() && !time.Now().Before(accepted.ExpiresAt)) {
		clearJobAccess(accepted)
		if current := m.requests[accepted.RequestedBy]; current != nil && current.Name == accepted.Name {
			delete(m.requests, accepted.RequestedBy)
		}
	}
	m.jobs[accepted.Name] = cloneJob(accepted)
	worker := cloneJob(accepted)
	shouldMonitor := !accepted.TerminationRequested && !isTerminalProwState(accepted.State) && (accepted.ExpiresAt.IsZero() || time.Now().Before(accepted.ExpiresAt))
	m.lock.Unlock()

	if shouldMonitor {
		go m.handleJobStartup(*worker, "start")
	}
	if pj == nil {
		pj = &prowapiv1.ProwJob{
			ObjectMeta: metav1.ObjectMeta{
				Name:              accepted.Name,
				CreationTimestamp: metav1.NewTime(accepted.RequestedAt),
				Annotations: map[string]string{
					"ci-chat-bot.openshift.io/user": accepted.RequestedBy,
					"ci-chat-bot.openshift.io/mode": accepted.Mode,
				},
			},
			Status: prowapiv1.ProwJobStatus{State: prowapiv1.PendingState, URL: accepted.URL},
		}
	}
	return clusterSummary(accepted, pj, time.Now()), worker, nil
}

func jobRequestFromClusterJob(job *Job) *JobRequest {
	if job == nil {
		return nil
	}
	var jobType JobType = JobTypeInstall
	if job.Mode == JobTypeWorkflowLaunch {
		jobType = JobTypeWorkflowLaunch
	}
	return &JobRequest{
		OriginalMessage:    job.OriginalMessage,
		User:               job.RequestedBy,
		UserName:           job.RequesterUserID,
		Inputs:             clusterInputs(job),
		Type:               jobType,
		Platform:           job.Platform,
		Channel:            job.RequestedChannel,
		RequestedAt:        job.RequestedAt,
		Name:               job.Name,
		JobName:            job.JobName,
		JobParams:          maps.Clone(job.JobParams),
		Architecture:       job.Architecture,
		ManagedClusterName: job.ManagedClusterName,
		RequestKeyHash:     job.RequestKeyHash,
		InputFingerprint:   job.InputFingerprint,
		RequestSource:      job.RequestSource,
	}
}

func validateClusterRequestID(servicePrincipal, userID, requestID string) error {
	if strings.TrimSpace(servicePrincipal) == "" || strings.TrimSpace(userID) == "" {
		return &ToolError{Code: ToolErrorCodeInvalidIdentity, Message: "A verified service principal and slack_user_id are required.", Retryable: false}
	}
	if strings.TrimSpace(requestID) == "" {
		return &ToolError{Code: ToolErrorCodeInvalidArguments, Message: "request_id is required.", Retryable: false}
	}
	if strings.ContainsRune(servicePrincipal, '\x00') || strings.ContainsRune(userID, '\x00') || strings.ContainsRune(requestID, '\x00') {
		return &ToolError{Code: ToolErrorCodeInvalidArguments, Message: "The launch request contains an invalid identifier.", Retryable: false}
	}
	return nil
}

func (m *jobManager) SubmitCluster(ctx context.Context, request ClusterLaunchRequest) (LaunchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateClusterRequestID(request.ServicePrincipal, request.SlackUserID, request.RequestID); err != nil {
		return LaunchResult{}, err
	}
	if err := m.launchRecoveryError(""); err != nil {
		return LaunchResult{}, err
	}
	if strings.TrimSpace(request.UserName) == "" {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeInvalidIdentity, Message: "The initiating Slack user has no usable Red Hat username.", Retryable: false}
	}
	if len(request.Inputs) == 0 {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeInvalidArguments, Message: "The inputs parameter must contain a valid OpenShift version or release image.", Retryable: false}
	}
	if err := ctx.Err(); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch request was canceled before submission.", Retryable: true}
	}

	keyHash := requestKeyHash(request.ServicePrincipal, request.SlackUserID, request.RequestID)
	fingerprint := launchInputFingerprint(request.Inputs, request.Platform, request.Architecture, request.Parameters)
	name := deterministicProwJobName(m.clusterPrefix, keyHash)
	jobRequest := &JobRequest{
		User:             request.SlackUserID,
		UserName:         request.UserName,
		Inputs:           [][]string{slices.Clone(request.Inputs)},
		Type:             JobTypeInstall,
		Platform:         request.Platform,
		JobParams:        maps.Clone(request.Parameters),
		Architecture:     request.Architecture,
		RequestedAt:      time.Now(),
		Name:             name,
		RequestKeyHash:   keyHash,
		InputFingerprint: fingerprint,
		RequestSource:    requestSourceMCP,
	}

	if result, found, err := m.lookupDurableClusterRequest(ctx, jobRequest); found || err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch request was canceled before submission.", Retryable: true, JobID: name}
	}
	if err := m.launchRecoveryError(name); err != nil {
		return LaunchResult{}, err
	}
	if cluster, _ := m.getROSAClusterForUser(request.SlackUserID); cluster != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeActiveClusterExists, Message: fmt.Sprintf("You already have a ROSA cluster request; %d minutes have elapsed.", int(time.Since(cluster.CreationTimestamp())/time.Minute)), Retryable: false, JobID: name}
	}

	if err := validateLaunchInputs(jobRequest); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeInvalidArguments, Message: err.Error(), Retryable: false}
	}
	if err := normalizeJobRequestOptions(jobRequest); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeInvalidArguments, Message: err.Error(), Retryable: false}
	}
	provisional := &Job{
		Name:             name,
		Mode:             JobTypeLaunch,
		State:            prowapiv1.PendingState,
		Platform:         jobRequest.Platform,
		JobParams:        maps.Clone(jobRequest.JobParams),
		RequestedBy:      jobRequest.User,
		RequesterUserID:  jobRequest.UserName,
		RequestedAt:      jobRequest.RequestedAt,
		ExpiresAt:        jobRequest.RequestedAt.Add(m.maxAge),
		Architecture:     jobRequest.Architecture,
		RequestKeyHash:   keyHash,
		InputFingerprint: fingerprint,
		RequestSource:    requestSourceMCP,
	}
	reserved, _, err := m.reserveClusterSubmission(jobRequest, provisional)
	if err != nil {
		return LaunchResult{}, err
	}
	if !reserved {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true, JobID: name}
	}

	reservationFinalized := false
	defer func() {
		if !reservationFinalized {
			m.rollbackClusterReservation(jobRequest)
		}
	}()

	if request.ResolveDM == nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "Slack could not open a notification channel for this user.", Retryable: true, JobID: name}
	}
	channel, err := request.ResolveDM(ctx)
	if err != nil || strings.TrimSpace(channel) == "" {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "Slack could not open a notification channel for this user.", Retryable: true, JobID: name}
	}
	jobRequest.Channel = channel
	if err := ctx.Err(); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch request was canceled before submission.", Retryable: true, JobID: name}
	}

	job, err := prepareProwLaunch(ctx, m, jobRequest)
	if err != nil {
		code := ToolErrorCodeInvalidArguments
		message := err.Error()
		retryable := false
		if ctx.Err() != nil {
			code = ToolErrorCodeLaunchInitialization
			message = "The launch is still being initialized. Retry with the same request ID."
			retryable = true
		} else if strings.Contains(strings.ToLower(err.Error()), "unable to lookup") || strings.Contains(strings.ToLower(err.Error()), "url resolve failed") || strings.Contains(strings.ToLower(err.Error()), "failed to get ocp release") {
			code = ToolErrorCodeBackendUnavailable
			message = "A launch dependency is temporarily unavailable. Retry with the same request ID."
			retryable = true
		}
		return LaunchResult{}, &ToolError{Code: code, Message: message, Retryable: retryable, JobID: name}
	}
	job.RequestKeyHash = keyHash
	job.InputFingerprint = fingerprint
	job.RequestSource = requestSourceMCP
	job.RequestedChannel = channel
	job.RequestedBy = request.SlackUserID
	job.RequesterUserID = request.UserName
	job.RequestedAt = jobRequest.RequestedAt
	job.ExpiresAt = jobRequest.RequestedAt.Add(m.maxAge)
	m.updateClusterReservation(jobRequest, job, true)

	submissionJob := cloneJob(job)
	_, createErr := m.newJobWithContext(ctx, submissionJob, false)
	if createErr != nil {
		if prowCreateErr, ok := errors.AsType[prowJobCreateError](createErr); ok {
			cause := prowCreateErr.Unwrap()
			if apierrors.IsAlreadyExists(cause) {
				result, found, reconcileErr := m.lookupDurableClusterRequest(ctx, jobRequest)
				if found {
					reservationFinalized = true
					return result, reconcileErr
				}
				if reconcileErr != nil {
					if isRequestConflictError(reconcileErr) {
						m.rollbackClusterReservation(jobRequest)
					} else {
						m.updateClusterReservation(jobRequest, submissionJob, false)
					}
					reservationFinalized = true
					return LaunchResult{}, reconcileErr
				}
				m.updateClusterReservation(jobRequest, submissionJob, false)
				reservationFinalized = true
				return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true, JobID: name}
			}
			if isAmbiguousProwCreateError(cause) {
				if ctx.Err() == nil {
					result, found, reconcileErr := m.lookupDurableClusterRequest(ctx, jobRequest)
					if found {
						reservationFinalized = true
						return result, reconcileErr
					}
					if isRequestConflictError(reconcileErr) {
						m.rollbackClusterReservation(jobRequest)
						reservationFinalized = true
						return LaunchResult{}, reconcileErr
					}
				}
				m.updateClusterReservation(jobRequest, submissionJob, false)
				reservationFinalized = true
				return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Prow may have accepted the launch. Retry with the same request ID to reconcile it.", Retryable: true, JobID: name}
			}
		}
		if _, ok := errors.AsType[prowJobAcceptedError](createErr); ok {
			result, found, reconcileErr := m.lookupDurableClusterRequest(ctx, jobRequest)
			if found {
				reservationFinalized = true
				return result, reconcileErr
			}
			if reconcileErr != nil {
				if isRequestConflictError(reconcileErr) {
					m.rollbackClusterReservation(jobRequest)
				} else {
					m.updateClusterReservation(jobRequest, submissionJob, false)
				}
				reservationFinalized = true
				return LaunchResult{}, reconcileErr
			}
			m.updateClusterReservation(jobRequest, submissionJob, false)
			reservationFinalized = true
			return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Prow may be processing the launch, but its status could not be verified. Retry with the same request ID.", Retryable: true, JobID: name}
		}
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Prow could not start the cluster launch.", Retryable: false, JobID: name}
	}

	// A successful Create response is durable acceptance even if an immediate
	// read is briefly unavailable. Keep the reservation and start monitoring.
	var acceptedProwJob *prowapiv1.ProwJob
	if ctx.Err() == nil {
		readCtx, cancel := directProwJobReadContext(ctx)
		acceptedProwJob, err = directProwJobRead(m, readCtx, name)
		cancel()
		if apierrors.IsNotFound(err) {
			acceptedProwJob = nil
		} else if err != nil {
			acceptedProwJob = nil
		}
	}
	if acceptedProwJob != nil {
		if _, err := validateDurableClusterJob(acceptedProwJob, jobRequest.User, keyHash, fingerprint); err != nil {
			reservationFinalized = true
			return LaunchResult{}, err
		}
	}
	summary, _, err := m.recordAcceptedClusterJob(jobRequest, submissionJob, acceptedProwJob)
	if err != nil {
		m.updateClusterReservation(jobRequest, submissionJob, false)
		reservationFinalized = true
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Prow accepted the launch, but its current status could not be read. Retry with the same request ID.", Retryable: true, JobID: name}
	}
	reservationFinalized = true
	return LaunchResult{Cluster: summary, Replayed: false}, nil
}

func (m *jobManager) launchClusterForSlack(req *JobRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("launch request is nil")
	}
	if err := m.launchRecoveryError(""); err != nil {
		return "", fmt.Errorf("cluster recovery is still in progress; retry shortly")
	}
	request := cloneJobRequest(req)
	if strings.TrimSpace(request.User) == "" {
		return "", fmt.Errorf("must specify the name of the user who requested this cluster")
	}
	if cluster, _ := m.getROSAClusterForUser(request.User); cluster != nil {
		return "", fmt.Errorf("you have already requested a cluster via the `rosa create` command; %d minutes have elapsed", int(time.Since(cluster.CreationTimestamp())/time.Minute))
	}
	if err := validateLaunchInputs(request); err != nil {
		return "", err
	}
	if err := normalizeJobRequestOptions(request); err != nil {
		return "", err
	}
	if request.RequestedAt.IsZero() {
		request.RequestedAt = time.Now()
	}
	if request.Name == "" {
		request.Name = fmt.Sprintf("%s%s", m.clusterPrefix, request.RequestedAt.UTC().Format("2006-01-02-150405.9999"))
	}
	request.RequestSource = requestSourceSlack
	mode := launchModeForRequest(request.Type)
	provisional := &Job{
		Name:               request.Name,
		Mode:               mode,
		State:              prowapiv1.PendingState,
		OriginalMessage:    request.OriginalMessage,
		Platform:           request.Platform,
		JobParams:          maps.Clone(request.JobParams),
		RequestedBy:        request.User,
		RequesterUserID:    request.UserName,
		RequestedChannel:   request.Channel,
		RequestedAt:        request.RequestedAt,
		ExpiresAt:          request.RequestedAt.Add(m.maxAge),
		Architecture:       request.Architecture,
		WorkflowName:       request.WorkflowName,
		ManagedClusterName: request.ManagedClusterName,
		RequestSource:      requestSourceSlack,
	}
	reserved, existingMessage, err := m.reserveClusterSubmission(request, provisional)
	if existingMessage != "" {
		return existingMessage, nil
	}
	if err != nil {
		return "", err
	}
	if !reserved {
		return "", fmt.Errorf("you have already requested a cluster and it should be ready in ~ 30 minutes")
	}
	reservationFinalized := false
	defer func() {
		if !reservationFinalized {
			m.rollbackClusterReservation(request)
		}
	}()

	job, err := prepareProwLaunch(context.Background(), m, request)
	if err != nil {
		return "", err
	}
	job.RequestSource = requestSourceSlack
	job.RequestedBy = request.User
	job.RequesterUserID = request.UserName
	job.RequestedChannel = request.Channel
	job.RequestedAt = request.RequestedAt
	job.ExpiresAt = request.RequestedAt.Add(m.maxAge)
	m.updateClusterReservation(request, job, true)

	submissionJob := cloneJob(job)
	prowURL, createErr := m.newJobWithContext(context.Background(), submissionJob, true)
	if createErr != nil {
		var acceptedErr prowJobAcceptedError
		accepted := errors.As(createErr, &acceptedErr)
		ambiguous := false
		if createErrWrapper, ok := errors.AsType[prowJobCreateError](createErr); ok {
			ambiguous = isAmbiguousProwCreateError(createErrWrapper.Unwrap())
		}
		if accepted || ambiguous {
			readCtx, cancel := directProwJobReadContext(context.Background())
			pj, readErr := directProwJobRead(m, readCtx, request.Name)
			cancel()
			if readErr == nil && pj.Annotations["ci-chat-bot.openshift.io/user"] == request.User && pj.Annotations["ci-chat-bot.openshift.io/mode"] == JobTypeLaunch {
				if _, _, recordErr := m.recordAcceptedClusterJob(request, submissionJob, pj); recordErr != nil {
					m.updateClusterReservation(request, submissionJob, false)
				} else {
					reservationFinalized = true
				}
			} else {
				m.updateClusterReservation(request, submissionJob, false)
				reservationFinalized = true
			}
			return "", createErr
		}
		return "", fmt.Errorf("the requested job cannot be started: %v", createErr)
	}

	var acceptedProwJob *prowapiv1.ProwJob
	readCtx, cancel := directProwJobReadContext(context.Background())
	acceptedProwJob, err = directProwJobRead(m, readCtx, request.Name)
	cancel()
	if err == nil && (acceptedProwJob.Annotations["ci-chat-bot.openshift.io/user"] != request.User || acceptedProwJob.Annotations["ci-chat-bot.openshift.io/mode"] != JobTypeLaunch) {
		return "", fmt.Errorf("the requested job could not be verified")
	}
	if err != nil {
		acceptedProwJob = nil
	}
	if prowURL != "" {
		submissionJob.URL = prowURL
	}
	if _, _, err := m.recordAcceptedClusterJob(request, submissionJob, acceptedProwJob); err != nil {
		m.updateClusterReservation(request, submissionJob, false)
		reservationFinalized = true
		return "", fmt.Errorf("the requested job is taking longer than expected to start: %v", err)
	}
	reservationFinalized = true
	return legacyClusterStartedMessage(submissionJob, prowURL), nil
}

func legacyClusterStartedMessage(job *Job, prowJobURL string) string {
	msg := ""
	if UseSpotInstances(job) {
		msg += "\nThis AWS cluster will use Spot instances for the worker nodes."
		msg += " This means that worker nodes may unexpectedly disappear, but will be replaced automatically."
		msg += " If your workload cannot tolerate disruptions, add the `no-spot` option to the options argument when launching your cluster."
		msg += " For more information on Spot instances, see this blog post: https://cloud.redhat.com/blog/a-guide-to-red-hat-openshift-and-aws-spot-instances.\n\n"
	}
	if job.Platform == "hypershift-hosted" {
		msg += "\nI noticed that you've created a `hypershift-hosted` cluster.  Next time, you might want to give ROSA's hypershift a try."
		msg += "  You can launch a cluster with: `rosa create <version> [duration]`.  See the `help` message for more information.\n"
		msg += "\nThis cluster is being launched with a <https://hypershift-docs.netlify.app/|hosted control plane (hypershift)>."
		msg += " This means that the control plane will run as pods (not virtual machines) on another cluster managed by DPTP; also by default there is 1 worker node."
		msg += " This has the advantage of much faster startup times and lower costs."
		msg += " However, if you are testing specific functionality relating to the control plane in the release version you provided or you require"
		msg += " multiple worker nodes, please end abort this launch with `done` and launch a cluster using another platform such as `aws` or `gcp`"
		msg += " (e.g. `launch 4.19 aws`).\n\n"
	}
	if prowJobURL != "" {
		msg += fmt.Sprintf("a <%s|cluster is being created>", prowJobURL)
	} else {
		msg += "a cluster is being created"
	}
	if job.Operator.Is {
		msg += " - On completion of the creation of the cluster, your optional operator will begin installation"
		if job.Operator.BundleName != "" {
			msg += fmt.Sprintf(" using the configuration for the `%s` bundle", job.Operator.BundleName)
		}
		msg += ". I'll send you the credentials once both the cluster and the operator are ready"
	} else {
		msg += " - I'll send you the credentials when the cluster is ready."
	}
	if jobHasRefs(job) {
		msg += "\n\nNote: your launch includes custom PR builds, which typically add 20-40 minutes to launch time. Total estimated time is up to ~90 minutes."
	}
	return msg
}

type pullRequestContextClient interface {
	GetPullRequestWithContext(ctx context.Context, org, repo string, number int) (*github.PullRequest, error)
}

func getPullRequestWithContext(ctx context.Context, client github.Client, org, repo string, number int) (*github.PullRequest, error) {
	if contextual, ok := client.(pullRequestContextClient); ok {
		return contextual.GetPullRequestWithContext(ctx, org, repo, number)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var query struct {
		Repository struct {
			PullRequest struct {
				Merged      githubv4.Boolean
				Mergeable   githubv4.MergeableState
				BaseRefName githubv4.String
				BaseRefOid  githubv4.GitObjectID
				HeadRefOid  githubv4.GitObjectID
				Author      struct {
					Login githubv4.String
				}
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	variables := map[string]any{
		"owner":  githubv4.String(org),
		"repo":   githubv4.String(repo),
		"number": githubv4.Int(number),
	}
	if err := client.QueryWithGitHubAppsSupport(ctx, &query, variables, org); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	merged := bool(query.Repository.PullRequest.Merged)
	var mergeable *bool
	switch query.Repository.PullRequest.Mergeable {
	case githubv4.MergeableStateMergeable:
		value := true
		mergeable = &value
	case githubv4.MergeableStateConflicting:
		value := false
		mergeable = &value
	}
	return &github.PullRequest{
		Merged:   merged,
		Mergable: mergeable,
		Base: github.PullRequestBranch{
			Ref: string(query.Repository.PullRequest.BaseRefName),
			SHA: string(query.Repository.PullRequest.BaseRefOid),
		},
		Head: github.PullRequestBranch{SHA: string(query.Repository.PullRequest.HeadRefOid)},
		User: github.User{Login: string(query.Repository.PullRequest.Author.Login)},
	}, nil
}
