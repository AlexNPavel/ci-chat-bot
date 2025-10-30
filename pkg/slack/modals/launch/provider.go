package launch

import (
	"net/http"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// LaunchViewProvider implements the common.ViewProvider interface for the launch modal
type LaunchViewProvider struct{}

// NewLaunchViewProvider creates a new LaunchViewProvider
func NewLaunchViewProvider() *LaunchViewProvider {
	return &LaunchViewProvider{}
}

// SelectVersionView returns the view for selecting a version
func (p *LaunchViewProvider) SelectVersionView(callback *slack.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, submissionData modals.CallbackData) slack.ModalViewRequest {
	return SelectVersionView(callback, jobmanager, httpclient, submissionData)
}

// PRInputView returns the view for PR input
func (p *LaunchViewProvider) PRInputView(callback *slack.InteractionCallback, submissionData modals.CallbackData) slack.ModalViewRequest {
	return PRInputView(callback, submissionData)
}

// ThirdStepView returns the third step view (options/confirmation)
func (p *LaunchViewProvider) ThirdStepView(callback *slack.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, submissionData modals.CallbackData) slack.ModalViewRequest {
	return ThirdStepView(callback, jobmanager, httpclient, submissionData)
}

// SelectMinorMajorView returns the view for selecting minor/major version
func (p *LaunchViewProvider) SelectMinorMajorView(callback *slack.InteractionCallback, httpclient *http.Client, submissionData modals.CallbackData) slack.ModalViewRequest {
	return SelectMinorMajor(callback, httpclient, submissionData)
}

// FilterVersionView returns the view for filtering versions
func (p *LaunchViewProvider) FilterVersionView(callback *slack.InteractionCallback, jobmanager manager.JobManager, submissionData modals.CallbackData, httpclient *http.Client, mode sets.Set[string], showError bool) slack.ModalViewRequest {
	return FilterVersionView(callback, jobmanager, submissionData, httpclient, mode, showError)
}

// GetModalTitle returns the title for the modal
func (p *LaunchViewProvider) GetModalTitle() string {
	return ModalTitle
}

// GetIdentifier returns the identifier for a given step
func (p *LaunchViewProvider) GetIdentifier(step string) modals.Identifier {
	switch step {
	case "select_version":
		return IdentifierSelectVersion
	case "select_minor_major":
		return IdentifierSelectMinorMajor
	case "pr_input":
		return IdentifierPRInputView
	case "filter_version":
		return IdentifierFilterVersionView
	case "mode":
		return IdentifierRegisterLaunchMode
	default:
		return IdentifierInitialView
	}
}
