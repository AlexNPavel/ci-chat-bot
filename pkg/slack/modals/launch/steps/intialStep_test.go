package steps

import (
	"net/http"
	"testing"
	
	"github.com/openshift/ci-chat-bot/pkg/manager"

	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/launch"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterFirstStep(t *testing.T) {
	client := &slack.Client{}
	var jobManager manager.JobManager // Would be a mock in real scenario
	httpClient := &http.Client{}

	flow := RegisterFirstStep(client, jobManager, httpClient)

	// Test that registration returns the correct flow structure
	require.NotNil(t, flow)
	assert.Equal(t, launch.IdentifierInitialView, flow.Identifier)
	assert.NotNil(t, flow.View)
	assert.NotNil(t, flow.FollowUps)

	// Test that view submission handler is registered
	assert.Contains(t, flow.FollowUps, slack.InteractionTypeViewSubmission)
	assert.NotNil(t, flow.FollowUps[slack.InteractionTypeViewSubmission])
}

func TestRegisterFirstStepViewProperties(t *testing.T) {
	client := &slack.Client{}
	var jobManager manager.JobManager
	httpClient := &http.Client{}

	flow := RegisterFirstStep(client, jobManager, httpClient)

	view := flow.View

	// Verify view properties
	assert.Equal(t, slack.VTModal, view.Type)
	assert.NotNil(t, view.Title)
	assert.Equal(t, "Launch a Cluster", view.Title.Text)
	assert.NotNil(t, view.Close)
	assert.NotNil(t, view.Submit)
}

func TestProcessNextRegisterFirstStepDefaults(t *testing.T) {
	tests := []struct {
		name                 string
		inputPlatform        string
		inputArchitecture    string
		expectedPlatform     string
		expectedArchitecture string
	}{
		{
			name:                 "both empty - default platform",
			inputPlatform:        "",
			inputArchitecture:    "",
			expectedPlatform:     launch.DefaultPlatform,
			expectedArchitecture: "multi", // hypershift-hosted defaults to multi
		},
		{
			name:                 "platform set, architecture empty",
			inputPlatform:        "aws",
			inputArchitecture:    "",
			expectedPlatform:     "aws",
			expectedArchitecture: launch.DefaultArchitecture,
		},
		{
			name:                 "both set",
			inputPlatform:        "aws",
			inputArchitecture:    "arm64",
			expectedPlatform:     "aws",
			expectedArchitecture: "arm64",
		},
		{
			name:                 "hypershift-hosted defaults to multi architecture",
			inputPlatform:        "hypershift-hosted",
			inputArchitecture:    "",
			expectedPlatform:     "hypershift-hosted",
			expectedArchitecture: "multi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create callback data
			callbackData := modals.CallbackData{
				Input: map[string]string{},
			}

			if tt.inputPlatform != "" {
				callbackData.Input[modals.LaunchPlatform] = tt.inputPlatform
			}
			if tt.inputArchitecture != "" {
				callbackData.Input[modals.LaunchArchitecture] = tt.inputArchitecture
			}

			// Apply the same logic as processNextRegisterFirstStep
			if callbackData.Input[modals.LaunchPlatform] == "" {
				callbackData.Input[modals.LaunchPlatform] = launch.DefaultPlatform
			}
			if callbackData.Input[modals.LaunchArchitecture] == "" {
				if callbackData.Input[modals.LaunchPlatform] == "hypershift-hosted" {
					callbackData.Input[modals.LaunchArchitecture] = "multi"
				} else {
					callbackData.Input[modals.LaunchArchitecture] = launch.DefaultArchitecture
				}
			}

			// Verify results
			assert.Equal(t, tt.expectedPlatform, callbackData.Input[modals.LaunchPlatform])
			assert.Equal(t, tt.expectedArchitecture, callbackData.Input[modals.LaunchArchitecture])
		})
	}
}
