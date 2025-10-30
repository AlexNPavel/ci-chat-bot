package steps

import (
	"net/http"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/common"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals/mce/create"
	"github.com/slack-go/slack"
)

func RegisterSelectVersion(client *slack.Client, jobmanager manager.JobManager, httpclient *http.Client) *modals.FlowWithViewAndFollowUps {
	return common.RegisterSelectVersion(client, jobmanager, httpclient, create.NewCreateViewProvider())
}
