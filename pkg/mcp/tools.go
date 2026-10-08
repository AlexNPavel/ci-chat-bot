package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/mail"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/slack-go/slack"
)

func (s *service) registerTools(server *mcp.Server) {
	s.addTool(server, &mcp.Tool{
		Name:        "launch_cluster",
		Title:       "Launch an OpenShift cluster",
		Description: "Launches one ordinary Prow cluster for the active human identified by slack_user_id; ownership and shared limits are enforced by Cluster Bot. Provisioning is asynchronous: success means Prow accepted the job, and the user receives the existing completion or failure DM. Poll get_cluster_status for readiness. Reuse the same request_id for every retry of one intended launch; the same ID and inputs return the original job, while changed inputs return REQUEST_CONFLICT. The connector must supply slack_user_id from trusted conversation context. Inputs are the existing single launch input group: one version or release image and optional PR references. Platform availability depends on configured Prow jobs.",
		Annotations: annotations("Launch an OpenShift cluster", false, false, true),
		InputSchema: objectSchema(map[string]any{
			"slack_user_id": stringSchema("Initiating active human's Slack user ID", 1, 64),
			"request_id":    stringSchema("Stable caller-generated ID reused across retries of this launch", 1, 128),
			"inputs": map[string]any{
				"type":        "array",
				"description": "One version or release image, optionally followed by pull request references, as one launch input group.",
				"items":       stringSchema("Launch input", 1, 512),
				"minItems":    1,
			},
			"platform":     enumStringSchema("Optional supported launch platform; omit or pass an empty string to use the Slack launch default. Availability depends on configured Prow jobs.", manager.SupportedPlatforms),
			"architecture": enumStringSchema("Optional supported CPU architecture; omit or pass an empty string to use the Slack launch default.", manager.SupportedArchitectures),
			"parameters": map[string]any{
				"type":                 "object",
				"description":          "Existing launch parameters; flag-style options use an empty string. Only supported parameters are accepted; test and unknown parameters are rejected.",
				"propertyNames":        map[string]any{"enum": supportedMCPParameters()},
				"additionalProperties": map[string]any{"type": "string"},
			},
		}, "slack_user_id", "request_id", "inputs"),
		OutputSchema: envelopeSchema(launchResultSchema()),
	}, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input launchClusterInput
		if err := decodeArguments(raw, &input); err != nil {
			return nil, invalidArguments()
		}
		if err := validateSlackID(input.SlackUserID); err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 || input.RequestID != strings.TrimSpace(input.RequestID) {
			return nil, invalidArguments()
		}
		if len(input.Inputs) == 0 {
			return nil, invalidArguments()
		}
		for i, value := range input.Inputs {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > 512 {
				return nil, invalidArguments()
			}
			input.Inputs[i] = value
		}
		for _, value := range []string{input.Platform, input.Architecture} {
			if len(value) > 128 {
				return nil, invalidArguments()
			}
		}

		submitCtx, cancel := context.WithTimeout(ctx, s.submitTimeout)
		defer cancel()
		identity, err := s.identity.resolve(submitCtx, input.SlackUserID)
		if err != nil {
			return nil, err
		}
		request := manager.ClusterLaunchRequest{
			ServicePrincipal: ServicePrincipal,
			SlackUserID:      identity.SlackUserID,
			UserName:         identity.UserName,
			RequestID:        input.RequestID,
			Inputs:           append([]string(nil), input.Inputs...),
			Platform:         input.Platform,
			Architecture:     input.Architecture,
			Parameters:       cloneStringMap(input.Parameters),
			ResolveDM: func(dmCtx context.Context) (string, error) {
				channel, _, _, openErr := s.slack.OpenConversationContext(dmCtx, &slack.OpenConversationParameters{
					Users: []string{identity.SlackUserID},
				})
				if openErr != nil || channel == nil || channel.ID == "" {
					return "", manager.ToolError{
						Code:      manager.ToolErrorCodeBackendUnavailable,
						Message:   "Cluster Bot could not open a direct message for this Slack user. Retry with the same request_id.",
						Retryable: true,
					}
				}
				return channel.ID, nil
			},
		}
		return s.manager.SubmitCluster(submitCtx, request)
	})

	s.addTool(server, &mcp.Tool{
		Name:        "get_cluster_status",
		Title:       "Get cluster status",
		Description: "Returns the secret-free status snapshot for the ordinary Prow cluster owned by slack_user_id and identified by job_id. Provisioning is asynchronous; ready means Cluster Bot's monitor retrieved credentials and passed readiness checks. Failed, terminating, terminated, or expired states take precedence over cached credentials. Unknown and other-user job IDs both return NOT_FOUND.",
		Annotations: annotations("Get cluster status", true, false, true),
		InputSchema: objectSchema(map[string]any{
			"slack_user_id": stringSchema("Initiating active human's Slack user ID", 1, 64),
			"job_id":        stringSchema("Exact Prow job ID", 1, 128),
		}, "slack_user_id", "job_id"),
		OutputSchema: envelopeSchema(clusterSummarySchema()),
	}, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input clusterActionInput
		if err := decodeArguments(raw, &input); err != nil || !validJobID(input.JobID) {
			return nil, invalidArguments()
		}
		identity, err := s.identity.resolve(ctx, input.SlackUserID)
		if err != nil {
			return nil, err
		}
		return s.manager.GetClusterStatus(ctx, identity.SlackUserID, input.JobID)
	})

	s.addTool(server, &mcp.Tool{
		Name:        "get_cluster_credentials",
		Title:       "Get cluster credentials",
		Description: "Returns kubeconfig and available console access details only when the ordinary Prow cluster identified by job_id is ready and owned by the active human in slack_user_id. This result contains sensitive credentials: handle it as a secret, never log it, and do not share it. Unknown and other-user job IDs both return NOT_FOUND.",
		Annotations: annotations("Get cluster credentials", true, false, true),
		InputSchema: objectSchema(map[string]any{
			"slack_user_id": stringSchema("Initiating active human's Slack user ID", 1, 64),
			"job_id":        stringSchema("Exact Prow job ID", 1, 128),
		}, "slack_user_id", "job_id"),
		OutputSchema: envelopeSchema(credentialsSchema()),
	}, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input clusterActionInput
		if err := decodeArguments(raw, &input); err != nil || !validJobID(input.JobID) {
			return nil, invalidArguments()
		}
		identity, err := s.identity.resolve(ctx, input.SlackUserID)
		if err != nil {
			return nil, err
		}
		return s.manager.GetClusterCredentials(ctx, identity.SlackUserID, input.JobID)
	})

	s.addTool(server, &mcp.Tool{
		Name:        "list_clusters",
		Title:       "List my clusters",
		Description: "Lists only active ordinary Prow clusters owned by the active human in slack_user_id, including clusters launched through Slack or Chai. Other workflows and other users' clusters are not returned.",
		Annotations: annotations("List my clusters", true, false, true),
		InputSchema: objectSchema(map[string]any{
			"slack_user_id": stringSchema("Initiating active human's Slack user ID", 1, 64),
		}, "slack_user_id"),
		OutputSchema: envelopeSchema(map[string]any{
			"type":        "array",
			"description": "Active ordinary Prow clusters owned by this Slack user.",
			"items":       clusterSummarySchema(),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input userInput
		if err := decodeArguments(raw, &input); err != nil {
			return nil, invalidArguments()
		}
		if err := validateSlackID(input.SlackUserID); err != nil {
			return nil, err
		}
		identity, err := s.identity.resolve(ctx, input.SlackUserID)
		if err != nil {
			return nil, err
		}
		return s.manager.ListClusters(ctx, identity.SlackUserID)
	})

	s.addTool(server, &mcp.Tool{
		Name:        "destroy_cluster",
		Title:       "Destroy a cluster",
		Description: "Requests shutdown of the exact ordinary Prow cluster named by job_id after verifying that the active human in slack_user_id owns it. The response confirms a termination request; cloud cleanup is asynchronous and may continue after the cluster reaches terminating. Repeating the request for the same job is safe. Unknown and other-user job IDs both return NOT_FOUND.",
		Annotations: annotations("Destroy a cluster", false, true, true),
		InputSchema: objectSchema(map[string]any{
			"slack_user_id": stringSchema("Initiating active human's Slack user ID", 1, 64),
			"job_id":        stringSchema("Exact Prow job ID to terminate", 1, 128),
		}, "slack_user_id", "job_id"),
		OutputSchema: envelopeSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{"type": "string"},
				"status": map[string]any{"type": "string", "enum": []string{"provisioning", "ready", "failed", "terminating", "terminated", "expired"}},
			},
			"required":             []string{"job_id", "status"},
			"additionalProperties": false,
		}),
	}, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input clusterActionInput
		if err := decodeArguments(raw, &input); err != nil || !validJobID(input.JobID) {
			return nil, invalidArguments()
		}
		identity, err := s.identity.resolve(ctx, input.SlackUserID)
		if err != nil {
			return nil, err
		}
		return s.manager.DestroyCluster(ctx, identity.SlackUserID, input.JobID)
	})
}

