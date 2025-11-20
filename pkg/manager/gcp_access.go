package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"k8s.io/klog"
)

const (
	// GCP project ID where users need IAM access
	GCPProjectID = "openshift-crt-ephemeral-access"

	// IAM role to grant to users for GCP workspace access
	GCPIAMRole = "roles/viewer"

	// Duration for which access is valid (7 days)
	GCPAccessDuration = 7 * 24 * time.Hour

	// Duration before automatic resource cleanup (48 hours)
	GCPResourceCleanupDuration = 48 * time.Hour

	// BigQuery dataset and table for audit logging
	BigQueryDataset = "ci_chat_bot"
	BigQueryTable   = "access_grants"
)

var (
	// timestampRegex extracts timestamp from IAM condition expression
	// Matches: request.time < timestamp('2026-01-19T12:34:56Z')
	timestampRegex = regexp.MustCompile(`timestamp\('([^']+)'\)`)
)

// GCPAccessManager handles adding/removing users from GCP project IAM
type GCPAccessManager struct {
	crmService *cloudresourcemanager.Service
	bqClient   *bigquery.Client
	projectID  string
	iamRole    string
	mutex      sync.RWMutex
	enabled    bool
	bqEnabled  bool
	bqDataset  string
	bqTable    string
	dryRun     bool // If true, skip IAM changes but still log to BigQuery
	// Runtime cache of active grants (email -> grant info)
	// Populated from GCP IAM policy on demand
	grantsCache map[string]*UserAccessGrant
}

