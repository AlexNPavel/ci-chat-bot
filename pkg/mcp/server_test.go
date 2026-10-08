package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openshift/ci-chat-bot/pkg/manager"
	chatmetrics "github.com/openshift/ci-chat-bot/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
)

const (
	testToken  = "test-bearer-token"
	testOrigin = "https://cluster-bot.example.test"
)

func TestStreamableHTTPToolsAcrossProtocolVersions(t *testing.T) {
	for _, protocolVersion := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(protocolVersion, func(t *testing.T) {
			managerFake := &fakeClusterManager{}
			slackFake := newFakeSlack()
			registry := prometheus.NewRegistry()
			toolMetrics, err := chatmetrics.New(registry)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewHandler(Config{
				Token:          func() []byte { return []byte(testToken) },
				PublicURL:      testOrigin + "/mcp",
				ClusterManager: managerFake,
				SlackClient:    slackFake,
				Metrics:        toolMetrics,
			})
			if err != nil {
				t.Fatalf("NewHandler() error = %v", err)
			}
			httpServer := httptest.NewServer(handler)
			defer httpServer.Close()

			client := mcp.NewClient(&mcp.Implementation{Name: "mcp-test", Version: "1"}, nil)
			session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint:             httpServer.URL,
				HTTPClient:           mcpHTTPClient(testToken, testOrigin),
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
			if err != nil {
				t.Fatalf("Connect() error = %v", err)
			}
			defer closeMCPSession(t, session)

			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools() error = %v", err)
			}
			wantTools := map[string]bool{
				"launch_cluster":          false,
				"get_cluster_status":      false,
				"get_cluster_credentials": false,
				"list_clusters":           false,
				"destroy_cluster":         false,
			}
			outputSchemas := make(map[string]*jsonschema.Resolved, len(wantTools))
			for _, tool := range listed.Tools {
				if _, ok := wantTools[tool.Name]; !ok {
					t.Errorf("unexpected tool %q", tool.Name)
					continue
				}
				wantTools[tool.Name] = true
				if tool.InputSchema == nil {
					t.Errorf("tool %q has no input schema", tool.Name)
				}
				if tool.OutputSchema == nil {
					t.Errorf("tool %q has no output schema", tool.Name)
				} else {
					outputSchemas[tool.Name] = resolveOutputSchema(t, tool.OutputSchema)
				}
				if tool.Annotations == nil {
					t.Errorf("tool %q has no annotations", tool.Name)
				} else {
					if tool.Annotations.ReadOnlyHint != (tool.Name == "get_cluster_status" || tool.Name == "get_cluster_credentials" || tool.Name == "list_clusters") {
						t.Errorf("tool %q read-only annotation = %t", tool.Name, tool.Annotations.ReadOnlyHint)
					}
					wantDestructive := tool.Name == "destroy_cluster"
					if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != wantDestructive {
						t.Errorf("tool %q destructive annotation = %v, want %t", tool.Name, tool.Annotations.DestructiveHint, wantDestructive)
					}
				}
				inputSchema := decodeSchema(t, tool.InputSchema)
				if inputSchema["type"] != "object" {
					t.Errorf("tool %q input schema type = %v, want object", tool.Name, inputSchema["type"])
				}
				if outputSchema := decodeSchema(t, tool.OutputSchema); outputSchema["type"] != "object" {
					t.Errorf("tool %q output schema type = %v, want object", tool.Name, outputSchema["type"])
				}
				required, ok := inputSchema["required"].([]any)
				requiresSlackID := false
				if ok {
					for _, name := range required {
						if name == "slack_user_id" {
							requiresSlackID = true
						}
					}
				}
				if !requiresSlackID {
					t.Errorf("tool %q schema does not require slack_user_id", tool.Name)
				}
				if tool.Name == "launch_cluster" {
					assertLaunchInputEnums(t, inputSchema)
				}
			}
			if len(listed.Tools) != len(wantTools) {
				t.Fatalf("ListTools() returned %d tools, want %d", len(listed.Tools), len(wantTools))
			}
			for name, found := range wantTools {
				if !found {
					t.Errorf("tool %q was not listed", name)
				}
			}

			calls := []struct {
				name string
				args map[string]any
			}{
				{
					name: "launch_cluster",
					args: map[string]any{
						"slack_user_id": "U123",
						"request_id":    "stable-request-1",
						"inputs":        []string{"4.19", "openshift/installer#7160"},
						"platform":      "aws",
						"parameters":    map[string]string{"techpreview": ""},
					},
				},
				{name: "get_cluster_status", args: map[string]any{"slack_user_id": "U123", "job_id": "job-1"}},
				{name: "get_cluster_credentials", args: map[string]any{"slack_user_id": "U123", "job_id": "job-1"}},
				{name: "list_clusters", args: map[string]any{"slack_user_id": "U123"}},
				{name: "destroy_cluster", args: map[string]any{"slack_user_id": "U123", "job_id": "job-1"}},
			}
			for _, call := range calls {
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: call.name, Arguments: call.args})
				if err != nil {
					t.Fatalf("CallTool(%s) error = %v", call.name, err)
				}
				if result.IsError {
					t.Fatalf("CallTool(%s) returned an MCP error: %s", call.name, resultText(result))
				}
				var envelope struct {
					Data  json.RawMessage `json:"data"`
					Error json.RawMessage `json:"error"`
				}
				decodeStructured(t, result.StructuredContent, &envelope)
				if len(envelope.Data) == 0 || string(envelope.Data) == "null" || len(envelope.Error) != 0 {
					t.Fatalf("CallTool(%s) did not return data envelope: %s", call.name, resultText(result))
				}
				validateStructuredOutput(t, outputSchemas[call.name], result)
			}

			managerFake.mu.Lock()
			defer managerFake.mu.Unlock()
			if managerFake.submitCalls != 1 || managerFake.acceptedCalls != 1 {
				t.Errorf("launch counts = (%d submits, %d accepted), want (1, 1)", managerFake.submitCalls, managerFake.acceptedCalls)
			}
			if got := managerFake.lastRequest.UserName; got != "alice" {
				t.Errorf("launch username = %q, want alice", got)
			}
			if got := managerFake.lastRequest.ServicePrincipal; got != ServicePrincipal {
				t.Errorf("service principal = %q, want %q", got, ServicePrincipal)
			}
			if got := managerFake.lastRequest.Platform; got != "aws" {
				t.Errorf("launch platform = %q, want aws", got)
			}
			if got := managerFake.lastDM; got != "D123" {
				t.Errorf("launch DM = %q, want D123", got)
			}
			slackFake.mu.Lock()
			defer slackFake.mu.Unlock()
			if slackFake.dmCalls != 1 {
				t.Errorf("DM opens = %d, want 1", slackFake.dmCalls)
			}
			if slackFake.openedUser != "U123" {
				t.Errorf("DM user = %q, want verified user U123", slackFake.openedUser)
			}
		})
	}
}

