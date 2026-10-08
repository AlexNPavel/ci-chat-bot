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

// launchInputFingerprint hashes the requested arguments before input
// resolution, so release aliases such as nightly keep their identity. Callers
// pass the resolved platform and architecture for the primary fingerprint and
// the caller-supplied values for the raw fingerprint.
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

// clusterInputFingerprintsMatch reports whether two requests describe the same
// launch. The resolved fingerprint is compared first, and the fingerprint of
// the caller-supplied arguments is accepted too so a retry still matches after
// a runtime default (such as the HyperShift supported-version list) changes.
func clusterInputFingerprintsMatch(resolvedA, rawA, resolvedB, rawB string) bool {
	if resolvedA == resolvedB {
		return true
	}
	return rawA != "" && rawA == rawB
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
	// Workflow launch platforms come from the workflow configuration and are
	// validated there, so they are not limited to the interactive launch
	// platforms.
	if platform != "" && (jobType == JobTypeInstall || jobType == JobTypeLaunch) && !slices.Contains(SupportedPlatforms, platform) {
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

	// Only a single release input is considered, and only its first item: a
	// later item such as the version in `launch <PR>,<version>` does not pick
	// the platform.
	if len(inputs) == 1 && len(inputs[0]) > 0 {
		for version := range HypershiftSupportedVersions.Versions {
			if strings.HasPrefix(inputs[0][0], version) {
				return true
			}
		}
	}
	current := fmt.Sprintf("%d.%d", CurrentRelease.Major, CurrentRelease.Minor)
	if !HypershiftSupportedVersions.Versions.Has(current) {
		return false
	}
	if len(inputs) > 0 && len(inputs[0]) > 0 {
		switch inputs[0][0] {
		case "nightly", "ci", "prerelease":
			return true
		}
	}
	return len(inputs) == 0
}

func normalizeClusterLaunchOptions(inputs [][]string, platform, architecture string, parameters map[string]string, jobType JobType) (string, string, map[string]string, error) {
	params := maps.Clone(parameters)
	if params == nil {
		params = make(map[string]string)
	}
	// Workflow launch parameters are arbitrary environment variables for the
	// workflow, so only ordinary launch parameter names are checked.
	if jobType == JobTypeInstall || jobType == JobTypeLaunch {
		for parameter := range params {
			if !slices.Contains(SupportedParameters, parameter) {
				return "", "", nil, fmt.Errorf("unrecognized option %q", parameter)
			}
		}
		if _, exists := params["test"]; exists {
			return "", "", nil, fmt.Errorf("TestUpgrade arguments may not be passed from the launch command")
		}
	}
	// Launch parameters are persisted as a comma-separated ProwJob annotation
	// and read back on recovery, so a comma in a value would not round-trip.
	if jobType == JobTypeInstall || jobType == JobTypeLaunch || jobType == JobTypeWorkflowLaunch {
		for parameter, value := range params {
			if strings.Contains(value, ",") {
				return "", "", nil, fmt.Errorf("the value of option %q may not contain commas", parameter)
			}
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

func clusterJobFromProwJob(pj *prowapiv1.ProwJob, maxAge time.Duration) (*Job, error) {
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
		RawInputFingerprint:  annotations[annotationRawInputFingerprint],
		RequestSource:        annotations[annotationRequestSource],
		TerminationRequested: annotations[annotationTerminationRequested] == terminationMarkerValue,
		ProwJobAccepted:      true,
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
		if maxAge <= 0 {
			maxAge = 3 * time.Hour
		}
		job.ExpiresAt = job.RequestedAt.Add(maxAge)
	}
	switch pj.Status.State {
	case prowapiv1.FailureState, prowapiv1.ErrorState:
		job.Failure = "job failed, see logs"
	case prowapiv1.AbortedState:
		if !job.TerminationRequested {
			job.Failure = "job aborted"
		}
	}
	return job, nil
}

// validateDurableClusterJob checks that pj is the launch recorded for req: the
// same request key, input fingerprint, and source, owned by req.User with the
// mode req asks for.
func validateDurableClusterJob(pj *prowapiv1.ProwJob, req *JobRequest, maxAge time.Duration) (*Job, error) {
	if pj == nil {
		return nil, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true}
	}
	job, err := clusterJobFromProwJob(pj, maxAge)
	if err != nil {
		return nil, &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID is already associated with a different launch request.", Retryable: false, JobID: pj.Name}
	}
	if job.RequestKeyHash != req.RequestKeyHash ||
		!clusterInputFingerprintsMatch(job.InputFingerprint, job.RawInputFingerprint, req.InputFingerprint, req.RawInputFingerprint) ||
		job.RequestSource != req.RequestSource {
		return nil, &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID is already associated with a different launch request.", Retryable: false, JobID: pj.Name}
	}
	if !prowJobMatches(pj, req.User, launchModeForRequest(req.Type)) {
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

// classifyLaunchPreparationError maps an error from preparing or submitting a
// launch to a ToolError. Cancellation and transient backend failures are
// retryable with the same request ID. Anything else is reported with
// fallbackCode and is not retryable, because the same request would fail again.
func classifyLaunchPreparationError(ctx context.Context, err error, fallbackCode string) *ToolError {
	// Every error here means no ProwJob exists (definitive create failure or
	// pre-create preparation failure), so no job ID is returned.
	if ctx != nil && ctx.Err() != nil {
		return &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true}
	}
	if isAmbiguousProwCreateError(err) {
		return &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "A launch dependency is temporarily unavailable. Retry with the same request ID.", Retryable: true}
	}
	// A missing pull request or base ref is a permanent input error, not a
	// transient lookup failure, so it must not be retried.
	if github.IsNotFound(err) {
		return &ToolError{Code: ToolErrorCodeInvalidArguments, Message: err.Error(), Retryable: false}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unable to lookup") || strings.Contains(message, "url resolve failed") || strings.Contains(message, "failed to get ocp release") {
		return &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "A launch dependency is temporarily unavailable. Retry with the same request ID.", Retryable: true}
	}
	if fallbackCode == ToolErrorCodeInvalidArguments {
		return &ToolError{Code: fallbackCode, Message: err.Error(), Retryable: false}
	}
	return &ToolError{Code: fallbackCode, Message: "Prow could not start the cluster launch.", Retryable: false}
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

// prepareProwLaunch resolves a validated and normalized launch request into a
// Job with its Prow job definition and build cluster selected.
func prepareProwLaunch(ctx context.Context, m *jobManager, req *JobRequest) (*Job, error) {
	job, err := m.resolveToJobWithContext(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := selectLaunchJob(ctx, m, req, job); err != nil {
		return nil, err
	}
	return job, nil
}

// selectLaunchJob picks the Prow job definition for a cluster launch, preferring
// one for the requested release, and schedules it onto a build cluster.
func selectLaunchJob(ctx context.Context, m *jobManager, req *JobRequest, job *Job) error {
	jobType := JobTypeLaunch
	if req.Type == JobTypeWorkflowUpgrade {
		jobType = JobTypeUpgrade
	}
	selector := labels.Set{"job-env": req.Platform, "job-type": string(jobType), "job-architecture": req.Architecture} // TODO: handle versioned variants better
	var prowJob *prowapiv1.ProwJob
	if len(job.Inputs[0].Version) > 0 {
		if version, parseErr := semver.ParseTolerant(job.Inputs[0].Version); parseErr == nil {
			withRelease := labels.Merge(selector, labels.Set{"job-release": fmt.Sprintf("%d.%d", version.Major, version.Minor)})
			prowJob, _ = prow.JobForLabels(m.prowConfigLoader, labels.SelectorFromSet(withRelease))
		}
	}
	if prowJob == nil {
		architectureLabel := req.Architecture
		// multiarch image launches use amd64 jobs
		if architectureLabel == "multi" {
			architectureLabel = "amd64"
		}
		// these jobs will only contain configs using non-deprecated features
		modernSelector := labels.Set{"job-env": req.Platform, "job-type": JobTypeLaunch, "config-type": "modern", "job-architecture": architectureLabel}
		prowJob, _ = prow.JobForLabels(m.prowConfigLoader, labels.SelectorFromSet(modernSelector))
		if prowJob != nil {
			// all multistage configs will be unresolved
			if sourceEnv, _, ok := firstEnvVar(prowJob.Spec.PodSpec, "UNRESOLVED_CONFIG"); ok {
				configHasVariant, _, variantErr := configContainsVariant(req.JobParams, req.Platform, sourceEnv.Value, job.Mode)
				if variantErr != nil {
					return variantErr
				}
				// if the config does not contain the wanted variant, reset prowjob to cause configuration error
				if !configHasVariant {
					prowJob = nil
				}
			}
		}
	}
	if prowJob == nil {
		return fmt.Errorf("configuration error, unable to find prow job matching %s with parameters=%v", selector, paramsToString(job.JobParams))
	}
	job.JobName = prowJob.Spec.Job
	var err error
	job.BuildCluster, err = m.scheduleWithContext(ctx, prowJob)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		klog.Error(err.Error())
		job.BuildCluster = prowJob.Spec.Cluster
	}
	// Delegate account dispersement to Test Platform via the platform's
	// cluster-profile set, which randomly selects an underlying account at
	// runtime. Non-amd64 launches keep the default per-platform profile.
	if req.Architecture == "amd64" {
		job.CloudProfileSet = platformProfileSets[req.Platform]
	}
	return nil
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
	reserved, _, existingMessage, err = m.reserveClusterSubmissionWithPrior(req, job)
	return reserved, existingMessage, err
}

// clusterReservation is a snapshot of a kept reservation, restored if a retry
// that took it over fails before reaching Prow.
type clusterReservation struct {
	request *JobRequest
	job     *Job
}

// reserveClusterSubmissionWithPrior is reserveClusterSubmission that also
// returns the reservation it took over when retrying a request ID whose
// earlier attempt was kept. A nil prior means this call created the
// reservation.
func (m *jobManager) reserveClusterSubmissionWithPrior(req *JobRequest, job *Job) (reserved bool, prior *clusterReservation, existingMessage string, err error) {
	if req == nil || job == nil {
		return false, nil, "", fmt.Errorf("cluster launch request is incomplete")
	}
	m.lock.Lock()
	defer m.lock.Unlock()

	if !m.launchReady {
		return false, nil, "", &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Cluster recovery is still in progress; retry shortly.", Retryable: true, JobID: job.Name}
	}

	activeCount, activeNames := activeClusterCountLocked(m)
	if current := m.requests[req.User]; current != nil && req.RequestKeyHash != "" && current.RequestKeyHash == req.RequestKeyHash {
		if !clusterInputFingerprintsMatch(current.InputFingerprint, current.RawInputFingerprint, req.InputFingerprint, req.RawInputFingerprint) ||
			current.RequestSource != req.RequestSource {
			return false, nil, "", &ToolError{Code: ToolErrorCodeRequestConflict, Message: "This request ID is already associated with a different launch request.", Retryable: false, JobID: job.Name}
		}
		if current.SubmissionInProgress {
			return false, nil, "", &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true, JobID: job.Name}
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
				return false, nil, "", clusterCapacityAdmissionError(req, m.jobs)
			}
			prior := &clusterReservation{request: cloneJobRequest(current), job: cloneJob(currentJob)}
			requestCopy := cloneJobRequest(req)
			requestCopy.SubmissionInProgress = true
			m.requests[req.User] = requestCopy
			return true, prior, "", nil
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
				return false, nil, "your cluster is already running, see your credentials again with the 'auth' command", nil
			}
			return false, nil, "", m.activeClusterAdmissionError(req, current)
		}
	}

	for _, currentJob := range m.jobs {
		if currentJob == nil || currentJob.RequestedBy != req.User || !isClusterLaunchMode(currentJob.Mode) || launchRequestFailure(currentJob) {
			continue
		}
		if len(currentJob.Credentials) > 0 && req.RequestSource == requestSourceSlack {
			return false, nil, "your cluster is already running, see your credentials again with the 'auth' command", nil
		}
		if currentJob.Name == job.Name && currentJob.RequestKeyHash == req.RequestKeyHash &&
			clusterInputFingerprintsMatch(currentJob.InputFingerprint, currentJob.RawInputFingerprint, req.InputFingerprint, req.RawInputFingerprint) {
			continue
		}
		return false, nil, "", m.activeClusterAdmissionError(req, jobRequestFromClusterJob(currentJob))
	}

	if activeCount >= m.maxClusters {
		return false, nil, "", clusterCapacityAdmissionError(req, m.jobs)
	}

	requestCopy := cloneJobRequest(req)
	requestCopy.SubmissionInProgress = true
	m.requests[req.User] = requestCopy
	m.jobs[job.Name] = cloneJob(job)
	return true, nil, "", nil
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

