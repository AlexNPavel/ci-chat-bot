package manager

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	installer "github.com/openshift/installer/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

func (m *jobManager) createManagedCluster(name, platform string) (*clusterv1.ManagedCluster, error) {
	namespaceName := fmt.Sprintf("chat-bot-%s", name)

	// All managed clusters get their own namespace. The namespace is automatically deleted when the ManagedCluster within it is deleted.
	_, err := m.dpcrNamespaceClient.Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName, Labels: map[string]string{"test": "test"}}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}

	// copy credentials from main chat-bot secrets namespace
	chatBotSecretsClient := m.dpcrCoreClient.Secrets("ci-chat-bot-credentials")
	platformsCreds, err := chatBotSecretsClient.Get(context.TODO(), fmt.Sprintf("%s-credentials"), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get platform (%s) credentials: %v", platform, err)
	}
	pullSecret, err := chatBotSecretsClient.Get(context.TODO(), "mce-pull-secret", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pull secret: %v", err)
	}
	installConfig := &installer.InstallConfig{}

	return nil, nil
}
