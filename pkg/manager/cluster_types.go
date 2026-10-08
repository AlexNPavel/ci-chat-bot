package manager

import (
	"context"
	"fmt"
	"time"
)

const (
	annotationRequestKeyHash       = "ci-chat-bot.openshift.io/request-key-hash"
	annotationInputFingerprint     = "ci-chat-bot.openshift.io/input-fingerprint"
	annotationRequestSource        = "ci-chat-bot.openshift.io/request-source"
	annotationTerminationRequested = "ci-chat-bot.openshift.io/termination-requested"
)

const (
	ToolErrorCodeInvalidArguments       = "INVALID_ARGUMENTS"
	ToolErrorCodeInvalidIdentity        = "INVALID_IDENTITY"
	ToolErrorCodeActiveClusterExists    = "ACTIVE_CLUSTER_EXISTS"
	ToolErrorCodeCapacityExhausted      = "CAPACITY_EXHAUSTED"
	ToolErrorCodeRequestConflict        = "REQUEST_CONFLICT"
	ToolErrorCodeLaunchInitialization   = "LAUNCH_INITIALIZATION"
	ToolErrorCodeNotFound               = "NOT_FOUND"
	ToolErrorCodeCredentialsUnavailable = "CREDENTIALS_UNAVAILABLE"
	ToolErrorCodeBackendUnavailable     = "BACKEND_UNAVAILABLE"
	ToolErrorCodeInternal               = "INTERNAL_ERROR"
)

// ClusterManager exposes ordinary Prow cluster operations to structured callers.
// Implementations enforce ownership for every operation.
type ClusterManager interface {
	SubmitCluster(ctx context.Context, req ClusterLaunchRequest) (LaunchResult, error)
	GetClusterStatus(ctx context.Context, user, jobID string) (ClusterSummary, error)
	GetClusterCredentials(ctx context.Context, user, jobID string) (ClusterCredentials, error)
	ListClusters(ctx context.Context, user string) ([]ClusterSummary, error)
	DestroyCluster(ctx context.Context, user, jobID string) (TerminationResult, error)
}

// ClusterLaunchRequest is the caller-verified request for one ordinary Prow cluster.
// Inputs contains the single launch input group used by Slack launches.
type ClusterLaunchRequest struct {
	ServicePrincipal string                                `json:"service_principal"`
	SlackUserID      string                                `json:"slack_user_id"`
	UserName         string                                `json:"user_name"`
	RequestID        string                                `json:"request_id"`
	Inputs           []string                              `json:"inputs"`
	Platform         string                                `json:"platform,omitempty"`
	Architecture     string                                `json:"architecture,omitempty"`
	Parameters       map[string]string                     `json:"parameters,omitempty"`
	ResolveDM        func(context.Context) (string, error) `json:"-"`
}

// ClusterSummary is a secret-free snapshot of a user's ordinary Prow cluster.
type ClusterSummary struct {
	JobID        string            `json:"job_id"`
	SlackUserID  string            `json:"slack_user_id"`
	UserName     string            `json:"user_name,omitempty"`
	Status       string            `json:"status"`
	Inputs       [][]string        `json:"inputs,omitempty"`
	Platform     string            `json:"platform,omitempty"`
	Architecture string            `json:"architecture,omitempty"`
	Parameters   map[string]string `json:"parameters,omitempty"`
	RequestedAt  time.Time         `json:"requested_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
	LogsURL      string            `json:"logs_url,omitempty"`
	ConsoleURL   string            `json:"console_url,omitempty"`
	APIURL       string            `json:"api_url,omitempty"`
	Failure      string            `json:"failure,omitempty"`
}

// ClusterCredentials contains sensitive access data. It must only be returned
// after the manager verifies that the caller owns the cluster.
type ClusterCredentials struct {
	JobID              string `json:"job_id"`
	Kubeconfig         string `json:"kubeconfig"`
	ConsoleURL         string `json:"console_url,omitempty"`
	APIURL             string `json:"api_url,omitempty"`
	ConsoleUsername    string `json:"console_username,omitempty"`
	ConsolePassword    string `json:"console_password,omitempty"`
	AccessInstructions string `json:"access_instructions,omitempty"`
}

// LaunchResult describes an accepted launch and whether it reuses an earlier request.
type LaunchResult struct {
	Cluster  ClusterSummary `json:"cluster"`
	Replayed bool           `json:"replayed"`
}

// TerminationResult confirms the lifecycle state after a shutdown request.
type TerminationResult struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// ToolError is a stable, structured error returned by ClusterManager operations.
type ToolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	JobID     string `json:"job_id,omitempty"`
}

func (e ToolError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("cluster operation failed (%s)", e.Code)
}