// UserAccessGrant represents a user's access grant
type UserAccessGrant struct {
	Email         string    `json:"email"`
	GrantedAt     time.Time `json:"granted_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	RequestedBy   string    `json:"requested_by"`  // Slack user ID
	Justification string    `json:"justification"` // Business justification for access
}

// AccessGrantLogEntry represents a row in the BigQuery audit log table
type AccessGrantLogEntry struct {
	Timestamp     time.Time `bigquery:"timestamp"`
	Command       string    `bigquery:"command"`
	UserEmail     string    `bigquery:"user_email"`
	SlackUserID   string    `bigquery:"slack_user_id"`
	Justification string    `bigquery:"justification"`
	Resource      string    `bigquery:"resource"`
	ProjectID     string    `bigquery:"project_id"`
	ExpiresAt     time.Time `bigquery:"expires_at"`
}

// NewGCPAccessManager creates a new GCP access manager
func NewGCPAccessManager(serviceAccountJSON string, dryRun bool) (*GCPAccessManager, error) {
	manager := &GCPAccessManager{
		projectID:   GCPProjectID,
		iamRole:     GCPIAMRole,
		enabled:     false,
		bqEnabled:   false,
		bqDataset:   BigQueryDataset,
		bqTable:     BigQueryTable,
		dryRun:      dryRun,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// If service account credentials are not provided, disable the manager
	if serviceAccountJSON == "" {
		klog.Warning("GCP access manager disabled: service account credentials not provided")
		return manager, nil
	}

	ctx := context.Background()

	// Parse service account JSON
	config, err := google.JWTConfigFromJSON(
		[]byte(serviceAccountJSON),
		cloudresourcemanager.CloudPlatformScope,
		bigquery.Scope,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse service account JSON: %w", err)
	}

	// Create Cloud Resource Manager service
	crmService, err := cloudresourcemanager.NewService(ctx, option.WithHTTPClient(config.Client(ctx)))
	if err != nil {
		return nil, fmt.Errorf("failed to create cloud resource manager service: %w", err)
	}

	manager.crmService = crmService
	manager.enabled = true

	// Create BigQuery client for audit logging
	bqClient, err := bigquery.NewClient(ctx, GCPProjectID, option.WithHTTPClient(config.Client(ctx)))
	if err != nil {
		klog.Warningf("Failed to create BigQuery client: %v. Audit logging will be disabled.", err)
	} else {
		manager.bqClient = bqClient
		manager.bqEnabled = true
		klog.Info("BigQuery audit logging enabled")
	}

	if dryRun {
		klog.Warning("GCP access manager running in DRY-RUN mode - IAM changes will be skipped")
	}

	klog.Info("GCP access manager initialized successfully")
	return manager, nil
}

// IsEnabled returns whether the GCP access manager is enabled
func (m *GCPAccessManager) IsEnabled() bool {
	return m.enabled
}

// logAccessGrant logs an access grant to BigQuery for audit purposes
func (m *GCPAccessManager) logAccessGrant(ctx context.Context, email, slackUserID, justification, resource string, expiresAt time.Time) error {
	if !m.bqEnabled {
		klog.V(2).Info("BigQuery logging disabled, skipping audit log")
		return nil
	}

	entry := &AccessGrantLogEntry{
		Timestamp:     time.Now(),
		Command:       "request",
		UserEmail:     email,
		SlackUserID:   slackUserID,
		Justification: justification,
		Resource:      resource,
		ProjectID:     m.projectID,
		ExpiresAt:     expiresAt,
	}

	inserter := m.bqClient.Dataset(m.bqDataset).Table(m.bqTable).Inserter()
	if err := inserter.Put(ctx, entry); err != nil {
		return fmt.Errorf("failed to insert audit log to BigQuery: %w", err)
	}

	klog.Infof("Logged access grant to BigQuery: user=%s, resource=%s", email, resource)
	return nil
}

// GrantAccess adds a user as an IAM member of the GCP project with a time-based condition
func (m *GCPAccessManager) GrantAccess(email, requestedBy, justification, resource string) error {
	if !m.enabled {
		return fmt.Errorf("GCP access manager is not enabled. Please contact an administrator to configure GCP service account")
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	ctx := context.Background()

	// Check if user already has active access in cache
	now := time.Now()
	if grant, exists := m.grantsCache[email]; exists {
		// If already granted and not expired, return an error
		if grant.ExpiresAt.After(now) {
			return fmt.Errorf("user already has active access")
		}
	}

	// Calculate expiration time
	expiresAt := now.Add(GCPAccessDuration)

	// In dry-run mode, skip IAM policy changes
	if m.dryRun {
		klog.Infof("DRY-RUN: Would grant GCP IAM access to user %s (role: %s, expires: %s)", email, m.iamRole, expiresAt.Format(time.RFC3339))
	} else {
		// Get current IAM policy
		policy, err := m.crmService.Projects.GetIamPolicy(m.projectID, &cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to get IAM policy: %w", err)
		}

		// Add user as IAM member with a time-based condition
		member := "user:" + email

		// Create a new binding with condition for this user
		// Include business justification in the description
		condition := &cloudresourcemanager.Expr{
			Title:       "Temp Access",
			Description: fmt.Sprintf("Access expires on %s. Justification: %s", expiresAt.Format(time.RFC3339), justification),
			Expression:  fmt.Sprintf("request.time < timestamp('%s')", expiresAt.Format(time.RFC3339)),
		}

		binding := &cloudresourcemanager.Binding{
			Role:      m.iamRole,
			Members:   []string{member},
			Condition: condition,
		}

		policy.Bindings = append(policy.Bindings, binding)

		// Set the updated IAM policy
		setRequest := &cloudresourcemanager.SetIamPolicyRequest{
			Policy: policy,
		}
		_, err = m.crmService.Projects.SetIamPolicy(m.projectID, setRequest).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to set IAM policy: %w", err)
		}
	}

	// Log to BigQuery for audit purposes
	if err := m.logAccessGrant(ctx, email, requestedBy, justification, resource, expiresAt); err != nil {
		// Log the error but don't fail the grant operation
		klog.Warningf("Failed to log access grant to BigQuery: %v", err)
	}

	// Update cache
	grant := &UserAccessGrant{
		Email:         email,
		GrantedAt:     now,
		ExpiresAt:     expiresAt,
		RequestedBy:   requestedBy,
		Justification: justification,
	}
	m.grantsCache[email] = grant

	if m.dryRun {
		klog.Infof("DRY-RUN: Granted GCP IAM access to user %s, expires at %s. Justification: %s", email, grant.ExpiresAt, justification)
	} else {
		klog.Infof("Granted GCP IAM access to user %s, expires at %s. Justification: %s", email, grant.ExpiresAt, justification)
	}
	return nil
}

// RevokeAccess removes a user from the GCP project IAM
func (m *GCPAccessManager) RevokeAccess(email string) error {
	if !m.enabled {
		return fmt.Errorf("GCP access manager is not enabled")
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// In dry-run mode, skip IAM policy changes
	if m.dryRun {
		klog.Infof("DRY-RUN: Would revoke GCP IAM access for user %s", email)
		delete(m.grantsCache, email)
		return nil
	}

	ctx := context.Background()

	// Get current IAM policy
	policy, err := m.crmService.Projects.GetIamPolicy(m.projectID, &cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %w", err)
	}

	// Remove conditional bindings for this user
	member := "user:" + email
	newBindings := []*cloudresourcemanager.Binding{}
	modified := false

	for _, binding := range policy.Bindings {
		if binding.Role == m.iamRole && binding.Condition != nil && binding.Condition.Title == "Temp Access" {
			// Check if this binding contains the user
			if slices.Contains(binding.Members, member) {
				// Remove the user from this binding
				binding.Members = slices.DeleteFunc(binding.Members, func(m string) bool {
					return m == member
				})
				modified = true

				// Only keep the binding if it still has members
				if len(binding.Members) > 0 {
					newBindings = append(newBindings, binding)
				}
			} else {
				newBindings = append(newBindings, binding)
			}
		} else {
			newBindings = append(newBindings, binding)
		}
	}

	// Set the updated IAM policy if modified
	if modified {
		policy.Bindings = newBindings
		setRequest := &cloudresourcemanager.SetIamPolicyRequest{
			Policy: policy,
		}
		_, err = m.crmService.Projects.SetIamPolicy(m.projectID, setRequest).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to set IAM policy: %w", err)
		}
	}

	// Update cache
	delete(m.grantsCache, email)

	klog.Infof("Revoked GCP IAM access for user %s", email)
	return nil
}

// CleanupExpiredAccess removes users whose access has expired
func (m *GCPAccessManager) CleanupExpiredAccess() error {
	if !m.enabled {
		return nil
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	ctx := context.Background()
	now := time.Now()

	// Get current IAM policy to find expired bindings
	policy, err := m.crmService.Projects.GetIamPolicy(m.projectID, &cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %w", err)
	}

	// Find expired bindings by parsing condition timestamps
	newBindings := []*cloudresourcemanager.Binding{}
	modified := false
	expiredCount := 0

	for _, binding := range policy.Bindings {
		if binding.Role == m.iamRole && binding.Condition != nil && binding.Condition.Title == "Temp Access" {
			// Extract expiration timestamp from condition expression
			expiresAt, err := parseExpirationFromCondition(binding.Condition)
			if err != nil {
				klog.Warningf("Failed to parse expiration from condition: %v", err)
				newBindings = append(newBindings, binding)
				continue
			}

			// If binding is expired, don't include it
			if expiresAt.Before(now) {
				modified = true
				expiredCount++
				// Remove from cache for all members in this binding
				for _, member := range binding.Members {
					if email := extractEmailFromMember(member); email != "" {
						delete(m.grantsCache, email)
						klog.Infof("Removed expired GCP IAM access for user %s", email)
					}
				}
			} else {
				newBindings = append(newBindings, binding)
			}
		} else {
			newBindings = append(newBindings, binding)
		}
	}

	if expiredCount == 0 {
		return nil
	}

	klog.Infof("Found %d expired access grants to clean up", expiredCount)

	// Set the updated IAM policy if modified
	if modified {
		policy.Bindings = newBindings
		setRequest := &cloudresourcemanager.SetIamPolicyRequest{
			Policy: policy,
		}
		_, err = m.crmService.Projects.SetIamPolicy(m.projectID, setRequest).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to set IAM policy: %w", err)
		}
	}

	return nil
}

// GetUserGrant returns the grant information for a user, if it exists
// It checks the cache first, then queries GCP IAM policy if not cached
func (m *GCPAccessManager) GetUserGrant(email string) (*UserAccessGrant, error) {
	if !m.enabled {
		return nil, fmt.Errorf("GCP access manager is not enabled")
	}

	m.mutex.RLock()
	// Check cache first
	if grant, exists := m.grantsCache[email]; exists {
		m.mutex.RUnlock()
		return grant, nil
	}
	m.mutex.RUnlock()

	// Not in cache, query GCP IAM policy
	m.mutex.Lock()
	defer m.mutex.Unlock()

	ctx := context.Background()
	policy, err := m.crmService.Projects.GetIamPolicy(m.projectID, &cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get IAM policy: %w", err)
	}

	member := "user:" + email

	// Look for the user in conditional bindings
	for _, binding := range policy.Bindings {
		if binding.Role == m.iamRole && binding.Condition != nil && binding.Condition.Title == "Temp Access" {
			if slices.Contains(binding.Members, member) {
				// Parse expiration from condition
				expiresAt, err := parseExpirationFromCondition(binding.Condition)
				if err != nil {
					klog.Warningf("Failed to parse expiration for user %s: %v", email, err)
					continue
				}

				// Create grant from IAM policy data
				grant := &UserAccessGrant{
					Email:     email,
					ExpiresAt: expiresAt,
					// GrantedAt and RequestedBy are not available from IAM policy
					// We can estimate GrantedAt from ExpiresAt - duration
					GrantedAt: expiresAt.Add(-GCPAccessDuration),
				}

				// Update cache
				m.grantsCache[email] = grant
				return grant, nil
			}
		}
	}

	return nil, nil
}

// parseExpirationFromCondition extracts the expiration timestamp from an IAM condition expression
// Expression format: request.time < timestamp('2026-01-19T12:34:56Z')
func parseExpirationFromCondition(condition *cloudresourcemanager.Expr) (time.Time, error) {
	if condition == nil || condition.Expression == "" {
		return time.Time{}, fmt.Errorf("condition or expression is empty")
	}

	matches := timestampRegex.FindStringSubmatch(condition.Expression)
	if len(matches) < 2 {
		return time.Time{}, fmt.Errorf("failed to extract timestamp from expression: %s", condition.Expression)
	}

	timestamp := matches[1]
	expiresAt, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse timestamp %s: %w", timestamp, err)
	}

	return expiresAt, nil
}

// extractEmailFromMember extracts the email from a member string
// Member format: user:email@example.com
func extractEmailFromMember(member string) string {
	if len(member) > 5 && member[:5] == "user:" {
		return member[5:]
	}
	return ""
}

// isAlreadyMemberError checks if the error indicates the user is already a member
func isAlreadyMemberError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		// HTTP 409 Conflict is returned when the member already exists
		return apiErr.Code == http.StatusConflict
	}
	return false
}

// isNotFoundError checks if the error indicates the resource was not found
func isNotFoundError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusNotFound
	}
	return false
}