func (m *jobManager) activeClusterAdmissionError(req *JobRequest, existing *JobRequest) error {
	if req.RequestSource == requestSourceMCP {
		// Point the caller at the cluster they already own; the would-be new
		// job name is not addressable and would produce a NOT_FOUND status.
		jobID := ""
		if existing != nil && existing.Type == JobTypeInstall {
			jobID = existing.Name
		}
		return &ToolError{Code: ToolErrorCodeActiveClusterExists, Message: "You already have an active cluster launch.", Retryable: false, JobID: jobID}
	}
	if existing == nil || existing.Name == "" {
		return fmt.Errorf("you have already requested a cluster and it should be ready in ~ %d minutes", m.estimateCompletion(time.Time{})/time.Minute)
	}
	return fmt.Errorf("you have already requested a cluster and it should be ready in ~ %d minutes", m.estimateCompletion(existing.RequestedAt)/time.Minute)
}

func clusterCapacityAdmissionError(req *JobRequest, jobs map[string]*Job) error {
	if req.RequestSource == requestSourceMCP {
		// No job exists yet for this request, so there is no useful ID to
		// return. Callers can retry the same request ID later.
		return &ToolError{Code: ToolErrorCodeCapacityExhausted, Message: "No cluster capacity is currently available.", Retryable: true}
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
	if current := m.jobs[req.Name]; current != nil && current.RequestedBy == req.User && current.RequestKeyHash == req.RequestKeyHash &&
		clusterInputFingerprintsMatch(current.InputFingerprint, current.RawInputFingerprint, req.InputFingerprint, req.RawInputFingerprint) &&
		current.RequestSource == req.RequestSource {
		delete(m.jobs, req.Name)
	}
}

// restoreClusterReservation puts back a kept reservation that a failed retry
// took over, instead of deleting it as rollbackClusterReservation would.
func (m *jobManager) restoreClusterReservation(req *JobRequest, prior *clusterReservation) {
	if req == nil || prior == nil {
		return
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	if current := m.requests[req.User]; matchesClusterReservation(current, req) {
		restored := cloneJobRequest(prior.request)
		restored.SubmissionInProgress = false
		m.requests[req.User] = restored
	}
	if current := m.jobs[req.Name]; prior.job != nil && (current == nil || current.RequestedBy == req.User && current.RequestKeyHash == req.RequestKeyHash) {
		m.jobs[req.Name] = cloneJob(prior.job)
	}
}

func matchesClusterReservation(current, req *JobRequest) bool {
	if current == nil || req == nil || current.Name != req.Name || current.User != req.User || current.RequestSource != req.RequestSource {
		return false
	}
	return current.RequestKeyHash == req.RequestKeyHash &&
		clusterInputFingerprintsMatch(current.InputFingerprint, current.RawInputFingerprint, req.InputFingerprint, req.RawInputFingerprint)
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
	job, err := validateDurableClusterJob(pj, req, m.maxAge)
	if err != nil {
		return LaunchResult{}, false, err
	}
	summary, _, err := m.recordAcceptedClusterJob(req, job, pj)
	if err != nil {
		return LaunchResult{}, false, err
	}
	return LaunchResult{Cluster: summary, Replayed: true}, true, nil
}

// recordAcceptedClusterJob stores a launch that Prow accepted and starts its
// monitor. It holds the job's lifecycle lock so that a concurrent destroy is
// either merged from the cache or applied after the record, never overwritten.
func (m *jobManager) recordAcceptedClusterJob(req *JobRequest, planned *Job, pj *prowapiv1.ProwJob) (ClusterSummary, *Job, error) {
	if planned == nil {
		return ClusterSummary{}, nil, fmt.Errorf("accepted cluster job is missing")
	}
	unlock := m.lifecycleLocks.lock(planned.Name)
	defer unlock()

	accepted := cloneJob(planned)
	// The caller only records a job after Prow accepted it (or after a
	// successful Create whose immediate read failed), so this record is
	// addressable even when the ProwJob snapshot is synthetic.
	accepted.ProwJobAccepted = true
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
	if accepted.RequestedAt.IsZero() {
		accepted.RequestedAt = time.Now()
	}
	if accepted.ExpiresAt.IsZero() {
		accepted.ExpiresAt = accepted.RequestedAt.Add(m.maxAge)
	}
	if !isTerminalProwState(accepted.State) {
		accepted.State = prowapiv1.PendingState
	}

	// A destroy that ran while this ProwJob was not visible only marked the
	// cached record. Persist that request now so Prow cancels the recovered job.
	// The accepted snapshot has already merged the cached marker, so consult
	// the durable ProwJob annotations for whether the request was persisted.
	durableTerminationRequested := pj != nil && pj.Annotations[annotationTerminationRequested] == terminationMarkerValue
	if cached := m.cachedJobSnapshot(accepted.Name); cached != nil && cached.TerminationRequested &&
		!durableTerminationRequested && !isTerminalProwState(accepted.State) &&
		cached.RequestedBy == accepted.RequestedBy && cached.RequestKeyHash == accepted.RequestKeyHash &&
		clusterInputFingerprintsMatch(cached.InputFingerprint, cached.RawInputFingerprint, accepted.InputFingerprint, accepted.RawInputFingerprint) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		terminated, err := m.requestProwJobTermination(ctx, accepted.RequestedBy, accepted.Name, accepted.Mode)
		cancel()
		if err != nil {
			klog.Errorf("Unable to persist termination request for recovered launch %q: %v", accepted.Name, err)
		} else {
			applyProwJobStatus(accepted, terminated)
			markJobTerminationRequested(accepted)
			pj = terminated
		}
	}

	m.lock.Lock()
	if current := m.jobs[accepted.Name]; current != nil && current.RequestedBy == accepted.RequestedBy && current.RequestKeyHash == accepted.RequestKeyHash &&
		clusterInputFingerprintsMatch(current.InputFingerprint, current.RawInputFingerprint, accepted.InputFingerprint, accepted.RawInputFingerprint) {
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
		OriginalMessage:     job.OriginalMessage,
		User:                job.RequestedBy,
		UserName:            job.RequesterUserID,
		Inputs:              clusterInputs(job),
		Type:                jobType,
		Platform:            job.Platform,
		Channel:             job.RequestedChannel,
		RequestedAt:         job.RequestedAt,
		Name:                job.Name,
		JobName:             job.JobName,
		JobParams:           maps.Clone(job.JobParams),
		Architecture:        job.Architecture,
		ManagedClusterName:  job.ManagedClusterName,
		RequestKeyHash:      job.RequestKeyHash,
		InputFingerprint:    job.InputFingerprint,
		RawInputFingerprint: job.RawInputFingerprint,
		RequestSource:       job.RequestSource,
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

	jobRequest := &JobRequest{
		User:          request.SlackUserID,
		UserName:      request.UserName,
		Inputs:        [][]string{slices.Clone(request.Inputs)},
		Type:          JobTypeInstall,
		Platform:      request.Platform,
		JobParams:     maps.Clone(request.Parameters),
		Architecture:  request.Architecture,
		RequestedAt:   time.Now(),
		RequestSource: requestSourceMCP,
	}
	if err := validateLaunchInputs(jobRequest); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeInvalidArguments, Message: err.Error(), Retryable: false}
	}
	rawFingerprint := launchInputFingerprint(request.Inputs, request.Platform, request.Architecture, request.Parameters)
	if err := normalizeJobRequestOptions(jobRequest); err != nil {
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeInvalidArguments, Message: err.Error(), Retryable: false}
	}
	keyHash := requestKeyHash(request.ServicePrincipal, request.SlackUserID, request.RequestID)
	fingerprint := launchInputFingerprint(request.Inputs, jobRequest.Platform, jobRequest.Architecture, jobRequest.JobParams)
	name := deterministicProwJobName(m.clusterPrefix, keyHash)
	jobRequest.Name = name
	jobRequest.RequestKeyHash = keyHash
	jobRequest.InputFingerprint = fingerprint
	jobRequest.RawInputFingerprint = rawFingerprint

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
		return LaunchResult{}, &ToolError{Code: ToolErrorCodeActiveClusterExists, Message: fmt.Sprintf("You already have a ROSA cluster request; %d minutes have elapsed.", int(time.Since(cluster.CreationTimestamp())/time.Minute)), Retryable: false}
	}

	dmUnavailable := &ToolError{Code: ToolErrorCodeBackendUnavailable, Message: "Slack could not open a notification channel for this user.", Retryable: true, JobID: name}
	result, err := m.submitClusterJob(ctx, jobRequest, JobTypeLaunch, clusterSubmitOptions{
		ResolveChannel: func(ctx context.Context) (string, error) {
			if request.ResolveDM == nil {
				return "", dmUnavailable
			}
			channel, err := request.ResolveDM(ctx)
			if err != nil || strings.TrimSpace(channel) == "" {
				return "", dmUnavailable
			}
			return channel, nil
		},
		LookupDurable: func(ctx context.Context, request *JobRequest, _ *Job) (LaunchResult, bool, error) {
			return m.lookupDurableClusterRequest(ctx, request)
		},
	})
	if err != nil {
		if prepareErr, ok := errors.AsType[launchPrepareError](err); ok {
			return LaunchResult{}, classifyLaunchPreparationError(ctx, prepareErr.err, ToolErrorCodeInvalidArguments)
		}
		if startErr, ok := errors.AsType[launchStartError](err); ok {
			return LaunchResult{}, classifyLaunchPreparationError(ctx, startErr.err, ToolErrorCodeLaunchInitialization)
		}
		return LaunchResult{}, err
	}
	return result.Launch, nil
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
	m.defaultJobName(request)
	request.RequestSource = requestSourceSlack

	result, err := m.submitClusterJob(context.Background(), request, launchModeForRequest(request.Type), clusterSubmitOptions{
		WaitForURL:    true,
		LookupDurable: m.lookupOwnedClusterJob,
	})
	if err != nil {
		if prepareErr, ok := errors.AsType[launchPrepareError](err); ok {
			return "", prepareErr.err
		}
		if startErr, ok := errors.AsType[launchStartError](err); ok {
			return "", fmt.Errorf("the requested job cannot be started: %v", startErr.err)
		}
		return "", err
	}
	if result.ExistingMessage != "" {
		return result.ExistingMessage, nil
	}
	return legacyClusterStartedMessage(result.Job, result.URL), nil
}

