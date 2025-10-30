package done

import (
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	slackClient "github.com/slack-go/slack"
)

func View() slackClient.ModalViewRequest {
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(modals.CallbackData{}, identifier),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, title, false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Submit", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewSectionBlock(
				slackClient.NewTextBlockObject(slackClient.MarkdownType, "Click submit to terminate your running cluster", false, false),
				nil,
				nil,
			),
		}},
	}
}