func TestLaunchReplayDoesNotOpenAnotherDM(t *testing.T) {
	managerFake := &fakeClusterManager{replayed: true}
	slackFake := newFakeSlack()
	server, session := connectForTest(t, managerFake, slackFake, "2025-11-25")
	defer server.Close()
	defer closeMCPSession(t, session)

	callLaunch(t, session, map[string]any{
		"slack_user_id": "U123",
		"request_id":    "same-request",
		"inputs":        []string{"4.19"},
	})

	slackFake.mu.Lock()
	defer slackFake.mu.Unlock()
	if slackFake.dmCalls != 0 {
		t.Fatalf("DM opens = %d on replay, want 0", slackFake.dmCalls)
	}
	if managerFake.acceptedCalls != 0 {
		t.Fatalf("accepted submissions = %d on replay, want 0", managerFake.acceptedCalls)
	}
}

func TestInvalidIdentityDoesNotSubmitLaunch(t *testing.T) {
	tests := []struct {
		name string
		edit func(*slack.User)
	}{
		{name: "deactivated", edit: func(user *slack.User) { user.Deleted = true }},
		{name: "bot", edit: func(user *slack.User) { user.IsBot = true }},
		{name: "app user", edit: func(user *slack.User) { user.IsAppUser = true }},
		{name: "restricted", edit: func(user *slack.User) { user.IsRestricted = true }},
		{name: "slackbot", edit: func(user *slack.User) { user.Name = "slackbot" }},
		{name: "other workspace", edit: func(user *slack.User) { user.TeamID = "T-other" }},
		{name: "no Red Hat email", edit: func(user *slack.User) { user.Profile.Email = "alice@example.test" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			managerFake := &fakeClusterManager{}
			slackFake := newFakeSlack()
			test.edit(slackFake.user)
			server, session := connectForTest(t, managerFake, slackFake, "2026-07-28")
			defer server.Close()
			defer closeMCPSession(t, session)

			result := callLaunch(t, session, map[string]any{
				"slack_user_id": "U123",
				"request_id":    "request-1",
				"inputs":        []string{"4.19"},
			})
			if !result.IsError {
				t.Fatal("CallTool() IsError = false, want true")
			}
			assertErrorCode(t, session, "launch_cluster", result, manager.ToolErrorCodeInvalidIdentity)
			if managerFake.submitCalls != 0 {
				t.Fatalf("SubmitCluster calls = %d, want 0", managerFake.submitCalls)
			}
			slackFake.mu.Lock()
			defer slackFake.mu.Unlock()
			if slackFake.dmCalls != 0 {
				t.Fatalf("DM opens = %d, want 0", slackFake.dmCalls)
			}
		})
	}
}