// launchPrepareError wraps an error from resolving a launch request into a
// Prow job. Each entry point reports it in its own terms.
type launchPrepareError struct{ err error }

func (e launchPrepareError) Error() string { return e.err.Error() }
func (e launchPrepareError) Unwrap() error { return e.err }

// launchStartError wraps an error that definitively failed to create the
// ProwJob. Each entry point reports it in its own terms.
type launchStartError struct{ err error }

func (e launchStartError) Error() string { return e.err.Error() }
func (e launchStartError) Unwrap() error { return e.err }

// clusterSubmitOptions holds the parts of a cluster submission that differ
// between entry points.
type clusterSubmitOptions struct {
	// WaitForURL waits for Prow to assign the logs URL before returning.
	WaitForURL bool
	// ResolveChannel returns the notification channel once the reservation is
	// held. When nil, the request's channel is used.
	ResolveChannel func(ctx context.Context) (string, error)
	// LookupDurable finds and records the ProwJob for a create whose outcome is
	// unknown. found is false when Prow has no such job. A REQUEST_CONFLICT
	// error releases the reservation; any other error keeps it.
	LookupDurable func(ctx context.Context, request *JobRequest, planned *Job) (result LaunchResult, found bool, err error)
}

type clusterSubmitResult struct {
	Launch LaunchResult
	// Job is the job that was submitted to Prow.
	Job *Job
	// URL is the logs URL known when the submission returned, if any.
	URL string
	// ExistingMessage reports that the user's cluster is already running;
	// nothing was submitted.
	ExistingMessage string
}

