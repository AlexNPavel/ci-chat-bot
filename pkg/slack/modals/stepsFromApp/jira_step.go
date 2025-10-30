package stepsFromApp

import (
	"github.com/openshift/ci-chat-bot/pkg/slack/events/workflowSubmissionEvents"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	"github.com/slack-go/slack"
)

const Identifier modals.Identifier = "workflow_step_edit"

const (
	TicketTitle = "ticket_title"
	ticketType  = "ticket_type"
	UserDetails = "user_details"
)

func WorkflowStepEditView(callback *slack.InteractionCallback) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:            slack.VTWorkflowStep,
		PrivateMetadata: string(Identifier),
		CallbackID:      callback.CallbackID,
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			// using a multi-select does not seem to work here (the selected option is always empty in the input, for
			// some reason). Using a text-block instead, with a hint on currently supported options. If the input is not
			// supported, it is detected and handled when handling the workflow_step_execute event
			slack.NewInputBlock(
				ticketType,
				slack.NewTextBlockObject(slack.PlainTextType, "Supported ticket type (currently supported: bug, enhancement, consultation)", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			),
			slack.NewInputBlock(
				UserDetails,
				slack.NewTextBlockObject(slack.PlainTextType, "Include the user details (the one who initiates the workflow)", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			),
			slack.NewInputBlock(
				TicketTitle,
				slack.NewTextBlockObject(slack.PlainTextType, "Ticket Title (one-line summary)", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			),
			slack.NewDividerBlock(),
			slack.NewHeaderBlock(
				slack.NewTextBlockObject(slack.PlainTextType, "These are bug specific configurations:", false, false),
			),
			slack.NewInputBlock(
				workflowSubmissionEvents.AffectedComponent,
				slack.NewTextBlockObject(slack.PlainTextType, "Bugs: Affected component", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.OtherComponent,
				slack.NewTextBlockObject(slack.PlainTextType, "Bugs: Other component if not listed", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.IncorrectBehaviour,
				slack.NewTextBlockObject(slack.PlainTextType, "Bugs: Incorrect behaviour", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.ExpectedBehaviour,
				slack.NewTextBlockObject(slack.PlainTextType, "Bugs: Expected behaviour", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.Impact,
				slack.NewTextBlockObject(slack.PlainTextType, "Bugs: Impact", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.IsReproducible,
				slack.NewTextBlockObject(slack.PlainTextType, "Bugs: Is it reproducible", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewDividerBlock(),
			slack.NewHeaderBlock(
				slack.NewTextBlockObject(slack.PlainTextType, "These are consultation specific configurations:", false, false),
			),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdRequirement,
				slack.NewTextBlockObject(slack.PlainTextType, "Consultation: Requirements", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdPrevious,
				slack.NewTextBlockObject(slack.PlainTextType, "Consultation: Previous", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdAcceptanceCriteria,
				slack.NewTextBlockObject(slack.PlainTextType, "Consultation: Acceptance Criteria", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdAdditional,
				slack.NewTextBlockObject(slack.PlainTextType, "Consultation: Additional", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewDividerBlock(),
			slack.NewHeaderBlock(
				slack.NewTextBlockObject(slack.PlainTextType, "These are enhancement specific configurations:", false, false),
			),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdAsA,
				slack.NewTextBlockObject(slack.PlainTextType, "Enhancement: As as...", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdIWant,
				slack.NewTextBlockObject(slack.PlainTextType, "Enhancement: I want...", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdSoThat,
				slack.NewTextBlockObject(slack.PlainTextType, "Enhancement: So that...", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdSummary,
				slack.NewTextBlockObject(slack.PlainTextType, "Enhancement: Summary", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
			slack.NewInputBlock(
				workflowSubmissionEvents.BlockIdImplementation,
				slack.NewTextBlockObject(slack.PlainTextType, "Enhancement: Implementation", false, false),
				nil,
				slack.NewPlainTextInputBlockElement(nil, ""),
			).WithOptional(true),
		},
		},
	}
}
