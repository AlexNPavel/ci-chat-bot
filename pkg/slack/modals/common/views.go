package common

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/openshift/ci-chat-bot/pkg/manager"
	"github.com/openshift/ci-chat-bot/pkg/slack/modals"
	slackClient "github.com/slack-go/slack"
	"golang.org/x/mod/semver"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
)

// ViewConfig contains configuration for generating views with different field names
type ViewConfig struct {
	// SecondaryFieldName is the display name for the secondary field (e.g., "Architecture" or "Duration")
	SecondaryFieldName string
	// SecondaryFieldKey is the key to extract secondary field value from data (e.g., modals.LaunchArchitecture or CreateDuration)
	SecondaryFieldKey string
	// DefaultSecondaryValue is the default value for the secondary field
	DefaultSecondaryValue string
	// DefaultPlatform is the default value for the platform field
	DefaultPlatform string
	// UseArchitectureFromData determines whether to use architecture from data or use default
	UseArchitectureFromData bool
	// DefaultArchitecture is used when UseArchitectureFromData is false
	DefaultArchitecture string
	// Identifier is the modal identifier for private metadata
	Identifier modals.Identifier
	// ModalTitle is the title of the modal
	ModalTitle string
}

// FetchReleases fetches accepted releases from the release controller
func FetchReleases(client *http.Client, architecture string) (map[string][]string, error) {
	url := fmt.Sprintf("https://%s.ocp.releases.ci.openshift.org/api/v1/releasestreams/accepted", architecture)
	acceptedReleases := make(map[string][]string, 0)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			klog.Errorf("Failed to close response for Resolve: %v", closeErr)
		}
	}()
	if err := json.NewDecoder(resp.Body).Decode(&acceptedReleases); err != nil {
		return nil, err
	}
	return acceptedReleases, nil
}

// SelectVersionView generates a view for selecting a version
func SelectVersionView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, httpclient *http.Client, data modals.CallbackData, config ViewConfig) slackClient.ModalViewRequest {
	if callback == nil {
		return slackClient.ModalViewRequest{}
	}

	platform := data.Input[modals.LaunchPlatform]
	secondaryFieldValue := data.Input[config.SecondaryFieldKey]
	mode := data.MultipleSelection[modals.LaunchMode]
	selectedStream := data.Input[modals.LaunchFromStream]
	selectedMajorMinor := data.Input[modals.LaunchFromMajorMinor]

	architecture := config.DefaultArchitecture
	if config.UseArchitectureFromData {
		architecture = data.Input[modals.LaunchArchitecture]
	}

	metadata := fmt.Sprintf("%s: %s; Platform: %s; %s: %s", config.SecondaryFieldName, secondaryFieldValue, platform, modals.LaunchModeContext, mode)
	releases, err := FetchReleases(httpclient, architecture)
	if err != nil {
		klog.Warningf("failed to fetch the data from release controller: %s", err)
		return modals.ErrorView("retrive valid releases from the release-controller", err)
	}
	var allTags []string
	for stream, tags := range releases {
		if stream == selectedStream {
			for _, tag := range tags {
				if strings.HasPrefix(tag, selectedMajorMinor) {
					allTags = append(allTags, tag)
				}
			}

		}
	}
	if len(allTags) > 99 {
		return SelectMinorMajor(callback, httpclient, data, config)
	}
	//sort.Strings(allTags)
	allTagsOptions := modals.BuildOptions(allTags, nil)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(config.Identifier)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select a Version", false, false),
			),
			slackClient.NewDividerBlock(),
			slackClient.NewInputBlock(
				modals.LaunchVersion,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select a version:", false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select an entry...", false, false),
					"",
					allTagsOptions...,
				),
			),
			slackClient.NewDividerBlock(),
			slackClient.NewContextBlock(
				modals.LaunchStepContext,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, metadata, false, false),
			),
		}},
	}
}