// provisionalClusterJob is the placeholder job held by a reservation until
// the launch has been resolved into a Prow job.
func (m *jobManager) provisionalClusterJob(request *JobRequest, mode string) *Job {
	return &Job{
		Name:                request.Name,
		Mode:                mode,
		State:               prowapiv1.PendingState,
		OriginalMessage:     request.OriginalMessage,
		Platform:            request.Platform,
		JobParams:           maps.Clone(request.JobParams),
		RequestedBy:         request.User,
		RequesterUserID:     request.UserName,
		RequestedChannel:    request.Channel,
		RequestedAt:         request.RequestedAt,
		ExpiresAt:           request.RequestedAt.Add(m.maxAge),
		Architecture:        request.Architecture,
		WorkflowName:        request.WorkflowName,
		ManagedClusterName:  request.ManagedClusterName,
		RequestKeyHash:      request.RequestKeyHash,
		InputFingerprint:    request.InputFingerprint,
		RawInputFingerprint: request.RawInputFingerprint,
		RequestSource:       request.RequestSource,
	}
}

// submitClusterJob reserves admission for a validated and normalized launch
// request, resolves it into a Prow job, creates the ProwJob, and records the
// accepted launch. Every error return leaves the reservation rolled back,
// restored, or deliberately kept for a ProwJob that may still appear, so
// callers do no cleanup. Errors from resolving the request are returned as
// launchPrepareError and definitive create failures as launchStartError.
func (m *jobManager) submitClusterJob(ctx context.Context, request *JobRequest, mode string, opts clusterSubmitOptions) (clusterSubmitResult, error) {
	reserved, prior, existingMessage, err := m.reserveClusterSubmissionWithPrior(request, m.provisionalClusterJob(request, mode))
	if existingMessage != "" {
		return clusterSubmitResult{ExistingMessage: existingMessage}, nil
	}
	if err != nil {
		return clusterSubmitResult{}, err
	}
	if !reserved {
		if request.RequestSource == requestSourceMCP {
			return clusterSubmitResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "The launch is still being initialized. Retry with the same request ID.", Retryable: true, JobID: request.Name}
		}
		return clusterSubmitResult{}, fmt.Errorf("you have already requested a cluster and it should be ready in ~ 30 minutes")
	}

	reservationFinalized := false
	defer func() {
		if reservationFinalized {
			return
		}
		// A retry that took over a reservation kept by an earlier ambiguous
		// attempt must not delete it; that ProwJob may still appear.
		if prior != nil {
			m.restoreClusterReservation(request, prior)
		} else {
			m.rollbackClusterReservation(request)
		}
	}()
	// keep finalizes the reservation without a recorded ProwJob. It stays
	// until the ProwJob appears, a retry reconciles it, or the user runs done.
	keep := func(job *Job) {
		m.updateClusterReservation(request, job, false)
		reservationFinalized = true
	}

	if opts.ResolveChannel != nil {
		channel, err := opts.ResolveChannel(ctx)
		if err != nil {
			return clusterSubmitResult{}, err
		}
		request.Channel = channel
	}
	if err := ctx.Err(); err != nil {
		return clusterSubmitResult{}, launchPrepareError{err: err}
	}

	job, err := prepareProwLaunch(ctx, m, request)
	if err != nil {
		return clusterSubmitResult{}, launchPrepareError{err: err}
	}
	job.RequestKeyHash = request.RequestKeyHash
	job.InputFingerprint = request.InputFingerprint
	job.RawInputFingerprint = request.RawInputFingerprint
	job.RequestSource = request.RequestSource
	job.RequestedChannel = request.Channel
	job.RequestedBy = request.User
	job.RequesterUserID = request.UserName
	job.RequestedAt = request.RequestedAt
	job.ExpiresAt = request.RequestedAt.Add(m.maxAge)
	m.updateClusterReservation(request, job, true)

	submissionJob := cloneJob(job)
	prowURL, createErr := m.newJobWithContext(ctx, submissionJob, opts.WaitForURL)
	if createErr != nil {
		outcome, unknown := classifyCreateOutcome(createErr)
		if !unknown {
			return clusterSubmitResult{}, launchStartError{err: createErr}
		}
		result, err := m.reconcileCreateOutcome(ctx, request, submissionJob, createErr, outcome, opts)
		if isRequestConflictError(err) {
			m.rollbackClusterReservation(request)
			reservationFinalized = true
			return clusterSubmitResult{}, err
		}
		if err != nil {
			keep(submissionJob)
			return clusterSubmitResult{}, err
		}
		reservationFinalized = true
		return result, nil
	}

	// A successful Create response is durable acceptance even if an immediate
	// read is briefly unavailable. Keep the reservation and start monitoring.
	var acceptedProwJob *prowapiv1.ProwJob
	if ctx.Err() == nil {
		readCtx, cancel := directProwJobReadContext(ctx)
		acceptedProwJob, err = directProwJobRead(m, readCtx, request.Name)
		cancel()
		if err != nil {
			acceptedProwJob = nil
		}
	}
	if acceptedProwJob != nil {
		if _, err := validateDurableClusterJob(acceptedProwJob, request, m.maxAge); err != nil {
			keep(submissionJob)
			if request.RequestSource == requestSourceMCP {
				return clusterSubmitResult{}, err
			}
			return clusterSubmitResult{}, fmt.Errorf("the requested job could not be verified")
		}
	}
	if prowURL != "" {
		submissionJob.URL = prowURL
	}
	summary, _, err := m.recordAcceptedClusterJob(request, submissionJob, acceptedProwJob)
	if err != nil {
		keep(submissionJob)
		if request.RequestSource == requestSourceMCP {
			return clusterSubmitResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: "Prow accepted the launch, but its current status could not be read. Retry with the same request ID.", Retryable: true, JobID: request.Name}
		}
		return clusterSubmitResult{}, fmt.Errorf("the requested job is taking longer than expected to start: %v", err)
	}
	reservationFinalized = true
	return clusterSubmitResult{Launch: LaunchResult{Cluster: summary}, Job: submissionJob, URL: prowURL}, nil
}

