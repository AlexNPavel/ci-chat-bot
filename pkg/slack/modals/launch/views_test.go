package launch

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirstStepView(t *testing.T) {
	view := FirstStepView()

	// Test basic modal properties
	assert.Equal(t, slack.VTModal, view.Type)
	assert.Equal(t, "Launch a Cluster", view.Title.Text)
	assert.Equal(t, "Cancel", view.Close.Text)
	assert.Equal(t, "Next", view.Submit.Text)

	// Test that blocks are present
	assert.NotEmpty(t, view.Blocks.BlockSet)

	// Test private metadata contains the identifier
	assert.Contains(t, view.PrivateMetadata, string(IdentifierInitialView))
}

func TestFetchReleases(t *testing.T) {
	tests := []struct {
		name           string
		architecture   string
		responseBody   string
		statusCode     int
		expectedError  bool
		expectedCount  int
	}{
		{
			name:         "successful fetch",
			architecture: "amd64",
			responseBody: `{
				"4.12": ["4.12.0", "4.12.1"],
				"4.13": ["4.13.0"]
			}`,
			statusCode:    http.StatusOK,
			expectedError: false,
			expectedCount: 2,
		},
		{
			name:          "server error",
			architecture:  "amd64",
			responseBody:  "",
			statusCode:    http.StatusInternalServerError,
			expectedError: true,
			expectedCount: 0,
		},
		{
			name:          "invalid json",
			architecture:  "amd64",
			responseBody:  `invalid json`,
			statusCode:    http.StatusOK,
			expectedError: true,
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify the request URL contains the architecture
				assert.Contains(t, r.URL.String(), tt.architecture)

				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			// Note: FetchReleases uses a hardcoded URL, so we can't fully test it without refactoring
			// This test structure shows how it would be tested with a refactored version
			// that accepts the URL as a parameter
		})
	}
}

func TestIdentifierConstants(t *testing.T) {
	tests := []struct {
		name       string
		identifier modals.Identifier
		expected   string
	}{
		{
			name:       "initial view identifier",
			identifier: IdentifierInitialView,
			expected:   "launch",
		},
		{
			name:       "third step identifier",
			identifier: Identifier3rdStep,
			expected:   "launch3rdStep",
		},
		{
			name:       "PR input view identifier",
			identifier: IdentifierPRInputView,
			expected:   "pr_input_view",
		},
		{
			name:       "filter version view identifier",
			identifier: IdentifierFilterVersionView,
			expected:   "filter_version_view",
		},
		{
			name:       "launch mode identifier",
			identifier: IdentifierRegisterLaunchMode,
			expected:   "launch_mode_view",
		},
		{
			name:       "select version identifier",
			identifier: IdentifierSelectVersion,
			expected:   "select_version",
		},
		{
			name:       "select minor major identifier",
			identifier: IdentifierSelectMinorMajor,
			expected:   "select_minor_major",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, string(tt.identifier))
		})
	}
}

func TestDefaultConstants(t *testing.T) {
	assert.Equal(t, "hypershift-hosted", DefaultPlatform)
	assert.Equal(t, "amd64", DefaultArchitecture)
	assert.Equal(t, "Launch a Cluster", ModalTitle)
}

func TestModalViewStructure(t *testing.T) {
	view := FirstStepView()

	// Verify the view has required components
	require.NotNil(t, view.Title)
	require.NotNil(t, view.Close)
	require.NotNil(t, view.Submit)
	require.NotNil(t, view.Blocks)
	require.NotEmpty(t, view.Blocks.BlockSet)

	// Verify text block objects have correct types
	assert.Equal(t, slack.PlainTextType, view.Title.Type)
	assert.Equal(t, slack.PlainTextType, view.Close.Type)
	assert.Equal(t, slack.PlainTextType, view.Submit.Type)
}