type userInput struct {
	SlackUserID string `json:"slack_user_id"`
}

type clusterActionInput struct {
	SlackUserID string `json:"slack_user_id"`
	JobID       string `json:"job_id"`
}

type launchClusterInput struct {
	SlackUserID  string            `json:"slack_user_id"`
	RequestID    string            `json:"request_id"`
	Inputs       []string          `json:"inputs"`
	Platform     string            `json:"platform,omitempty"`
	Architecture string            `json:"architecture,omitempty"`
	Parameters   map[string]string `json:"parameters,omitempty"`
}

type verifiedIdentity struct {
	SlackUserID string
	UserName    string
}

type identityResolver struct {
	mu    sync.Mutex
	slack SlackClient
	team  string
}

func newIdentityResolver(client SlackClient) *identityResolver {
	return &identityResolver{slack: client}
}

func (r *identityResolver) resolve(ctx context.Context, userID string) (verifiedIdentity, error) {
	if err := validateSlackID(userID); err != nil {
		return verifiedIdentity{}, err
	}
	teamID, err := r.workspace(ctx)
	if err != nil {
		return verifiedIdentity{}, err
	}
	user, err := r.slack.GetUserInfoContext(ctx, userID)
	if err != nil {
		return verifiedIdentity{}, manager.ToolError{
			Code:      manager.ToolErrorCodeBackendUnavailable,
			Message:   "Slack could not verify this user. Retry the request later.",
			Retryable: true,
		}
	}
	if user == nil || user.ID != userID || user.Deleted || user.IsBot || user.IsAppUser || user.IsWorkflowBot || user.IsConnectorBot || user.IsRestricted || user.IsUltraRestricted || strings.EqualFold(user.Name, "slackbot") || strings.EqualFold(user.RealName, "slackbot") || user.TeamID == "" || user.TeamID != teamID {
		return verifiedIdentity{}, invalidIdentity()
	}
	username, ok := redHatUsername(user.Profile.Email)
	if !ok {
		return verifiedIdentity{}, invalidIdentity()
	}
	return verifiedIdentity{SlackUserID: user.ID, UserName: username}, nil
}

