package create

import (
	"fmt"
	"net/http"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/common"
	slackClient "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// mceViewConfig returns the configuration for MCE create views
func mceViewConfig(identifier modals.Identifier, modalTitle string) common.ViewConfig {
	return common.ViewConfig{
		SecondaryFieldName:      "Duration",
		SecondaryFieldKey:       CreateDuration,
		DefaultSecondaryValue:   defaultDuration,
		DefaultPlatform:         defaultPlatform,
		UseArchitectureFromData: false,
		DefaultArchitecture:     "amd64",
		Identifier:              identifier,
		ModalTitle:              modalTitle,
	}
}

func FirstStepView() slackClient.ModalViewRequest {
	platformOptions := modals.BuildOptions(manager.MCEPlatforms.UnsortedList(), nil)
	durations := []string{}
	for i := 2; i <= int(manager.MaxMCEDuration/time.Hour); i++ {
		durations = append(durations, fmt.Sprintf("%dh", i))
	}
	architectureOptions := modals.BuildOptions(durations, nil)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(modals.CallbackData{}, string(IdentifierInitialView)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch an MCE Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select the Launch Platform and Duration", false, false),
			),
			slackClient.NewInputBlock(
				modals.LaunchPlatform,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, fmt.Sprintf("Platform (Default - %s)", defaultPlatform), false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, defaultPlatform, false, false),
					"",
					platformOptions...,
				),
			).WithOptional(true),
			slackClient.NewInputBlock(
				CreateDuration,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, fmt.Sprintf("Duration (Default - %s)", defaultDuration), false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, defaultDuration, false, false),
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
	duration := data.Input[CreateDuration]
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
	context := fmt.Sprintf("Duration: %s;Platform: %s;Version: %s;PR: %s", duration, platform, version, prs)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(Identifier3rdStep)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Submit", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewContextBlock(
				modals.LaunchStepContext,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, context, false, false),
			),
		}},
	}
}

func FilterVersionView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, data modals.CallbackData, httpclient *http.Client, mode sets.Set[string], noneSelected bool) slackClient.ModalViewRequest {
	return common.FilterVersionView(callback, jobmanager, data, httpclient, mode, noneSelected, mceViewConfig(IdentifierFilterVersionView, "Launch a Cluster"))
}

func PRInputView(callback *slackClient.InteractionCallback, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.PRInputView(callback, data, mceViewConfig(IdentifierPRInputView, "Launch a Cluster"))
}

func SelectVersionView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.SelectVersionView(callback, jobmanager, httpclient, data, mceViewConfig(IdentifierSelectVersion, "Launch a Cluster"))
}

func SelectMinorMajor(callback *slackClient.InteractionCallback, httpclient *http.Client, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.SelectMinorMajor(callback, httpclient, data, mceViewConfig(IdentifierSelectMinorMajor, "Launch a Cluster"))
}

func SelectModeView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, data modals.CallbackData) slackClient.ModalViewRequest {
	return common.SelectModeView(callback, data, mceViewConfig(IdentifierSelectModeView, ModalTitle))
}
