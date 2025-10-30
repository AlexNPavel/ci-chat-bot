package modals

import (
	"testing"

	"github.com/openshift/ci-chat-bot/pkg/slack/interactions"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForView(t *testing.T) {
	identifier := Identifier("test_modal")
	view := slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: &slack.TextBlockObject{Type: slack.PlainTextType, Text: "Test Modal"},
	}

	result := ForView(identifier, view)

	require.NotNil(t, result)
	assert.Equal(t, identifier, result.Identifier)
	assert.Equal(t, view, result.View)
}

func TestFlowWithView_WithFollowUps(t *testing.T) {
	identifier := Identifier("test_modal")
	view := slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: &slack.TextBlockObject{Type: slack.PlainTextType, Text: "Test Modal"},
	}

	flowWithView := &FlowWithView{
		Identifier: identifier,
		View:       view,
	}

	mockHandler := interactions.HandlerFunc("test_handler", func(callback *slack.InteractionCallback, logger *logrus.Entry) ([]byte, error) {
		return nil, nil
	})

	followUps := map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: mockHandler,
	}

	result := flowWithView.WithFollowUps(followUps)

	require.NotNil(t, result)
	assert.Equal(t, flowWithView, result.FlowWithView)
	assert.Equal(t, followUps, result.FollowUps)
	assert.Len(t, result.FollowUps, 1)
	assert.NotNil(t, result.FollowUps[slack.InteractionTypeViewSubmission])
}

func TestFlowWithViewAndFollowUps(t *testing.T) {
	identifier := Identifier("test_modal")
	view := slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: &slack.TextBlockObject{Type: slack.PlainTextType, Text: "Test Modal"},
	}

	mockHandler := interactions.HandlerFunc("test_handler", func(callback *slack.InteractionCallback, logger *logrus.Entry) ([]byte, error) {
		return nil, nil
	})

	followUps := map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission:   mockHandler,
		slack.InteractionTypeBlockActions:     mockHandler,
		slack.InteractionTypeViewClosed:       mockHandler,
	}

	result := ForView(identifier, view).WithFollowUps(followUps)

	require.NotNil(t, result)
	assert.Equal(t, identifier, result.Identifier)
	assert.Equal(t, view, result.View)
	assert.Len(t, result.FollowUps, 3)

	// Verify all interaction types are registered
	assert.NotNil(t, result.FollowUps[slack.InteractionTypeViewSubmission])
	assert.NotNil(t, result.FollowUps[slack.InteractionTypeBlockActions])
	assert.NotNil(t, result.FollowUps[slack.InteractionTypeViewClosed])
}

func TestIdentifierType(t *testing.T) {
	tests := []struct {
		name       string
		identifier Identifier
		expected   string
	}{
		{
			name:       "simple identifier",
			identifier: Identifier("test"),
			expected:   "test",
		},
		{
			name:       "complex identifier",
			identifier: Identifier("test_modal_launch"),
			expected:   "test_modal_launch",
		},
		{
			name:       "predefined identifier",
			identifier: IdentifierJira,
			expected:   "jira",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, string(tt.identifier))
		})
	}
}

func TestFlowWithViewChaining(t *testing.T) {
	// Test the fluent API pattern
	identifier := Identifier("chained_modal")
	view := slack.ModalViewRequest{
		Type:  slack.VTModal,
		Title: &slack.TextBlockObject{Type: slack.PlainTextType, Text: "Chained Modal"},
	}

	mockHandler := interactions.HandlerFunc("handler", func(callback *slack.InteractionCallback, logger *logrus.Entry) ([]byte, error) {
		return []byte("success"), nil
	})

	followUps := map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: mockHandler,
	}

	// Test the entire chain
	result := ForView(identifier, view).WithFollowUps(followUps)

	require.NotNil(t, result)
	assert.Equal(t, identifier, result.Identifier)
	assert.Equal(t, view.Type, result.View.Type)
	assert.Equal(t, view.Title.Text, result.View.Title.Text)
	assert.NotNil(t, result.FollowUps[slack.InteractionTypeViewSubmission])
}