func (r *identityResolver) workspace(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.team != "" {
		return r.team, nil
	}
	response, err := r.slack.AuthTestContext(ctx)
	if err != nil || response == nil || response.TeamID == "" {
		return "", manager.ToolError{
			Code:      manager.ToolErrorCodeBackendUnavailable,
			Message:   "Cluster Bot could not verify its Slack workspace. Retry the request later.",
			Retryable: true,
		}
	}
	r.team = response.TeamID
	return r.team, nil
}

func redHatUsername(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", false
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil || parsed.Address != raw {
		return "", false
	}
	username, domain, ok := strings.Cut(raw, "@")
	if !ok || username == "" || strings.Contains(username, "@") || !strings.EqualFold(domain, "redhat.com") {
		return "", false
	}
	return username, true
}

func validateSlackID(userID string) error {
	if !isSlackUserID(userID) {
		return invalidArguments()
	}
	return nil
}

func isSlackUserID(userID string) bool {
	if len(userID) < 3 || len(userID) > 64 || (userID[0] != 'U' && userID[0] != 'W') {
		return false
	}
	for i := 1; i < len(userID); i++ {
		char := userID[i]
		switch {
		case 'A' <= char && char <= 'Z', '0' <= char && char <= '9':
		default:
			return false
		}
	}
	return true
}

func validJobID(jobID string) bool {
	return jobID != "" && jobID == strings.TrimSpace(jobID) && len(jobID) <= 128
}

func invalidArguments() error {
	return manager.ToolError{
		Code:      manager.ToolErrorCodeInvalidArguments,
		Message:   "One or more arguments are missing or invalid.",
		Retryable: false,
	}
}

func invalidIdentity() error {
	return manager.ToolError{
		Code:      manager.ToolErrorCodeInvalidIdentity,
		Message:   "slack_user_id must identify an active human in Cluster Bot's workspace with a Red Hat email address.",
		Retryable: false,
	}
}