func TestSlackLookupAndDMFailuresAreStructuredToolErrors(t *testing.T) {
	t.Run("workspace lookup", func(t *testing.T) {
		managerFake := &fakeClusterManager{}
		slackFake := newFakeSlack()
		slackFake.authErr = errors.New("upstream response with secret payload")
		server, session := connectForTest(t, managerFake, slackFake, "2025-11-25")
		defer server.Close()
		defer closeMCPSession(t, session)

		result := callLaunch(t, session, map[string]any{
			"slack_user_id": "U123",
			"request_id":    "request-1",
			"inputs":        []string{"4.19"},
		})
		if !result.IsError {
			t.Fatal("CallTool() IsError = false, want true")
		}
		assertErrorCode(t, session, "launch_cluster", result, manager.ToolErrorCodeBackendUnavailable)
		if strings.Contains(resultText(result), "secret payload") {
			t.Fatal("upstream Slack error leaked to MCP response")
		}
		if managerFake.submitCalls != 0 {
			t.Fatalf("SubmitCluster calls = %d, want 0", managerFake.submitCalls)
		}
	})

	t.Run("profile lookup", func(t *testing.T) {
		managerFake := &fakeClusterManager{}
		slackFake := newFakeSlack()
		slackFake.userErr = errors.New("users.info response with secret payload")
		server, session := connectForTest(t, managerFake, slackFake, "2026-07-28")
		defer server.Close()
		defer closeMCPSession(t, session)

		result := callLaunch(t, session, map[string]any{
			"slack_user_id": "U123",
			"request_id":    "request-1",
			"inputs":        []string{"4.19"},
		})
		if !result.IsError {
			t.Fatal("CallTool() IsError = false, want true")
		}
		assertErrorCode(t, session, "launch_cluster", result, manager.ToolErrorCodeBackendUnavailable)
		if strings.Contains(resultText(result), "secret payload") {
			t.Fatal("upstream Slack error leaked to MCP response")
		}
		if managerFake.submitCalls != 0 {
			t.Fatalf("SubmitCluster calls = %d, want 0", managerFake.submitCalls)
		}
	})

	t.Run("DM creation", func(t *testing.T) {
		managerFake := &fakeClusterManager{}
		slackFake := newFakeSlack()
		slackFake.dmErr = errors.New("upstream DM response with secret payload")
		server, session := connectForTest(t, managerFake, slackFake, "2026-07-28")
		defer server.Close()
		defer closeMCPSession(t, session)

		result := callLaunch(t, session, map[string]any{
			"slack_user_id": "U123",
			"request_id":    "request-2",
			"inputs":        []string{"4.19"},
		})
		if !result.IsError {
			t.Fatal("CallTool() IsError = false, want true")
		}
		assertErrorCode(t, session, "launch_cluster", result, manager.ToolErrorCodeBackendUnavailable)
		if strings.Contains(resultText(result), "secret payload") {
			t.Fatal("upstream Slack error leaked to MCP response")
		}
		if managerFake.acceptedCalls != 0 {
			t.Fatalf("Prow acceptance count = %d after DM failure, want 0", managerFake.acceptedCalls)
		}
	})
}

