package auth

import (
	"encoding/json"
	"testing"
	
	"github.com/openshift/ci-chat-bot/pkg/manager"

	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestView(t *testing.T) {
	view := View()

	// Test basic modal properties
	assert.Equal(t, slack.VTModal, view.Type)
	assert.Equal(t, title, view.Title.Text)
	assert.Equal(t, "Cancel", view.Close.Text)
	assert.Equal(t, "Submit", view.Submit.Text)

	// Test that private metadata contains the identifier
	var metadata modals.CallbackDataAndIdentifier
	err := json.Unmarshal([]byte(view.PrivateMetadata), &metadata)
	require.NoError(t, err)
	assert.Equal(t, identifier, metadata.Identifier)

	// Test blocks
	require.Len(t, view.Blocks.BlockSet, 1)
	block := view.Blocks.BlockSet[0].(*slack.SectionBlock)
	assert.Equal(t, slack.MBTSection, block.Type)
	assert.Equal(t, slack.MarkdownType, block.Text.Type)
	assert.Contains(t, block.Text.Text, "credentials")
}

func TestRegister(t *testing.T) {
	// Create mock client (using nil for simplicity as Register just sets up the flow)
	client := &slack.Client{}
	var jobManager manager.JobManager // Would be a mock in real scenario

	flow := Register(client, jobManager)

	// Test that registration returns the correct flow structure
	require.NotNil(t, flow)
	assert.Equal(t, modals.Identifier(identifier), flow.Identifier)
	assert.NotNil(t, flow.View)
	assert.NotNil(t, flow.FollowUps)

	// Test that view submission handler is registered
	assert.Contains(t, flow.FollowUps, slack.InteractionTypeViewSubmission)
	assert.NotNil(t, flow.FollowUps[slack.InteractionTypeViewSubmission])
}

func TestIdentifierConstant(t *testing.T) {
	// Test that the identifier constant has the expected value
	assert.Equal(t, "auth", identifier)
}

func TestTitleConstant(t *testing.T) {
	// Test that the title constant has the expected value
	assert.Equal(t, "Authentication", title)
}