// SelectMinorMajor generates a view for selecting major.minor version
func SelectMinorMajor(callback *slackClient.InteractionCallback, httpclient *http.Client, data modals.CallbackData, config ViewConfig) slackClient.ModalViewRequest {
	if callback == nil {
		return slackClient.ModalViewRequest{}
	}

	platform := data.Input[modals.LaunchPlatform]
	secondaryFieldValue := data.Input[config.SecondaryFieldKey]
	mode := data.MultipleSelection[modals.LaunchMode]
	selectedStream := data.Input[modals.LaunchFromStream]

	architecture := config.DefaultArchitecture
	if config.UseArchitectureFromData {
		architecture = data.Input[modals.LaunchArchitecture]
	}

	metadata := fmt.Sprintf("%s: %s; Platform: %s; %s: %s; %s: %s", config.SecondaryFieldName, secondaryFieldValue, platform, modals.LaunchModeContext, mode, modals.LaunchFromStream, selectedStream)
	releases, err := FetchReleases(httpclient, architecture)
	if err != nil {
		klog.Warningf("failed to fetch the data from release controller: %s", err)
		return modals.ErrorView("retrive valid releases from the release-controller", err)
	}

	majorMinor := make(map[string]bool, 0)
	for stream, tags := range releases {
		if stream != selectedStream {
			continue
		}
		if strings.HasPrefix(stream, modals.StableReleasesPrefix) {
			for _, tag := range tags {
				splitTag := strings.Split(tag, ".")
				if len(splitTag) >= 2 {
					majorMinor[fmt.Sprintf("%s.%s", splitTag[0], splitTag[1])] = true
				}
			}

		}
	}
	var majorMinorReleases []string
	for key := range majorMinor {
		if manager.HypershiftSupportedVersions.Versions.Has(key) || platform != "hypershift-hosted" {
			majorMinorReleases = append(majorMinorReleases, key)
		}

	}
	// the x/mod/semver requires a `v` prefix for a version to be considered valid
	for index, version := range majorMinorReleases {
		majorMinorReleases[index] = "v" + version
	}
	semver.Sort(majorMinorReleases)
	for index, version := range majorMinorReleases {
		majorMinorReleases[index] = strings.TrimPrefix(version, "v")
	}
	slices.Reverse(majorMinorReleases)
	majorMinorOptions := modals.BuildOptions(majorMinorReleases, nil)
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(config.Identifier)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "There are to many results from the selected Stream. Select a Minor.Major as well", false, false),
			),
			slackClient.NewDividerBlock(),
			slackClient.NewInputBlock(
				modals.LaunchFromMajorMinor,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Specify the Major.Minor:", false, false),
				nil,
				slackClient.NewOptionsSelectBlockElement(
					slackClient.OptTypeStatic,
					slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select an entry...", false, false),
					"",
					majorMinorOptions...,
				),
			),
			slackClient.NewDividerBlock(),
			slackClient.NewContextBlock(
				modals.LaunchStepContext,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, metadata, false, false),
			),
		}},
	}
}

// PRInputView generates a view for entering PRs
func PRInputView(callback *slackClient.InteractionCallback, data modals.CallbackData, config ViewConfig) slackClient.ModalViewRequest {
	if callback == nil {
		return slackClient.ModalViewRequest{}
	}
	platform := data.Input[modals.LaunchPlatform]
	secondaryFieldValue := data.Input[config.SecondaryFieldKey]
	mode := data.MultipleSelection[modals.LaunchMode]
	launchWithVersion := false
	for _, key := range mode {
		if strings.TrimSpace(key) == modals.LaunchModeVersionKey {
			launchWithVersion = true
		}
	}
	metadata := fmt.Sprintf("%s: %s; Platform: %s;%s: %s", config.SecondaryFieldName, secondaryFieldValue, platform, modals.LaunchModeContext, mode)
	if launchWithVersion {
		version := data.Input[modals.LaunchVersion]
		if version == "" {
			version = data.Input[modals.LaunchFromLatestBuild]
		}
		if version == "" {
			version = data.Input[modals.LaunchFromCustom]
		}
		metadata = fmt.Sprintf("%s;Version: %s", metadata, version)
	}
	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(config.Identifier)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Enter A PR", false, false),
			),
			slackClient.NewInputBlock(
				modals.LaunchFromPR,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Enter one or more PRs, separated by comma:", false, false),
				nil,
				slackClient.NewPlainTextInputBlockElement(
					slackClient.NewTextBlockObject(slackClient.PlainTextType, "Enter one or more PRs...", false, false),
					"",
				),
			),
			slackClient.NewDividerBlock(),
			slackClient.NewContextBlock(
				modals.LaunchStepContext,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, metadata, false, false),
			),
		}},
	}
}