func TestMalformedToolArgumentsReturnStructuredError(t *testing.T) {
	server, session := connectForTest(t, &fakeClusterManager{}, newFakeSlack(), "2026-07-28")
	defer server.Close()
	defer closeMCPSession(t, session)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "launch_cluster",
		Arguments: map[string]any{"slack_user_id": 123, "request_id": "request-1", "inputs": []string{"4.19"}},
	})
	if err != nil {
		t.Fatalf("CallTool() protocol error = %v", err)
	}
	if !result.IsError {
		t.Fatal("CallTool() IsError = false, want true")
	}
	assertErrorCode(t, session, "launch_cluster", result, manager.ToolErrorCodeInvalidArguments)
}

func TestManagerErrorMessageIsSanitized(t *testing.T) {
	managerFake := &fakeClusterManager{
		submitErr: manager.ToolError{
			Code:      manager.ToolErrorCodeBackendUnavailable,
			Message:   "backend response includes secret payload: opaque-token",
			Retryable: true,
			JobID:     "job-1",
		},
	}
	server, session := connectForTest(t, managerFake, newFakeSlack(), "2026-07-28")
	defer server.Close()
	defer closeMCPSession(t, session)

	result := callLaunch(t, session, map[string]any{
		"slack_user_id": "U123",
		"request_id":    "request-1",
		"inputs":        []string{"4.19"},
	})
	if !result.IsError {
		t.Fatal("CallTool() IsError = false, want true")
	}
	assertErrorCode(t, session, "launch_cluster", result, manager.ToolErrorCodeBackendUnavailable)
	if strings.Contains(resultText(result), "secret payload") || strings.Contains(resultText(result), "opaque-token") {
		t.Fatalf("manager error detail leaked to MCP response: %s", resultText(result))
	}
	var envelope struct {
		Error manager.ToolError `json:"error"`
	}
	decodeStructured(t, result.StructuredContent, &envelope)
	if envelope.Error.Message != safeToolErrorMessages[manager.ToolErrorCodeBackendUnavailable] {
		t.Fatalf("sanitized message = %q, want %q", envelope.Error.Message, safeToolErrorMessages[manager.ToolErrorCodeBackendUnavailable])
	}
	if !envelope.Error.Retryable {
		t.Fatal("retryable flag = false, want true")
	}
}

func TestAllToolErrorsMatchTheirAdvertisedOutputSchemas(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "launch_cluster", args: map[string]any{"slack_user_id": "U123", "request_id": "request-1", "inputs": []string{"4.19"}}},
		{name: "get_cluster_status", args: map[string]any{"slack_user_id": "U123", "job_id": "job-1"}},
		{name: "get_cluster_credentials", args: map[string]any{"slack_user_id": "U123", "job_id": "job-1"}},
		{name: "list_clusters", args: map[string]any{"slack_user_id": "U123"}},
		{name: "destroy_cluster", args: map[string]any{"slack_user_id": "U123", "job_id": "job-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			managerFake := &fakeClusterManager{toolErrors: map[string]error{
				test.name: manager.ToolError{
					Code:      manager.ToolErrorCodeBackendUnavailable,
					Message:   "upstream returned a secret response body",
					Retryable: true,
				},
			}}
			server, session := connectForTest(t, managerFake, newFakeSlack(), "2026-07-28")
			defer server.Close()
			defer closeMCPSession(t, session)

			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: test.name, Arguments: test.args})
			if err != nil {
				t.Fatalf("CallTool(%s) error = %v", test.name, err)
			}
			if !result.IsError {
				t.Fatal("CallTool() IsError = false, want true")
			}
			assertErrorCode(t, session, test.name, result, manager.ToolErrorCodeBackendUnavailable)
			if strings.Contains(resultText(result), "secret response body") {
				t.Fatalf("manager error detail leaked: %s", resultText(result))
			}
		})
	}
}

