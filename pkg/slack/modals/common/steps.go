package common

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/interactions"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
)

// ViewProvider defines an interface for generating views based on callback and submission data
type ViewProvider interface {
	// SelectVersionView returns the view for selecting a version
	SelectVersionView(callback *slack.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, submissionData modals.CallbackData) slack.ModalViewRequest
	// PRInputView returns the view for PR input
	PRInputView(callback *slack.InteractionCallback, submissionData modals.CallbackData) slack.ModalViewRequest
	// ThirdStepView returns the third step view (options/confirmation)
	ThirdStepView(callback *slack.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, submissionData modals.CallbackData) slack.ModalViewRequest
	// SelectMinorMajorView returns the view for selecting minor/major version
	SelectMinorMajorView(callback *slack.InteractionCallback, httpclient *http.Client, submissionData modals.CallbackData) slack.ModalViewRequest
	// FilterVersionView returns the view for filtering versions
	FilterVersionView(callback *slack.InteractionCallback, jobmanager manager.JobManager, submissionData modals.CallbackData, httpclient *http.Client, mode sets.Set[string], showError bool) slack.ModalViewRequest
	// GetModalTitle returns the title for the modal
	GetModalTitle() string
	// GetIdentifier returns the identifier for a given step
	GetIdentifier(step string) modals.Identifier
}

// RegisterSelectVersion creates a flow for the select version step
func RegisterSelectVersion(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) *modals.FlowWithViewAndFollowUps {
	return modals.ForView(provider.GetIdentifier("select_version"), provider.SelectVersionView(nil, jobmanager, httpclient, modals.CallbackData{})).WithFollowUps(map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: ProcessNextSelectVersion(client, jobmanager, httpclient, provider),
	})
}

// ProcessNextSelectVersion handles the select version submission
func ProcessNextSelectVersion(updater modals.ViewUpdater, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) interactions.Handler {
	identifier := string(provider.GetIdentifier("select_version"))
	return interactions.HandlerFunc(identifier, func(callback *slack.InteractionCallback, logger *logrus.Entry) (output []byte, err error) {
		klog.Infof("Private Metadata: %s", callback.View.PrivateMetadata)
		submissionData := modals.MergeCallbackData(callback)
		mode := submissionData.MultipleSelection[modals.LaunchMode]
		launchWithPR := false
		for _, key := range mode {
			if strings.TrimSpace(key) == modals.LaunchModePRKey {
				launchWithPR = true
			}
		}
		go func() {
			if launchWithPR {
				modals.OverwriteView(updater, provider.PRInputView(callback, submissionData), callback, logger)
			} else {
				modals.OverwriteView(updater, provider.ThirdStepView(callback, jobmanager, httpclient, submissionData), callback, logger)
			}
		}()
		return modals.SubmitPrepare(provider.GetModalTitle(), identifier, logger)
	})
}

// RegisterSelectMinorMajor creates a flow for the select minor/major step
func RegisterSelectMinorMajor(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) *modals.FlowWithViewAndFollowUps {
	return modals.ForView(provider.GetIdentifier("select_minor_major"), provider.SelectMinorMajorView(nil, httpclient, modals.CallbackData{})).WithFollowUps(map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: ProcessNextSelectMinorMajor(client, jobmanager, httpclient, provider),
	})
}

// ProcessNextSelectMinorMajor handles the select minor/major submission
func ProcessNextSelectMinorMajor(updater modals.ViewUpdater, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) interactions.Handler {
	identifier := string(provider.GetIdentifier("select_minor_major"))
	return interactions.HandlerFunc(identifier, func(callback *slack.InteractionCallback, logger *logrus.Entry) (output []byte, err error) {
		klog.Infof("Private Metadata: %s", callback.View.PrivateMetadata)
		submissionData := modals.MergeCallbackData(callback)
		go modals.OverwriteView(updater, provider.SelectVersionView(callback, jobmanager, httpclient, submissionData), callback, logger)
		return modals.SubmitPrepare(provider.GetModalTitle(), identifier, logger)
	})
}