// createOutcome is what a failed ProwJob create says about whether Prow
// accepted the job.
type createOutcome int

const (
	// createAlreadyExists means a ProwJob with the planned name exists.
	createAlreadyExists createOutcome = iota
	// createAmbiguous means the create may or may not have been persisted.
	createAmbiguous
	// createAccepted means Prow accepted the job but its URL was not seen.
	createAccepted
)

// classifyCreateOutcome reports whether createErr leaves the ProwJob's
// existence unknown, and how. Any other error definitively failed the create.
func classifyCreateOutcome(createErr error) (createOutcome, bool) {
	if prowCreateErr, ok := errors.AsType[prowJobCreateError](createErr); ok {
		cause := prowCreateErr.Unwrap()
		if apierrors.IsAlreadyExists(cause) {
			return createAlreadyExists, true
		}
		if isAmbiguousProwCreateError(cause) {
			return createAmbiguous, true
		}
		return 0, false
	}
	if _, ok := errors.AsType[prowJobAcceptedError](createErr); ok {
		return createAccepted, true
	}
	return 0, false
}

// reconcileCreateOutcome looks up the ProwJob after a create whose outcome is
// unknown. Lookups are skipped once ctx has ended, because their result could
// not be trusted. A found ProwJob is recorded and returned. A REQUEST_CONFLICT
// error means the reservation must be released; any other error means it must
// be kept, since the ProwJob may still appear.
func (m *jobManager) reconcileCreateOutcome(ctx context.Context, request *JobRequest, submissionJob *Job, createErr error, outcome createOutcome, opts clusterSubmitOptions) (clusterSubmitResult, error) {
	if ctx.Err() == nil {
		result, found, err := opts.LookupDurable(ctx, request, submissionJob)
		if found {
			return clusterSubmitResult{Launch: result, Job: submissionJob, URL: result.Cluster.LogsURL}, nil
		}
		if isRequestConflictError(err) {
			return clusterSubmitResult{}, err
		}
		// A structured caller is told why the lookup failed. An ambiguous
		// create is reported as possibly accepted either way.
		if err != nil && outcome != createAmbiguous && request.RequestSource == requestSourceMCP {
			return clusterSubmitResult{}, err
		}
	}
	if request.RequestSource != requestSourceMCP {
		return clusterSubmitResult{}, fmt.Errorf("%v; if the cluster does not appear shortly, run `done` to clear this request", createErr)
	}
	message := "Prow may be processing the launch, but its status could not be verified. Retry with the same request ID."
	switch outcome {
	case createAlreadyExists:
		message = "The launch is still being initialized. Retry with the same request ID."
	case createAmbiguous:
		message = "Prow may have accepted the launch. Retry with the same request ID to reconcile it."
	}
	return clusterSubmitResult{}, &ToolError{Code: ToolErrorCodeLaunchInitialization, Message: message, Retryable: true, JobID: request.Name}
}