func TestConcurrentIdentityResolutionIsSafe(t *testing.T) {
	managerFake := &fakeClusterManager{}
	slackFake := newFakeSlack()
	server, session := connectForTest(t, managerFake, slackFake, "2026-07-28")
	defer server.Close()
	defer closeMCPSession(t, session)

	const callers = 8
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_clusters",
				Arguments: map[string]any{"slack_user_id": "U123"},
			})
			if err != nil || result == nil || result.IsError {
				t.Errorf("concurrent CallTool() result = (%v, %v)", result, err)
			}
		})
	}
	wg.Wait()
}

func connectForTest(t *testing.T, managerFake *fakeClusterManager, slackFake *fakeSlack, protocolVersion string) (*httptest.Server, *mcp.ClientSession) {
	t.Helper()
	handler, err := NewHandler(Config{
		Token:          func() []byte { return []byte(testToken) },
		PublicURL:      testOrigin + "/mcp",
		ClusterManager: managerFake,
		SlackClient:    slackFake,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	httpServer := httptest.NewServer(handler)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           mcpHTTPClient(testToken, testOrigin),
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
	if err != nil {
		httpServer.Close()
		t.Fatalf("Connect() error = %v", err)
	}
	return httpServer, session
}

func closeMCPSession(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	if err := session.Close(); err != nil {
		t.Errorf("close MCP session: %v", err)
	}
}

func callLaunch(t *testing.T, session *mcp.ClientSession, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "launch_cluster", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(launch_cluster) error = %v", err)
	}
	return result
}

func assertErrorCode(t *testing.T, session *mcp.ClientSession, toolName string, result *mcp.CallToolResult, code string) {
	t.Helper()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() while validating error output schema: %v", err)
	}
	var outputSchema *jsonschema.Resolved
	for _, tool := range listed.Tools {
		if tool.Name == toolName {
			outputSchema = resolveOutputSchema(t, tool.OutputSchema)
			break
		}
	}
	if outputSchema == nil {
		t.Fatalf("%s output schema was not advertised", toolName)
	}
	validateStructuredOutput(t, outputSchema, result)
	var envelope struct {
		Error manager.ToolError `json:"error"`
	}
	decodeStructured(t, result.StructuredContent, &envelope)
	if envelope.Error.Code != code {
		t.Fatalf("MCP error code = %q, want %q (%s)", envelope.Error.Code, code, resultText(result))
	}
}

func resolveOutputSchema(t *testing.T, raw any) *jsonschema.Resolved {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal(output schema) error = %v", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatalf("Unmarshal(output schema) error = %v: %s", err, encoded)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve(output schema) error = %v: %s", err, encoded)
	}
	return resolved
}

func validateStructuredOutput(t *testing.T, schema *jsonschema.Resolved, result *mcp.CallToolResult) {
	t.Helper()
	if schema == nil {
		t.Fatal("output schema is nil")
	}
	if result == nil || result.StructuredContent == nil {
		t.Fatal("tool result has no structured content")
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal(structured content) error = %v", err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		t.Fatalf("Unmarshal(structured content) error = %v: %s", err, encoded)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("structured content does not match the advertised output schema: %v: %s", err, encoded)
	}
}

