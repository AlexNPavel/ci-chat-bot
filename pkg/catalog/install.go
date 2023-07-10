// Copyright 2020 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package catalog

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	fbcutil "github.com/openshift/ci-chat-bot/pkg/catalog/fbcutil"
	operator "github.com/openshift/ci-chat-bot/pkg/catalog/operator"
	declarativeconfig "github.com/operator-framework/operator-registry/alpha/declcfg"
	registrybundle "github.com/operator-framework/operator-registry/pkg/lib/bundle"
)

func CreateContent(ctx context.Context, bundleImage string) (string, error) {
	// Load bundle labels and set label-dependent values.
	labels, bundle, err := operator.LoadBundle(ctx, bundleImage, false, false)
	if err != nil {
		return "", err
	}
	csv := bundle.CSV

	// FBC variables
	f := &fbcutil.FBCContext{
		Package: labels[registrybundle.PackageLabel],
		Refs:    []string{bundleImage},
		ChannelEntry: declarativeconfig.ChannelEntry{
			Name: csv.Name,
		},
	}

	// ignore channels for the bundle and instead use the default
	f.ChannelName = fbcutil.DefaultChannel

	// generate an fbc if an fbc specific label is found on the image or for a default index image.
	content, err := generateFBCContent(ctx, f, bundleImage)
	if err != nil {
		return "", fmt.Errorf("error generating File-Based Catalog content with bundle %q: %v", bundleImage, err)
	}

	return content, nil
}

// generateFBCContent creates a File-Based Catalog using the bundle image and context
func generateFBCContent(ctx context.Context, f *fbcutil.FBCContext, bundleImage string) (string, error) {
	log.Infof("Creating a File-Based Catalog of the bundle %q", bundleImage)
	// generate a File-Based Catalog representation of the bundle image
	bundleDeclcfg, err := f.CreateFBC(ctx)
	if err != nil {
		return "", fmt.Errorf("error creating a File-Based Catalog with image %q: %v", bundleImage, err)
	}

	declcfg := &declarativeconfig.DeclarativeConfig{
		Bundles:  []declarativeconfig.Bundle{bundleDeclcfg.Bundle},
		Packages: []declarativeconfig.Package{bundleDeclcfg.Package},
		Channels: []declarativeconfig.Channel{bundleDeclcfg.Channel},
	}

	// validate the declarative config and convert it to a string
	var content string
	if content, err = fbcutil.ValidateAndStringify(declcfg); err != nil {
		return "", fmt.Errorf("error validating and converting the declarative config object to a string format: %v", err)
	}

	log.Infof("Generated a valid File-Based Catalog")

	return content, nil
}
