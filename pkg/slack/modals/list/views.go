package list

import (
	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	slackClient "github.com/slack-go/slack"
)

func View() slackClient.ModalViewRequest {
	platformOptions := modals.BuildOptions(manager.SupportedPlatforms, nil)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(modals.CallbackData{}, identifier),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "List Running Clusters", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Submit", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "See who is hogging all the clusters", false, false),
			),
			slackClient.NewInputBlock(
				filterByPlatform,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Filter By platform:", false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select a platform", false, false),
					"",
					platformOptions...,
				),
			).WithOptional(true),
			slackClient.NewInputBlock(
				filterByVersion,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Filter by version", false, false),
				nil,
				slackClient.NewPlainTextInputBlockElement(
					slackClient.NewTextBlockObject(slackClient.PlainTextType, "Enter a version...", false, false),
					"",
				),
			).WithOptional(true),
			&slackClient.SectionBlock{
				Type:    slackClient.MBTSection,
				BlockID: filterByUser,
				Text:    slackClient.NewTextBlockObject(slackClient.MarkdownType, "*Filter by User*", false, false),
				Accessory: &slackClient.Accessory{
					SelectElement: slackClient.NewOptionsSelectBlockElement(
						"users_select",
						slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select a User", false, false),
						"users_select-action",
					),
				},
			},
		}},
	}
}