// RegisterPRInput creates a flow for the PR input step
func RegisterPRInput(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) *modals.FlowWithViewAndFollowUps {
	return modals.ForView(provider.GetIdentifier("pr_input"), provider.PRInputView(nil, modals.CallbackData{})).WithFollowUps(map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: ProcessNextPRInput(client, jobmanager, httpclient, provider),
	})
}

// ProcessNextPRInput handles the PR input submission
func ProcessNextPRInput(updater modals.ViewUpdater, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) interactions.Handler {
	identifier := string(provider.GetIdentifier("pr_input"))
	return interactions.HandlerFunc(identifier, func(callback *slack.InteractionCallback, logger *logrus.Entry) (output []byte, err error) {
		klog.Infof("Private Metadata: %s", callback.View.PrivateMetadata)
		submissionData := modals.MergeCallbackData(callback)
		errorsResponse := ValidatePRInputView(submissionData, jobmanager)
		if errorsResponse != nil {
			return errorsResponse, nil
		}
		go modals.OverwriteView(updater, provider.ThirdStepView(callback, jobmanager, httpclient, submissionData), callback, logger)
		return modals.SubmitPrepare(provider.GetModalTitle(), identifier, logger)
	})
}

// ValidatePRInputView validates the PR input
func ValidatePRInputView(submissionData modals.CallbackData, jobmanager manager.JobManager) []byte {
	prs, ok := submissionData.Input[modals.LaunchFromPR]
	if !ok {
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error)

	prSlice := strings.Split(prs, ",")
	for _, pr := range prSlice {
		wg.Add(1)
		go func(pr string) {
			defer wg.Done()
			prParts, err := jobmanager.ResolveAsPullRequest(pr)
			if prParts == nil {
				errCh <- fmt.Errorf("invalid PR(s)")
			}
			if err != nil {
				errCh <- err
			}
		}(pr)
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	errors := make(map[string]string)
	var prErrors []string

	for err := range errCh {
		prErrors = append(prErrors, err.Error())
	}

	if len(prErrors) == 0 {
		return nil
	}

	errors[modals.LaunchFromPR] = strings.Join(prErrors, "; ")
	response, err := modals.ValidationError(errors)
	if err != nil {
		klog.Warningf("failed to build validation error: %v", err)
		return nil
	}

	return response
}

// RegisterFilterVersion creates a flow for the filter version step
func RegisterFilterVersion(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) *modals.FlowWithViewAndFollowUps {
	return modals.ForView(provider.GetIdentifier("filter_version"), provider.FilterVersionView(nil, nil, modals.CallbackData{}, nil, nil, false)).WithFollowUps(map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: ProcessNextFilterVersion(client, jobmanager, httpclient, provider),
	})
}

// ProcessNextFilterVersion handles the filter version submission
func ProcessNextFilterVersion(updater modals.ViewUpdater, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) interactions.Handler {
	identifier := string(provider.GetIdentifier("filter_version"))
	return interactions.HandlerFunc(identifier, func(callback *slack.InteractionCallback, logger *logrus.Entry) (output []byte, err error) {
		klog.Infof("Private Metadata: %s", callback.View.PrivateMetadata)
		submissionData := modals.MergeCallbackData(callback)
		errorResponse := ValidateFilterVersion(submissionData)
		if errorResponse != nil {
			return errorResponse, nil
		}
		nightlyOrCi := submissionData.Input[modals.LaunchFromLatestBuild]
		customBuild := submissionData.Input[modals.LaunchFromCustom]
		stream := submissionData.Input[modals.LaunchFromStream]
		mode := submissionData.MultipleSelection[modals.LaunchMode]
		launchWithPr := false
		for _, key := range mode {
			if strings.TrimSpace(key) == modals.LaunchModePRKey {
				launchWithPr = true
			}
		}
		go func() {
			if (nightlyOrCi == "") && customBuild == "" && !launchWithPr && stream == "" {
				modals.OverwriteView(updater, provider.FilterVersionView(callback, jobmanager, submissionData, httpclient, sets.New(mode...), true), callback, logger)
			} else if (nightlyOrCi != "" || customBuild != "") && launchWithPr {
				modals.OverwriteView(updater, provider.PRInputView(callback, submissionData), callback, logger)
			} else if (nightlyOrCi != "" || customBuild != "") && !launchWithPr {
				modals.OverwriteView(updater, provider.ThirdStepView(callback, jobmanager, httpclient, submissionData), callback, logger)
			} else {
				modals.OverwriteView(updater, provider.SelectVersionView(callback, jobmanager, httpclient, submissionData), callback, logger)
			}

		}()
		return modals.SubmitPrepare(provider.GetModalTitle(), string(provider.GetIdentifier("select_version")), logger)
	})
}