// FilterVersionView generates a view for filtering versions
func FilterVersionView(callback *slackClient.InteractionCallback, jobmanager manager.JobManager, data modals.CallbackData, httpclient *http.Client, mode sets.Set[string], noneSelected bool, config ViewConfig) slackClient.ModalViewRequest {
	if callback == nil {
		return slackClient.ModalViewRequest{}
	}
	platform := data.Input[modals.LaunchPlatform]
	secondaryFieldValue := data.Input[config.SecondaryFieldKey]

	architecture := config.DefaultArchitecture
	if config.UseArchitectureFromData {
		architecture = data.Input[modals.LaunchArchitecture]
	}

	latestBuildOptions := []*slackClient.OptionBlockObject{}
	_, nightly, _, err := jobmanager.ResolveImageOrVersion("nightly", "", architecture)
	if err == nil {
		latestBuildOptions = append(latestBuildOptions, slackClient.NewOptionBlockObject("nightly", slackClient.NewTextBlockObject(slackClient.PlainTextType, nightly, false, false), nil))
	}
	_, ci, _, err := jobmanager.ResolveImageOrVersion("ci", "", architecture)
	if err == nil {
		latestBuildOptions = append(latestBuildOptions, slackClient.NewOptionBlockObject("ci", slackClient.NewTextBlockObject(slackClient.PlainTextType, ci, false, false), nil))
	}
	releases, err := FetchReleases(httpclient, architecture)
	if err != nil {
		klog.Warningf("failed to fetch the data from release controller: %s", err)
		return modals.ErrorView("retrive valid releases from the release-controller", err)
	}
	var streams []string
	for stream := range releases {
		if platform == "hypershift-hosted" {
			for _, v := range sets.List(manager.HypershiftSupportedVersions.Versions) {
				if strings.HasPrefix(stream, v) || strings.Split(stream, "-")[1] == "dev" || strings.Split(stream, "-")[1] == "stable" {
					streams = append(streams, stream)
					break
				}
			}
		} else {
			streams = append(streams, stream)
		}

	}

	sort.Strings(streams)
	streamsOptions := modals.BuildOptions(streams, nil)
	metadata := fmt.Sprintf("%s: %s;Platform: %s;%s: %s", config.SecondaryFieldName, secondaryFieldValue, platform, modals.LaunchModeContext, strings.Join(sets.List(mode), ","))

	blocks := []slackClient.Block{
		slackClient.NewHeaderBlock(
			slackClient.NewTextBlockObject(slackClient.PlainTextType, "Version Specifications", false, false),
		),
		slackClient.NewSectionBlock(
			slackClient.NewTextBlockObject(slackClient.MarkdownType, "*Specify the _stream_ to get a list of versions to select from*", false, false),
			nil,
			nil,
		),
		slackClient.NewInputBlock(
			modals.LaunchFromStream,
			slackClient.NewTextBlockObject(slackClient.PlainTextType, "Specify the Stream:", false, false),
			nil,
			slackClient.NewOptionsSelectBlockElement(
				slackClient.OptTypeStatic,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select an entry...", false, false),
				"",
				streamsOptions...,
			),
		).WithOptional(true),
		slackClient.NewDividerBlock(),
		slackClient.NewSectionBlock(
			slackClient.NewTextBlockObject(slackClient.MarkdownType, "\n*Alternatively:*\n*Launch using the latest Nightly or CI build*", false, false),
			nil,
			nil,
		),
		slackClient.NewInputBlock(
			modals.LaunchFromLatestBuild,
			slackClient.NewTextBlockObject(slackClient.PlainTextType, "The latest build (nightly) or CI build:", false, false),
			nil,
			slackClient.NewOptionsSelectBlockElement(
				slackClient.OptTypeStatic,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select an entry...", false, false),
				"",
				latestBuildOptions...,
			),
		).WithOptional(true),
		slackClient.NewDividerBlock(),
		slackClient.NewSectionBlock(
			slackClient.NewTextBlockObject(slackClient.MarkdownType, "\n*Alternatively:*\n*Launch using a _Custom_ Pull Spec*", false, false),
			nil,
			nil,
		),
		slackClient.NewInputBlock(
			modals.LaunchFromCustom,
			slackClient.NewTextBlockObject(slackClient.PlainTextType, "Enter a Custom Pull Spec:", false, false),
			nil,
			slackClient.NewPlainTextInputBlockElement(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Enter a custom pull spec...", false, false),
				"",
			),
		).WithOptional(true),
		slackClient.NewDividerBlock(),
		slackClient.NewContextBlock(
			modals.LaunchStepContext,
			slackClient.NewTextBlockObject(slackClient.PlainTextType, metadata, false, false),
		),
	}

	if noneSelected {
		blocks = append([]slackClient.Block{slackClient.NewHeaderBlock(slackClient.NewTextBlockObject(slackClient.PlainTextType, ":warning: Error: At least one option must be selected :warning:", true, false))}, blocks...)
	}

	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(config.Identifier)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch a Cluster", false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks:          slackClient.Blocks{BlockSet: blocks},
	}
}