func assertLaunchInputEnums(t *testing.T, inputSchema map[string]any) {
	t.Helper()
	properties, ok := inputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("launch_cluster input schema has no properties")
	}
	assertEnum := func(name string, supported []string) {
		t.Helper()
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("launch_cluster input schema property %q is missing", name)
		}
		enum, ok := property["enum"].([]any)
		if !ok {
			t.Fatalf("launch_cluster input schema property %q has no enum", name)
		}
		got := make(map[string]bool, len(enum))
		for _, item := range enum {
			value, ok := item.(string)
			if !ok {
				t.Fatalf("launch_cluster property %q enum member %v is not a string", name, item)
			}
			got[value] = true
		}
		if !got[""] {
			t.Errorf("launch_cluster property %q enum omits the empty default value", name)
		}
		for _, value := range supported {
			if !got[value] {
				t.Errorf("launch_cluster property %q enum omits supported value %q", name, value)
			}
		}
	}
	assertEnum("platform", manager.SupportedPlatforms)
	assertEnum("architecture", manager.SupportedArchitectures)

	parameters, ok := properties["parameters"].(map[string]any)
	if !ok {
		t.Fatal("launch_cluster parameters schema is missing")
	}
	propertyNames, ok := parameters["propertyNames"].(map[string]any)
	if !ok {
		t.Fatal("launch_cluster parameters schema has no propertyNames constraint")
	}
	enum, ok := propertyNames["enum"].([]any)
	if !ok {
		t.Fatal("launch_cluster parameters propertyNames has no enum")
	}
	gotParameters := make(map[string]bool, len(enum))
	for _, item := range enum {
		value, ok := item.(string)
		if !ok {
			t.Fatalf("launch_cluster parameter name %v is not a string", item)
		}
		gotParameters[value] = true
	}
	if gotParameters["test"] {
		t.Error("launch_cluster parameters schema permits the reserved test option")
	}
	for _, value := range manager.SupportedParameters {
		if value != "test" && !gotParameters[value] {
			t.Errorf("launch_cluster parameters schema omits supported option %q", value)
		}
	}
}

func decodeStructured(t *testing.T, structured any, destination any) {
	t.Helper()
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("Marshal(StructuredContent) error = %v", err)
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		t.Fatalf("Unmarshal(StructuredContent) error = %v: %s", err, string(encoded))
	}
}

func decodeSchema(t *testing.T, schema any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("Marshal(schema) error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal(schema) error = %v", err)
	}
	return decoded
}

