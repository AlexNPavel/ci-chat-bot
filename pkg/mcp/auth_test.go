package mcp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAuthenticationOriginAndTokenRotation(t *testing.T) {
	var tokenMu sync.RWMutex
	token := []byte("first-token")
	getToken := func() []byte {
		tokenMu.RLock()
		defer tokenMu.RUnlock()
		return bytes.Clone(token)
	}
	handler, err := NewHandler(Config{
		Token:          getToken,
		PublicURL:      testOrigin + "/mcp",
		ClusterManager: &fakeClusterManager{},
		SlackClient:    newFakeSlack(),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	assertStatus := func(name, bearer, origin string, want int) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatalf("%s request error = %v", name, err)
		}
		defer closeResponseBody(t, response.Body, name+" response")
		if response.StatusCode != want {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s status = %d, want %d: %s", name, response.StatusCode, want, body)
		}
	}

	assertStatus("missing bearer", "", "", http.StatusUnauthorized)
	assertStatus("bad bearer", "wrong", "", http.StatusUnauthorized)
	assertStatus("unauthorized origin", "first-token", "https://attacker.example.test", http.StatusForbidden)
	assertStatus("origin with a path", "first-token", testOrigin+"/path", http.StatusForbidden)
	assertStatus("malformed origin", "first-token", "not-an-origin", http.StatusForbidden)
	assertStatus("origin with an empty port", "first-token", testOrigin+":", http.StatusForbidden)
	assertStatus("allowed origin", "first-token", testOrigin, http.StatusMethodNotAllowed)
	assertStatus("absent origin", "first-token", "", http.StatusMethodNotAllowed)

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer first-token")
	request.Header.Add("Origin", testOrigin)
	request.Header.Add("Origin", testOrigin)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("multiple-origin request error = %v", err)
	}
	closeResponseBody(t, response.Body, "multiple-origin response")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("multiple Origin headers status = %d, want 403", response.StatusCode)
	}

	tokenMu.Lock()
	token = []byte("rotated-token")
	tokenMu.Unlock()
	assertStatus("old token after rotation", "first-token", "", http.StatusUnauthorized)
	assertStatus("new token after rotation", "rotated-token", "", http.StatusMethodNotAllowed)

	tokenMu.Lock()
	token = []byte(" \n\t")
	tokenMu.Unlock()
	assertStatus("whitespace token after rotation", "rotated-token", "", http.StatusUnauthorized)
}

func TestNewHandlerRejectsMissingConfiguration(t *testing.T) {
	valid := Config{
		Token:          func() []byte { return []byte(testToken) },
		PublicURL:      testOrigin + "/mcp",
		ClusterManager: &fakeClusterManager{},
		SlackClient:    newFakeSlack(),
	}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{name: "token getter", edit: func(config *Config) { config.Token = nil }},
		{name: "empty token", edit: func(config *Config) { config.Token = func() []byte { return nil } }},
		{name: "whitespace token", edit: func(config *Config) { config.Token = func() []byte { return []byte(" \n\t") } }},
		{name: "nil manager", edit: func(config *Config) { config.ClusterManager = nil }},
		{name: "nil Slack client", edit: func(config *Config) { config.SlackClient = nil }},
		{name: "typed nil manager", edit: func(config *Config) {
			var managerFake *fakeClusterManager
			config.ClusterManager = managerFake
		}},
		{name: "typed nil Slack client", edit: func(config *Config) {
			var slackFake *fakeSlack
			config.SlackClient = slackFake
		}},
		{name: "missing public URL", edit: func(config *Config) { config.PublicURL = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.edit(&config)
			if handler, err := NewHandler(config); err == nil || handler != nil {
				t.Fatalf("NewHandler() = (%v, %v), want a configuration error", handler, err)
			}
		})
	}
}

func TestMCPRequestBodyLimit(t *testing.T) {
	handler, err := NewHandler(Config{
		Token:          func() []byte { return []byte(testToken) },
		PublicURL:      testOrigin + "/mcp",
		ClusterManager: &fakeClusterManager{},
		SlackClient:    newFakeSlack(),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	body := strings.NewReader(strings.Repeat(" ", MaxRequestBodyBytes+1))
	request, err := http.NewRequest(http.MethodPost, server.URL, body)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("oversized request error = %v", err)
	}
	defer closeResponseBody(t, response.Body, "oversized response")
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("oversized request status = %d, want 413: %s", response.StatusCode, body)
	}
}

func TestDisabledHandlerReturnsNotFound(t *testing.T) {
	server := httptest.NewServer(DisabledHandler())
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("GET disabled MCP endpoint error = %v", err)
	}
	defer closeResponseBody(t, response.Body, "disabled-endpoint response")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled endpoint status = %d, want 404", response.StatusCode)
	}
}

func closeResponseBody(t *testing.T, body io.Closer, description string) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Errorf("close %s body: %v", description, err)
	}
}

func TestMCPConfigRequiresPublicHTTPSMCPURL(t *testing.T) {
	if err := ValidateEndpointConfig("", ""); err != nil {
		t.Fatalf("disabled MCP config: %v", err)
	}
	if err := ValidateEndpointConfig(" ", ""); err == nil {
		t.Fatal("whitespace token path should be rejected")
	}
	if err := ValidateEndpointConfig("", " \t"); err == nil {
		t.Fatal("whitespace public URL should be rejected")
	}
	if err := ValidateEndpointConfig("", testOrigin+"/mcp"); err == nil {
		t.Fatal("public URL without token file should be rejected")
	}
	if err := ValidateEndpointConfig("/var/run/secrets/mcp/token", testOrigin+"/mcp"); err != nil {
		t.Fatalf("valid MCP config: %v", err)
	}
	invalid := []string{
		"http://cluster-bot.example.test/mcp",
		"https://cluster-bot.example.test/",
		"https://cluster-bot.example.test/mcp/",
		"https://user@cluster-bot.example.test/mcp",
		"https://cluster-bot.example.test/mcp?debug=true",
		"https://cluster-bot.example.test/mcp#fragment",
		"https://cluster-bot.example.test:/mcp",
		"https://cluster-bot.example.test:99999/mcp",
	}
	for _, publicURL := range invalid {
		t.Run(publicURL, func(t *testing.T) {
			if err := ValidateEndpointConfig("token-file", publicURL); err == nil {
				t.Fatalf("ValidateEndpointConfig(%q) unexpectedly succeeded", publicURL)
			}
		})
	}
}
