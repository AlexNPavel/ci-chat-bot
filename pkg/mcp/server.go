// Package mcp exposes user-attributed ordinary Prow cluster operations over
// authenticated MCP Streamable HTTP.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openshift/ci-chat-bot/pkg/manager"
	chatmetrics "github.com/openshift/ci-chat-bot/pkg/metrics"
	botversion "github.com/openshift/ci-chat-bot/pkg/version"
	"github.com/slack-go/slack"
	"k8s.io/klog/v2"
)

const (
	// ServicePrincipal identifies the configured Chai connector. It is always
	// assigned by this server and never comes from an MCP request.
	ServicePrincipal = "chai"

	// MaxRequestBodyBytes bounds the JSON-RPC body accepted by the MCP endpoint.
	MaxRequestBodyBytes = 1 << 20

	defaultSubmitTimeout = 60 * time.Second
)

// SlackClient is the subset of Slack's API needed to verify an initiating
// human and open that user's notification DM.
type SlackClient interface {
	AuthTestContext(context.Context) (*slack.AuthTestResponse, error)
	GetUserInfoContext(context.Context, string) (*slack.User, error)
	OpenConversationContext(context.Context, *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
}

// MCPRecorder records bounded MCP tool metrics.
type MCPRecorder interface {
	RecordMCPOperation(tool, outcome string, duration time.Duration)
}

// Config contains the dependencies for the MCP endpoint. Token is called for
// every HTTP request so secret-agent rotations take effect without restart.
type Config struct {
	Token          func() []byte
	PublicURL      string
	ClusterManager manager.ClusterManager
	SlackClient    SlackClient
	Metrics        MCPRecorder
	SubmitTimeout  time.Duration
}

// NewHandler builds the authenticated, stateless Streamable HTTP endpoint.
func NewHandler(config Config) (http.Handler, error) {
	origin, err := PublicOrigin(config.PublicURL)
	if err != nil {
		return nil, err
	}
	if config.Token == nil {
		return nil, fmt.Errorf("MCP token getter is required")
	}
	if len(bytes.TrimSpace(config.Token())) == 0 {
		return nil, fmt.Errorf("MCP token is empty")
	}
	if isNilDependency(config.ClusterManager) {
		return nil, fmt.Errorf("MCP cluster manager is required")
	}
	if isNilDependency(config.SlackClient) {
		return nil, fmt.Errorf("MCP Slack client is required")
	}
	if config.SubmitTimeout <= 0 {
		config.SubmitTimeout = defaultSubmitTimeout
	}
	if config.Metrics == nil {
		config.Metrics = chatmetrics.NoopMCPRecorder{}
	}

	service := &service{
		manager:       config.ClusterManager,
		identity:      newIdentityResolver(config.SlackClient),
		slack:         config.SlackClient,
		metrics:       config.Metrics,
		submitTimeout: config.SubmitTimeout,
	}

	version := botversion.Get().GitVersion
	if version == "" {
		version = "unknown"
	}
	protocolServer := mcp.NewServer(&mcp.Implementation{Name: "ci-chat-bot", Version: version}, &mcp.ServerOptions{
		Instructions: "This server manages only ordinary Prow OpenShift clusters. Every tool requires the initiating human's Slack user ID; Chai must supply it from trusted conversation context. Cluster ownership is checked by the manager. Provisioning is asynchronous, and credentials are sensitive.",
	})
	service.registerTools(protocolServer)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return protocolServer
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true,
		MaxRequestBodyBytes:          MaxRequestBodyBytes,
	})
	return authenticateAndCheckOrigin(transport, config.Token, origin), nil
}

// DisabledHandler is mounted at /mcp when MCP has no configured token file.
func DisabledHandler() http.Handler { return http.NotFoundHandler() }

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// ValidateEndpointConfig validates the CLI values before startup initializes
// clients. An empty token file disables MCP; a public endpoint is then unused.
func ValidateEndpointConfig(tokenFile, publicURL string) error {
	if tokenFile == "" {
		if publicURL != "" {
			if strings.TrimSpace(publicURL) == "" {
				return fmt.Errorf("--mcp-public-url must not be empty or whitespace")
			}
			return fmt.Errorf("--mcp-public-url requires --mcp-token-file")
		}
		return nil
	}
	if strings.TrimSpace(tokenFile) == "" || tokenFile != strings.TrimSpace(tokenFile) {
		return fmt.Errorf("--mcp-token-file must not contain leading or trailing whitespace")
	}
	_, err := PublicOrigin(publicURL)
	return err
}

// PublicOrigin validates a configured public MCP URL and returns its canonical
// HTTPS origin. The endpoint path must be exactly /mcp.
func PublicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("--mcp-public-url is invalid: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil || u.Path != "/mcp" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("--mcp-public-url must be an HTTPS URL with the exact /mcp path and no credentials, query, or fragment")
	}
	canonical, err := canonicalOrigin(u)
	if err != nil {
		return "", fmt.Errorf("--mcp-public-url is invalid: %w", err)
	}
	return canonical, nil
}

