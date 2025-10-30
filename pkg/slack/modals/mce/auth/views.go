package auth

import (
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	slackClient "github.com/slack-go/slack"
)

func View() slackClient.ModalViewRequest {
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(modals.CallbackData{}, identifier),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "MCE Authentication", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Submit", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewSectionBlock(
				slackClient.NewTextBlockObject(slackClient.MarkdownType, "Click submit to retrieve the credentials for you MCE cluster", false, false),
				nil,
				nil,
			),
		}},
	}
}