func resultText(result *mcp.CallToolResult) string {
	if result == nil {
		return "<nil>"
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func mcpHTTPClient(token, origin string) *http.Client {
	return &http.Client{Transport: headerTransport{
		base:   http.DefaultTransport,
		token:  token,
		origin: origin,
	}}
}

type headerTransport struct {
	base   http.RoundTripper
	token  string
	origin string
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	if t.token != "" {
		copy.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.origin != "" {
		copy.Header.Set("Origin", t.origin)
	}
	return t.base.RoundTrip(copy)
}

type fakeClusterManager struct {
	mu            sync.Mutex
	submitCalls   int
	acceptedCalls int
	replayed      bool
	submitErr     error
	toolErrors    map[string]error
	lastRequest   manager.ClusterLaunchRequest
	lastDM        string
	launchResult  manager.LaunchResult
	status        manager.ClusterSummary
	credentials   manager.ClusterCredentials
	clusters      []manager.ClusterSummary
	termination   manager.TerminationResult
}

func (m *fakeClusterManager) SubmitCluster(ctx context.Context, request manager.ClusterLaunchRequest) (manager.LaunchResult, error) {
	m.mu.Lock()
	m.submitCalls++
	m.lastRequest = request
	replayed := m.replayed
	submitErr := m.submitErr
	if toolErr := m.toolErrors["launch_cluster"]; toolErr != nil {
		submitErr = toolErr
	}
	m.mu.Unlock()
	if replayed {
		result := m.launchResult
		if result.Cluster.JobID == "" {
			result.Cluster = defaultSummary(request.SlackUserID, request.UserName)
		}
		result.Replayed = true
		return result, nil
	}
	if submitErr != nil {
		return manager.LaunchResult{}, submitErr
	}
	if request.ResolveDM != nil {
		channel, err := request.ResolveDM(ctx)
		if err != nil {
			return manager.LaunchResult{}, err
		}
		m.mu.Lock()
		m.lastDM = channel
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.acceptedCalls++
	result := m.launchResult
	m.mu.Unlock()
	if result.Cluster.JobID == "" {
		result.Cluster = defaultSummary(request.SlackUserID, request.UserName)
	}
	return result, nil
}

func (m *fakeClusterManager) GetClusterStatus(_ context.Context, user, _ string) (manager.ClusterSummary, error) {
	if err := m.toolErrors["get_cluster_status"]; err != nil {
		return manager.ClusterSummary{}, err
	}
	if m.status.JobID == "" {
		return defaultSummary(user, "alice"), nil
	}
	return m.status, nil
}

func (m *fakeClusterManager) GetClusterCredentials(context.Context, string, string) (manager.ClusterCredentials, error) {
	if err := m.toolErrors["get_cluster_credentials"]; err != nil {
		return manager.ClusterCredentials{}, err
	}
	if m.credentials.JobID == "" {
		return manager.ClusterCredentials{
			JobID:              "job-1",
			Kubeconfig:         "secret kubeconfig",
			ConsoleURL:         "https://console.example.test",
			APIURL:             "https://api.example.test:6443",
			ConsoleUsername:    "kubeadmin",
			ConsolePassword:    "secret password",
			AccessInstructions: "Use the kubeconfig with oc.",
		}, nil
	}
	return m.credentials, nil
}

func (m *fakeClusterManager) ListClusters(context.Context, string) ([]manager.ClusterSummary, error) {
	if err := m.toolErrors["list_clusters"]; err != nil {
		return nil, err
	}
	if m.clusters == nil {
		return []manager.ClusterSummary{defaultSummary("U123", "alice")}, nil
	}
	return m.clusters, nil
}

func (m *fakeClusterManager) DestroyCluster(context.Context, string, string) (manager.TerminationResult, error) {
	if err := m.toolErrors["destroy_cluster"]; err != nil {
		return manager.TerminationResult{}, err
	}
	if m.termination.JobID == "" {
		return manager.TerminationResult{JobID: "job-1", Status: "terminating"}, nil
	}
	return m.termination, nil
}

func defaultSummary(userID, username string) manager.ClusterSummary {
	return manager.ClusterSummary{
		JobID:        "job-1",
		SlackUserID:  userID,
		UserName:     username,
		Status:       "provisioning",
		Inputs:       [][]string{{"4.19"}},
		Platform:     "aws",
		Architecture: "amd64",
		LogsURL:      "https://prow.example.test/job-1",
	}
}

type fakeSlack struct {
	mu         sync.Mutex
	teamID     string
	user       *slack.User
	authErr    error
	userErr    error
	dmErr      error
	authCalls  int
	userCalls  int
	dmCalls    int
	openedUser string
}

func newFakeSlack() *fakeSlack {
	return &fakeSlack{
		teamID: "T123",
		user: &slack.User{
			ID:       "U123",
			TeamID:   "T123",
			Name:     "alice",
			RealName: "Alice Example",
			Profile:  slack.UserProfile{Email: "alice@redhat.com"},
		},
	}
}

func (f *fakeSlack) AuthTestContext(context.Context) (*slack.AuthTestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	if f.authErr != nil {
		return nil, f.authErr
	}
	return &slack.AuthTestResponse{TeamID: f.teamID}, nil
}

func (f *fakeSlack) GetUserInfoContext(_ context.Context, _ string) (*slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls++
	if f.userErr != nil {
		return nil, f.userErr
	}
	if f.user == nil {
		return nil, nil
	}
	user := *f.user
	return &user, nil
}

func (f *fakeSlack) OpenConversationContext(_ context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dmCalls++
	if params != nil && len(params.Users) == 1 {
		f.openedUser = params.Users[0]
	}
	if f.dmErr != nil {
		return nil, false, false, f.dmErr
	}
	return &slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "D123"}}}, false, false, nil
}