// checkVariables returns true if at most one variable is non-empty
func checkVariables(vars ...string) bool {
	count := 0
	for _, v := range vars {
		if v != "" {
			count++
		}
	}
	return count <= 1
}

// ValidateFilterVersion validates the filter version input
func ValidateFilterVersion(submissionData modals.CallbackData) []byte {
	errs := make(map[string]string, 0)
	nightlyOrCi := submissionData.Input[modals.LaunchFromLatestBuild]
	if nightlyOrCi != "" {
		errs[modals.LaunchFromLatestBuild] = "Select only one parameter!"
	}
	customBuild := submissionData.Input[modals.LaunchFromCustom]
	if customBuild != "" {
		errs[modals.LaunchFromCustom] = "Select only one parameter!"
	}
	selectedStream := submissionData.Input[modals.LaunchFromStream]
	if selectedStream != "" {
		errs[modals.LaunchFromStream] = "Select only one parameter!"
	}
	if !checkVariables(nightlyOrCi, customBuild, selectedStream) {
		response, err := modals.ValidationError(errs)
		if err == nil {
			return response
		}
	}
	return nil
}

// RegisterLaunchModeStep creates a flow for the launch mode selection step
func RegisterLaunchModeStep(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) *modals.FlowWithViewAndFollowUps {
	identifier := provider.GetIdentifier("mode")
	return modals.ForView(identifier, provider.ThirdStepView(nil, jobmanager, httpclient, modals.CallbackData{})).WithFollowUps(map[slack.InteractionType]interactions.Handler{
		slack.InteractionTypeViewSubmission: ProcessNextLaunchModeStep(client, jobmanager, httpclient, provider),
	})
}

// ProcessNextLaunchModeStep handles the launch mode step submission
func ProcessNextLaunchModeStep(updater modals.ViewUpdater, jobmanager manager.JobManager, httpclient *http.Client, provider ViewProvider) interactions.Handler {
	identifier := provider.GetIdentifier("mode")
	return interactions.HandlerFunc(string(identifier), func(callback *slack.InteractionCallback, logger *logrus.Entry) (output []byte, err error) {
		submissionData := modals.MergeCallbackData(callback)
		mode := sets.New[string]()
		for _, selection := range submissionData.MultipleSelection[modals.LaunchMode] {
			switch selection {
			case modals.LaunchModePRKey:
				mode.Insert(modals.LaunchModePR)
			case modals.LaunchModeVersionKey:
				mode.Insert(modals.LaunchModeVersion)
			}
		}
		go func() {
			if mode.Has(modals.LaunchModeVersion) {
				modals.OverwriteView(updater, provider.FilterVersionView(callback, jobmanager, submissionData, httpclient, mode, false), callback, logger)
			} else {
				modals.OverwriteView(updater, provider.PRInputView(callback, submissionData), callback, logger)
			}
		}()
		return modals.SubmitPrepare(provider.GetModalTitle(), string(identifier), logger)
	})
}