// SelectModeView generates the view for selecting launch mode (PR and/or version)
func SelectModeView(callback *slackClient.InteractionCallback, data modals.CallbackData, config ViewConfig) slackClient.ModalViewRequest {
	if callback == nil {
		return slackClient.ModalViewRequest{}
	}

	platform, ok := data.Input[modals.LaunchPlatform]
	if !ok {
		platform = config.DefaultPlatform
	}

	secondaryValue, ok := data.Input[config.SecondaryFieldKey]
	if !ok {
		secondaryValue = config.DefaultSecondaryValue
	}

	// Build metadata string with the order dependent on field name
	var metadata string
	if config.SecondaryFieldName == "Architecture" {
		metadata = fmt.Sprintf("Architecture: %s; Platform: %s", secondaryValue, platform)
	} else {
		metadata = fmt.Sprintf("Platform: %s; %s: %s", platform, config.SecondaryFieldName, secondaryValue)
	}

	options := modals.BuildOptions([]string{modals.LaunchModePRKey, modals.LaunchModeVersionKey}, nil)

	return slackClient.ModalViewRequest{
		Type:            slackClient.VTModal,
		PrivateMetadata: modals.CallbackDataToMetadata(data, string(config.Identifier)),
		Title:           slackClient.NewTextBlockObject(slackClient.PlainTextType, config.ModalTitle, false, false),
		Close:           slackClient.NewTextBlockObject(slackClient.PlainTextType, "Cancel", false, false),
		Submit:          slackClient.NewTextBlockObject(slackClient.PlainTextType, "Next", false, false),
		Blocks: slackClient.Blocks{BlockSet: []slackClient.Block{
			slackClient.NewHeaderBlock(
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Select the launch mode", false, false),
			),
			slackClient.NewInputBlock(
				modals.LaunchMode,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, "Launch the Cluster using:", false, false),
				nil,
				slackClient.NewCheckboxGroupsBlockElement("", options...),
			),
			slackClient.NewDividerBlock(),
			slackClient.NewContextBlock(
				modals.LaunchStepContext,
				slackClient.NewTextBlockObject(slackClient.PlainTextType, metadata, false, false),
			),
		}},
	}
}
