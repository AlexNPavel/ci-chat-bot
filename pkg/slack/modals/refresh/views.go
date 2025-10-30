package refresh

import (
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	slackClient "github.com/slack-go/slack"
)

func ResultView(msg string) slackClient.ModalViewRequest {
	return slackClient.ModalViewRequest{
		Type:  slackClient.VTModal,
		Title: slackClient.NewTextBlockObject(slackClient.PlainTextType, "Refresh the Status", false, false),
		Close: slackClient.NewTextBlockObject(slackClient.PlainTextType, "Close", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewSectionBlock(
				slackClient.NewTextBlockObject(slackClient.MarkdownType, msg, false, false),
				nil,
				nil,
			),
		}},
	}
}

func View() slackClient.ModalViewRequest {
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(modals.CallbackData{}, identifier),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Refresh the Status", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Submit", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewSectionBlock(
				slackClient.NewTextBlockObject(slackClient.MarkdownType, "If the cluster is currently marked as failed, retry fetching its credentials in case of an error", false, false),
				nil,
				nil,
			),
		}},
	}
}

func PrepareNextStepView() *slackClient.ModalViewRequest {
	return &slackClient.ModalViewRequest{
		Type:  slackClient.VTModal,
		Title: slackClient.NewTextBlockObject(slackClient.PlainTextType, "Refresh the Status", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewSectionBlock(
				slackClient.NewTextBlockObject(slackClient.MarkdownType, "Processing the next step, do not close this window...", false, false),
				nil,
				nil,
			),
		}},
	}
}