func decodeArguments(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		return fmt.Errorf("arguments are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	cloned := make(map[string]string, len(input))
	maps.Copy(cloned, input)
	return cloned
}

func annotations(title string, readOnly, destructive, idempotent bool) *mcp.ToolAnnotations {
	destructiveHint := destructive
	openWorld := true
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    readOnly,
		DestructiveHint: &destructiveHint,
		IdempotentHint:  idempotent,
		OpenWorldHint:   &openWorld,
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func stringSchema(description string, minLength, maxLength int) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"minLength":   minLength,
		"maxLength":   maxLength,
	}
}

func enumStringSchema(description string, supported []string) map[string]any {
	enum := make([]string, 0, len(supported)+1)
	enum = append(enum, "")
	enum = append(enum, supported...)
	return map[string]any{
		"type":        "string",
		"description": description,
		"enum":        enum,
	}
}

func supportedMCPParameters() []string {
	parameters := make([]string, 0, len(manager.SupportedParameters))
	for _, parameter := range manager.SupportedParameters {
		if parameter != "test" {
			parameters = append(parameters, parameter)
		}
	}
	return parameters
}

func envelopeSchema(dataSchema any) map[string]any {
	errorSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type": "string",
				"enum": []string{
					manager.ToolErrorCodeInvalidArguments,
					manager.ToolErrorCodeInvalidIdentity,
					manager.ToolErrorCodeActiveClusterExists,
					manager.ToolErrorCodeCapacityExhausted,
					manager.ToolErrorCodeRequestConflict,
					manager.ToolErrorCodeLaunchInitialization,
					manager.ToolErrorCodeNotFound,
					manager.ToolErrorCodeCredentialsUnavailable,
					manager.ToolErrorCodeBackendUnavailable,
					manager.ToolErrorCodeInternal,
				},
			},
			"message":   map[string]any{"type": "string"},
			"retryable": map[string]any{"type": "boolean"},
			"job_id":    map[string]any{"type": "string"},
		},
		"required":             []string{"code", "message", "retryable"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"oneOf": []any{
			map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"data": dataSchema},
				"required":             []string{"data"},
				"additionalProperties": false,
			},
			map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"error": errorSchema},
				"required":             []string{"error"},
				"additionalProperties": false,
			},
		},
	}
}

func launchResultSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cluster":  clusterSummarySchema(),
			"replayed": map[string]any{"type": "boolean", "description": "True when this request reused an earlier launch."},
		},
		"required":             []string{"cluster", "replayed"},
		"additionalProperties": false,
	}
}

func clusterSummarySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"job_id":        map[string]any{"type": "string"},
			"slack_user_id": map[string]any{"type": "string"},
			"user_name":     map[string]any{"type": "string"},
			"status":        map[string]any{"type": "string", "enum": []string{"provisioning", "ready", "failed", "terminating", "terminated", "expired"}},
			"inputs":        map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
			"platform":      map[string]any{"type": "string"},
			"architecture":  map[string]any{"type": "string"},
			"parameters":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"requested_at":  dateTimeSchema(),
			"expires_at":    dateTimeSchema(),
			"completed_at":  nullableDateTimeSchema(),
			"logs_url":      map[string]any{"type": "string"},
			"console_url":   map[string]any{"type": "string"},
			"api_url":       map[string]any{"type": "string"},
			"failure":       map[string]any{"type": "string", "description": "Sanitized failure detail; contains no credentials."},
		},
		"required":             []string{"job_id", "slack_user_id", "status"},
		"additionalProperties": false,
	}
}

func credentialsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"job_id":              map[string]any{"type": "string"},
			"kubeconfig":          map[string]any{"type": "string", "description": "Secret cluster access configuration."},
			"console_url":         map[string]any{"type": "string"},
			"api_url":             map[string]any{"type": "string"},
			"console_username":    map[string]any{"type": "string"},
			"console_password":    map[string]any{"type": "string", "description": "Secret console password."},
			"access_instructions": map[string]any{"type": "string"},
		},
		"required":             []string{"job_id", "kubeconfig"},
		"additionalProperties": false,
	}
}

func dateTimeSchema() map[string]any {
	return map[string]any{"type": "string", "format": "date-time"}
}

func nullableDateTimeSchema() map[string]any {
	return map[string]any{"anyOf": []any{dateTimeSchema(), map[string]any{"type": "null"}}}
}
