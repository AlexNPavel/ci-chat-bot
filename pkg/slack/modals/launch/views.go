package launch

import (
	"fmt"
	"net/http"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/common"
	slackClient "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// launchViewConfig returns the configuration for launch views
func launchViewConfig(identifier modals.Identifier, modalTitle string) common.ViewConfig {
	return common.ViewConfig{
		SecondaryFieldName:      "Architecture",
		SecondaryFieldKey:       modals.LaunchArchitecture,
		DefaultSecondaryValue:   DefaultArchitecture,
		DefaultPlatform:         DefaultPlatform,
		UseArchitectureFromData: true,
		DefaultArchitecture:     "",
		Identifier:              identifier,
		ModalTitle:              modalTitle,
	}
}

func FirstStepView() slackClient.ModalViewRequest {
	platformOptions := modals.BuildOptions(manager.SupportedPlatforms, nil)
	architectureOptions := modals.BuildOptions(manager.SupportedArchitectures, nil)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(modals.CallbackData{}, string(IdentifierInitialView)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select the Launch Platform and Architecture", false, false),
			),
			slackClient.NewInputBlock(
				modals.LaunchPlatform,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, fmt.Sprintf("Platform (Default - %s)", DefaultPlatform), false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, DefaultPlatform, false, false),
					"",
					platformOptions...,
				),
			).WithOptional(true),
			slackClient.NewInputBlock(
				modals.LaunchArchitecture,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, fmt.Sprintf("Architecture (Default - %s)", DefaultArchitecture), false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, DefaultArchitecture, false, false),
					"",
					architectureOptions...,
				),
			).WithOptional(true),
		}},
	}
}

func ThirdStepView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, data modals.CallbackData) slackClient.ModalViewRequest {
	if callback == nil {
		return slackClient.ModalViewRequest{}
	}
	platform := data.Input[modals.LaunchPlatform]
	architecture := data.Input[modals.LaunchArchitecture]
	prs, ok := data.Input[modals.LaunchFromPR]
	if !ok {
		prs = "None"
	}
	version := modals.GetVersion(data, jobmanager)
	blacklist := sets.Set[string]{}
	for _, parameter := range manager.SupportedParameters {
		for k, envs := range manager.MultistageParameters {
			if k == parameter {
				if !envs.Platforms.Has(platform) {
					blacklist.Insert(parameter)
				}
			}

		}
	}
	options := modals.BuildOptions(manager.SupportedParameters, blacklist)
	context := fmt.Sprintf("Architecture: %s;Platform: %s;Version: %s;PR: %s", architecture, platform, version, prs)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(Identifier3rdStep)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Submit", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewInputBlock(
				modals.LaunchParameters,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select one or more parameters for your cluster:", false, false),
				nil,
				slackClient.NewOptionsMultiSelectBlockElement(
					slackClient.MultiOptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select one or more parameters...", false, false),
					"",
					options...,
				),
			).WithOptional(true),
			slackClient.NewDividerBlock(),
			slackClient.NewContextBlock(
				modals.LaunchStepContext,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, context, false, false),
			),
		}},
	}
}

func SubmissionView(msg string) slackClient.ModalViewRequest {
	return slackClient.ModalViewRequest{
		Type:  slackClient.VTModal,
		Title: slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
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

func FilterVersionView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, data modals.CallbackData, httpclient *http.Client, mode sets.Set[string], noneSelected bool) slackClient.ModalViewRequest {
	return common.FilterVersionView(callback, jobmanager, data, httpclient, mode, noneSelected, launchViewConfig(IdentifierFilterVersionView, "Launch a Cluster"))
}

func PRInputView(callback *slackClient.InteractionCallback, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.PRInputView(callback, data, launchViewConfig(IdentifierPRInputView, "Launch a Cluster"))
}

func SelectVersionView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.SelectVersionView(callback, jobmanager, httpclient, data, launchViewConfig(IdentifierSelectVersion, "Launch a Cluster"))
}

func SelectMinorMajor(callback *slackClient.InteractionCallback, httpclient *http.Client, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.SelectMinorMajor(callback, httpclient, data, launchViewConfig(IdentifierSelectMinorMajor, "Launch a Cluster"))
}

func SelectModeView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.SelectModeView(callback, data, launchViewConfig(IdentifierRegisterLaunchMode, "Launch a Cluster"))
}