func authenticateAndCheckOrigin(next http.Handler, token func() []byte, expectedOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, bytes.TrimSpace(token())) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if len(origins) == 1 {
			got, err := parseOrigin(origins[0])
			if err != nil || got != expectedOrigin {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func authorized(r *http.Request, expected []byte) bool {
	if len(expected) == 0 {
		return false
	}
	authorization := r.Header.Values("Authorization")
	if len(authorization) != 1 {
		return false
	}
	parts := strings.Fields(authorization[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return false
	}
	// Compare fixed-size hashes so token length does not change the comparison
	// time. Hashing also ensures the constant-time comparison always sees the
	// same number of bytes.
	want := sha256.Sum256(expected)
	got := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func parseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("invalid origin")
	}
	return canonicalOrigin(u)
}

func canonicalOrigin(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("origin host is empty")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("origin port is empty")
	}
	port := u.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("origin port is invalid")
		}
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

type service struct {
	manager       manager.ClusterManager
	identity      *identityResolver
	slack         SlackClient
	metrics       MCPRecorder
	submitTimeout time.Duration
}

type toolFunc func(context.Context, json.RawMessage) (any, error)

func (s *service) addTool(server *mcp.Server, tool *mcp.Tool, handler toolFunc) {
	server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started := time.Now()
		args := json.RawMessage(nil)
		if request != nil && request.Params != nil {
			args = request.Params.Arguments
		}
		var audit struct {
			SlackUserID string `json:"slack_user_id"`
			RequestID   string `json:"request_id"`
			JobID       string `json:"job_id"`
		}
		_ = json.Unmarshal(args, &audit)

		data, callErr := handler(ctx, args)
		outcome := "success"
		envelope := map[string]any{"data": data}
		jobID := audit.JobID
		if callErr != nil {
			outcome = "error"
			domain := safeToolError(tool.Name, callErr)
			envelope = map[string]any{"error": domain}
			if domain.JobID != "" {
				jobID = domain.JobID
			}
		} else if summary, ok := data.(manager.LaunchResult); ok {
			jobID = summary.Cluster.JobID
		} else if credentials, ok := data.(manager.ClusterCredentials); ok {
			jobID = credentials.JobID
		} else if termination, ok := data.(manager.TerminationResult); ok {
			jobID = termination.JobID
		}

		encoded, err := json.Marshal(envelope)
		if err != nil {
			encoded = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"The operation could not be completed.","retryable":false}}`)
			outcome = "error"
		}
		duration := time.Since(started)
		s.metrics.RecordMCPOperation(tool.Name, outcome, duration)
		fields := []any{
			"service_principal", ServicePrincipal,
			"slack_user_id", safeSlackID(audit.SlackUserID),
			"job_id", safeJobID(jobID),
			"request_key_hash", requestKeyHash(audit.SlackUserID, audit.RequestID),
			"operation", tool.Name,
			"outcome", outcome,
		}
		klog.InfoS("MCP tool audit", fields...)
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
			StructuredContent: json.RawMessage(encoded),
			IsError:           outcome == "error",
		}, nil
	})
}

func safeToolError(tool string, err error) manager.ToolError {
	if value, ok := errors.AsType[manager.ToolError](err); ok {
		return sanitizeManagerToolError(value)
	}
	var pointer *manager.ToolError
	if errors.As(err, &pointer) && pointer != nil {
		return sanitizeManagerToolError(*pointer)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if tool == "launch_cluster" {
			return manager.ToolError{
				Code:      manager.ToolErrorCodeLaunchInitialization,
				Message:   "Launch submission timed out. Retry using the same request_id to recover the accepted job.",
				Retryable: true,
			}
		}
		return manager.ToolError{
			Code:      manager.ToolErrorCodeBackendUnavailable,
			Message:   "The operation timed out. Retry the request using the same request_id when launching.",
			Retryable: true,
		}
	}
	if errors.Is(err, context.Canceled) {
		return manager.ToolError{
			Code:      manager.ToolErrorCodeBackendUnavailable,
			Message:   "The operation was canceled. Retry the request; use the same request_id when launching.",
			Retryable: true,
		}
	}
	return manager.ToolError{
		Code:      manager.ToolErrorCodeInternal,
		Message:   "The operation could not be completed.",
		Retryable: false,
	}
}

func sanitizeManagerToolError(value manager.ToolError) manager.ToolError {
	message, ok := safeToolErrorMessages[value.Code]
	if !ok {
		return manager.ToolError{
			Code:      manager.ToolErrorCodeInternal,
			Message:   safeToolErrorMessages[manager.ToolErrorCodeInternal],
			Retryable: false,
		}
	}
	value.Message = message
	value.JobID = safeJobID(value.JobID)
	return value
}

var safeToolErrorMessages = map[string]string{
	manager.ToolErrorCodeInvalidArguments:       "One or more arguments are missing or invalid.",
	manager.ToolErrorCodeInvalidIdentity:        "slack_user_id must identify an active human in Cluster Bot's workspace with a Red Hat email address.",
	manager.ToolErrorCodeActiveClusterExists:    "You already have an active cluster. Wait for it to finish or destroy an eligible cluster before launching another.",
	manager.ToolErrorCodeCapacityExhausted:      "Cluster capacity is temporarily exhausted. Retry later.",
	manager.ToolErrorCodeRequestConflict:        "This request_id was already used with different launch arguments.",
	manager.ToolErrorCodeLaunchInitialization:   "The launch could not be initialized. Retry with the same request_id.",
	manager.ToolErrorCodeNotFound:               "Cluster not found.",
	manager.ToolErrorCodeCredentialsUnavailable: "Cluster credentials are unavailable for this job in its current state. Check cluster status.",
	manager.ToolErrorCodeBackendUnavailable:     "A required backend is temporarily unavailable. Retry later.",
	manager.ToolErrorCodeInternal:               "The operation could not be completed.",
}

func requestKeyHash(slackUserID, requestID string) string {
	if requestID == "" || !isSlackUserID(slackUserID) {
		return ""
	}
	return manager.ScopedRequestKeyHash(ServicePrincipal, slackUserID, requestID)
}

func safeSlackID(value string) string {
	if !isSlackUserID(value) {
		return ""
	}
	return value
}

func safeJobID(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		switch {
		case 'a' <= char && char <= 'z', '0' <= char && char <= '9', char == '-', char == '.':
		default:
			return ""
		}
	}
	return value
}
