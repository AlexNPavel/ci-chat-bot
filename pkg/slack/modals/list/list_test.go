package list

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
	assert.Equal(t, "List Running Clusters", view.Title.Text)
	assert.Equal(t, "Cancel", view.Close.Text)
	assert.Equal(t, "Submit", view.Submit.Text)

	// Test that private metadata contains the identifier
	var metadata modals.CallbackDataAndIdentifier
	err := json.Unmarshal([]byte(view.PrivateMetadata), &metadata)
	require.NoError(t, err)
	assert.Equal(t, identifier, metadata.Identifier)

	// Test blocks - should have header and multiple input blocks
	require.NotEmpty(t, view.Blocks.BlockSet)

	// First block should be a header
	headerBlock := view.Blocks.BlockSet[0].(*slack.HeaderBlock)
	assert.Equal(t, slack.MBTHeader, headerBlock.Type)
	assert.Contains(t, headerBlock.Text.Text, "clusters")

	// Verify filter blocks exist
	hasFilterByPlatform := false
	hasFilterByVersion := false
	hasFilterByUser := false

	for _, block := range view.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.InputBlock:
			if b.BlockID == filterByPlatform {
				hasFilterByPlatform = true
			}
			if b.BlockID == filterByVersion {
				hasFilterByVersion = true
			}
		case *slack.SectionBlock:
			if b.BlockID == filterByUser {
				hasFilterByUser = true
			}
		}
	}

	assert.True(t, hasFilterByPlatform, "Should have platform filter")
	assert.True(t, hasFilterByVersion, "Should have version filter")
	assert.True(t, hasFilterByUser, "Should have user filter")
}

func TestRegister(t *testing.T) {
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

func TestFilterConstants(t *testing.T) {
	assert.Equal(t, "list", identifier)
	assert.Equal(t, "filter_by_version", filterByVersion)
	assert.Equal(t, "filter_by_platform", filterByPlatform)
	assert.Equal(t, "filter_by_user", filterByUser)
	assert.Equal(t, "List Running Clusters", title)
}

func TestViewFilterInputsAreOptional(t *testing.T) {
	view := View()

	// Verify that filter inputs are marked as optional
	for _, block := range view.Blocks.BlockSet {
		if inputBlock, ok := block.(*slack.InputBlock); ok {
			assert.True(t, inputBlock.Optional, "Filter inputs should be optional")
		}
	}
}