// lookupOwnedClusterJob is the LookupDurable step for Slack launches, which
// have no request key: the ProwJob is accepted when the requesting user owns
// it with the launch's mode.
func (m *jobManager) lookupOwnedClusterJob(ctx context.Context, request *JobRequest, planned *Job) (LaunchResult, bool, error) {
	readCtx, cancel := directProwJobReadContext(ctx)
	defer cancel()
	pj, err := directProwJobRead(m, readCtx, request.Name)
	if apierrors.IsNotFound(err) {
		return LaunchResult{}, false, nil
	}
	if err != nil {
		return LaunchResult{}, false, err
	}
	if !prowJobMatches(pj, request.User, planned.Mode) {
		return LaunchResult{}, false, fmt.Errorf("the requested job could not be verified")
	}
	summary, _, err := m.recordAcceptedClusterJob(request, planned, pj)
	if err != nil {
		return LaunchResult{}, false, err
	}
	return LaunchResult{Cluster: summary, Replayed: true}, true, nil
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

// getPullRequestWithContext looks up a pull request through the REST API. The
// vendored client has no context-aware variant, so the call runs on its own
// goroutine and is abandoned if ctx ends first. Abandoning it is safe: the
// lookup is read-only and its result send never blocks.
func getPullRequestWithContext(ctx context.Context, client github.Client, org, repo string, number int) (*github.PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type pullRequestResult struct {
		pr  *github.PullRequest
		err error
	}
	done := make(chan pullRequestResult, 1)
	go func() {
		pr, err := client.GetPullRequest(org, repo, number)
		done <- pullRequestResult{pr: pr, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		if result.err != nil {
			return nil, result.err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.pr, nil
	}
}
