package steps

import (
	"net/http"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/common"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/launch"
	"github.com/slack-go/slack"
)

func RegisterFilterVersion(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client) *modals.FlowWithViewAndFollowUps {
	return common.RegisterFilterVersion(client, jobmanager, httpclient, launch.NewLaunchViewProvider())
}
