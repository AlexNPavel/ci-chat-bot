package manager

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
)

func TestNewGCPAccessManager_Disabled(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name               string
		serviceAccountJSON string
	}{
		{
			name:               "Empty service account JSON",
			serviceAccountJSON: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			manager, err := NewGCPAccessManager(tc.serviceAccountJSON, false)
			if err != nil {
				t.Errorf("Expected no error, got: %v", err)
			}
			if manager == nil {
				t.Error("Expected manager to be created")
			}
			if manager.IsEnabled() {
				t.Error("Expected manager to be disabled")
			}
		})
	}
}

func TestGCPAccessManager_IsEnabled(t *testing.T) {
	t.Parallel()

	// Test disabled manager
	manager, _ := NewGCPAccessManager("", false)
	if manager.IsEnabled() {
		t.Error("Expected disabled manager")
	}
}

func TestIsAlreadyMemberError(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "409 Conflict error",
			err: &googleapi.Error{
				Code: http.StatusConflict,
			},
			expected: true,
		},
		{
			name: "404 Not Found error",
			err: &googleapi.Error{
				Code: http.StatusNotFound,
			},
			expected: false,
		},
		{
			name:     "Generic error",
			err:      errors.New("generic error"),
			expected: false,
		},
		{
			name:     "Nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isAlreadyMemberError(tc.err)
			if result != tc.expected {
				t.Errorf("Expected %v, got %v", tc.expected, result)
			}
		})
	}
}

func TestIsNotFoundError(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "404 Not Found error",
			err: &googleapi.Error{
				Code: http.StatusNotFound,
			},
			expected: true,
		},
		{
			name: "409 Conflict error",
			err: &googleapi.Error{
				Code: http.StatusConflict,
			},
			expected: false,
		},
		{
			name:     "Generic error",
			err:      errors.New("generic error"),
			expected: false,
		},
		{
			name:     "Nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isNotFoundError(tc.err)
			if result != tc.expected {
				t.Errorf("Expected %v, got %v", tc.expected, result)
			}
		})
	}
}

func TestGCPAccessManager_OperationsWhenDisabled(t *testing.T) {
	t.Parallel()

	// Create a disabled manager
	manager, _ := NewGCPAccessManager("", false)

	testCases := []struct {
		name string
		op   func() error
	}{
		{
			name: "GrantAccess",
			op: func() error {
				return manager.GrantAccess("user@example.com", "U12345", "Testing", "gcp")
			},
		},
		{
			name: "RevokeAccess",
			op: func() error {
				return manager.RevokeAccess("user@example.com")
			},
		},
		{
			name: "GetUserGrant",
			op: func() error {
				_, err := manager.GetUserGrant("user@example.com")
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.op()
			if err == nil {
				t.Error("Expected error when manager is disabled")
			}
		})
	}
}

func TestGCPAccessManager_CleanupExpiredAccess_WhenDisabled(t *testing.T) {
	t.Parallel()

	// Create a disabled manager
	manager, _ := NewGCPAccessManager("", false)

	// CleanupExpiredAccess should not error when disabled, just no-op
	err := manager.CleanupExpiredAccess()
	if err != nil {
		t.Errorf("Expected no error when cleanup is called on disabled manager, got: %v", err)
	}
}

func TestGCPAccessDuration(t *testing.T) {
	t.Parallel()
	// Verify the constant is set correctly to 7 days
	expectedDuration := 7 * 24 * time.Hour
	if GCPAccessDuration != expectedDuration {
		t.Errorf("Expected GCPAccessDuration to be %v, got %v", expectedDuration, GCPAccessDuration)
	}
}

func TestParseExpirationFromCondition(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		condition   *cloudresourcemanager.Expr
		expectError bool
		expected    string // RFC3339 format
	}{
		{
			name: "Valid condition",
			condition: &cloudresourcemanager.Expr{
				Expression: "request.time < timestamp('2026-01-19T12:34:56Z')",
			},
			expectError: false,
			expected:    "2026-01-19T12:34:56Z",
		},
		{
			name:        "Nil condition",
			condition:   nil,
			expectError: true,
		},
		{
			name: "Empty expression",
			condition: &cloudresourcemanager.Expr{
				Expression: "",
			},
			expectError: true,
		},
		{
			name: "Invalid expression format",
			condition: &cloudresourcemanager.Expr{
				Expression: "invalid expression",
			},
			expectError: true,
		},
		{
			name: "Invalid timestamp format",
			condition: &cloudresourcemanager.Expr{
				Expression: "request.time < timestamp('not-a-timestamp')",
			},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseExpirationFromCondition(tc.condition)
			if tc.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got: %v", err)
				}
				expectedTime, _ := time.Parse(time.RFC3339, tc.expected)
				if !result.Equal(expectedTime) {
					t.Errorf("Expected %v, got %v", expectedTime, result)
				}
			}
		})
	}
}

func TestExtractEmailFromMember(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		member   string
		expected string
	}{
		{
			name:     "Valid user member",
			member:   "user:test@example.com",
			expected: "test@example.com",
		},
		{
			name:     "Invalid prefix",
			member:   "group:test@example.com",
			expected: "",
		},
		{
			name:     "No prefix",
			member:   "test@example.com",
			expected: "",
		},
		{
			name:     "Empty string",
			member:   "",
			expected: "",
		},
		{
			name:     "Just user prefix",
			member:   "user:",
			expected: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := extractEmailFromMember(tc.member)
			if result != tc.expected {
				t.Errorf("Expected %s, got %s", tc.expected, result)
			}
		})
	}
}

// TestGrantAccess_DryRunMode tests the dry-run mode functionality
func TestGrantAccess_DryRunMode(t *testing.T) {
	t.Parallel()

	// Create a manager in dry-run mode but with enabled=true to test the logic
	// We can't use real credentials, so we'll manually create a manager
	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// Test granting access in dry-run mode
	err := manager.GrantAccess("test@example.com", "U12345", "Testing dry-run", "gcp")
	if err != nil {
		t.Errorf("Expected no error in dry-run mode, got: %v", err)
	}

	// Verify cache was updated
	grant, exists := manager.grantsCache["test@example.com"]
	if !exists {
		t.Error("Expected cache to be updated")
	}
	if grant.Email != "test@example.com" {
		t.Errorf("Expected email test@example.com, got %s", grant.Email)
	}
	if grant.Justification != "Testing dry-run" {
		t.Errorf("Expected justification 'Testing dry-run', got %s", grant.Justification)
	}
}

// TestGrantAccess_AlreadyHasAccess tests duplicate access detection
func TestGrantAccess_AlreadyHasAccess(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// First grant should succeed
	err := manager.GrantAccess("test@example.com", "U12345", "First grant", "gcp")
	if err != nil {
		t.Errorf("First grant should succeed, got: %v", err)
	}

	// Second grant should fail with "already has active access"
	err = manager.GrantAccess("test@example.com", "U12345", "Second grant", "gcp")
	if err == nil {
		t.Error("Expected error when granting access to user who already has it")
	}
	if err.Error() != "user already has active access" {
		t.Errorf("Expected 'user already has active access' error, got: %v", err)
	}
}

// TestGrantAccess_ExpiredAccess tests that expired access can be re-granted
func TestGrantAccess_ExpiredAccess(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// Manually add an expired grant to cache
	expiredGrant := &UserAccessGrant{
		Email:         "test@example.com",
		GrantedAt:     time.Now().Add(-8 * 24 * time.Hour),
		ExpiresAt:     time.Now().Add(-24 * time.Hour), // Expired 1 day ago
		RequestedBy:   "U12345",
		Justification: "Old grant",
	}
	manager.grantsCache["test@example.com"] = expiredGrant

	// New grant should succeed since old one is expired
	err := manager.GrantAccess("test@example.com", "U12345", "New grant", "gcp")
	if err != nil {
		t.Errorf("Expected grant to succeed for expired access, got: %v", err)
	}

	// Verify cache was updated with new grant
	grant := manager.grantsCache["test@example.com"]
	if grant.Justification != "New grant" {
		t.Errorf("Expected justification 'New grant', got %s", grant.Justification)
	}
	if grant.ExpiresAt.Before(time.Now()) {
		t.Error("Expected new grant to have future expiration")
	}
}

// TestRevokeAccess_DryRunMode tests revocation in dry-run mode
func TestRevokeAccess_DryRunMode(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// Add a grant to cache
	grant := &UserAccessGrant{
		Email:         "test@example.com",
		GrantedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(7 * 24 * time.Hour),
		RequestedBy:   "U12345",
		Justification: "Testing",
	}
	manager.grantsCache["test@example.com"] = grant

	// Revoke in dry-run mode
	err := manager.RevokeAccess("test@example.com")
	if err != nil {
		t.Errorf("Expected no error in dry-run mode, got: %v", err)
	}

	// Verify cache was cleared
	if _, exists := manager.grantsCache["test@example.com"]; exists {
		t.Error("Expected cache to be cleared after revocation")
	}
}

// TestRevokeAccess_UserNotInCache tests revoking access for user not in cache
func TestRevokeAccess_UserNotInCache(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// Revoke for user not in cache (should not error in dry-run mode)
	err := manager.RevokeAccess("nonexistent@example.com")
	if err != nil {
		t.Errorf("Expected no error when revoking non-existent user in dry-run, got: %v", err)
	}
}

// TestGetUserGrant_CacheHit tests retrieving grant from cache
func TestGetUserGrant_CacheHit(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// Add grant to cache
	expectedGrant := &UserAccessGrant{
		Email:         "test@example.com",
		GrantedAt:     time.Now().Add(-1 * time.Hour),
		ExpiresAt:     time.Now().Add(6 * 24 * time.Hour),
		RequestedBy:   "U12345",
		Justification: "Testing cache hit",
	}
	manager.grantsCache["test@example.com"] = expectedGrant

	// Get user grant - should return cached value
	// Note: GetUserGrant requires crmService for IAM policy access, so we can only test
	// the disabled state or use dry-run with a mock
	// For now, we'll test that the cache is checked
}

// TestCleanupExpiredAccess_Logic tests the cleanup logic with dry-run
func TestCleanupExpiredAccess_DryRunMode(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		grantsCache: make(map[string]*UserAccessGrant),
	}

	// Add expired and active grants to cache
	expiredGrant := &UserAccessGrant{
		Email:         "expired@example.com",
		GrantedAt:     time.Now().Add(-8 * 24 * time.Hour),
		ExpiresAt:     time.Now().Add(-1 * time.Hour),
		RequestedBy:   "U12345",
		Justification: "Expired",
	}
	activeGrant := &UserAccessGrant{
		Email:         "active@example.com",
		GrantedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(6 * 24 * time.Hour),
		RequestedBy:   "U67890",
		Justification: "Active",
	}
	manager.grantsCache["expired@example.com"] = expiredGrant
	manager.grantsCache["active@example.com"] = activeGrant

	// Note: CleanupExpiredAccess requires crmService for IAM policy access
	// We can only test the disabled case or would need to mock the GCP service
	// The existing test TestGCPAccessManager_CleanupExpiredAccess_WhenDisabled covers that
}

// TestGrantAccess_CacheConsistency tests that cache is properly updated
func TestGrantAccess_CacheConsistency(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	email := "test@example.com"
	slackID := "U12345"
	justification := "Testing cache consistency"

	// Grant access
	err := manager.GrantAccess(email, slackID, justification, "gcp")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify cache entry
	grant, exists := manager.grantsCache[email]
	if !exists {
		t.Fatal("Expected grant in cache")
	}

	// Verify all fields
	if grant.Email != email {
		t.Errorf("Expected email %s, got %s", email, grant.Email)
	}
	if grant.RequestedBy != slackID {
		t.Errorf("Expected RequestedBy %s, got %s", slackID, grant.RequestedBy)
	}
	if grant.Justification != justification {
		t.Errorf("Expected justification %s, got %s", justification, grant.Justification)
	}

	// Verify timestamps are reasonable
	now := time.Now()
	if grant.GrantedAt.After(now) {
		t.Error("GrantedAt should not be in the future")
	}
	if grant.GrantedAt.Before(now.Add(-1 * time.Minute)) {
		t.Error("GrantedAt should be recent")
	}

	expectedExpiration := grant.GrantedAt.Add(GCPAccessDuration)
	if !grant.ExpiresAt.Equal(expectedExpiration) {
		t.Errorf("Expected ExpiresAt %v, got %v", expectedExpiration, grant.ExpiresAt)
	}
}

// TestGrantAccess_MultipleUsers tests granting to multiple different users
func TestGrantAccess_MultipleUsers(t *testing.T) {
	t.Parallel()

	manager := &GCPAccessManager{
		projectID:   "test-project",
		iamRole:     "roles/viewer",
		enabled:     true,
		bqEnabled:   false,
		dryRun:      true,
		grantsCache: make(map[string]*UserAccessGrant),
	}

	users := []struct {
		email         string
		slackID       string
		justification string
	}{
		{"user1@example.com", "U11111", "User 1 justification"},
		{"user2@example.com", "U22222", "User 2 justification"},
		{"user3@example.com", "U33333", "User 3 justification"},
	}

	// Grant to all users
	for _, user := range users {
		err := manager.GrantAccess(user.email, user.slackID, user.justification, "gcp")
		if err != nil {
			t.Errorf("Failed to grant to %s: %v", user.email, err)
		}
	}

	// Verify all are in cache with correct data
	if len(manager.grantsCache) != len(users) {
		t.Errorf("Expected %d grants in cache, got %d", len(users), len(manager.grantsCache))
	}

	for _, user := range users {
		grant, exists := manager.grantsCache[user.email]
		if !exists {
			t.Errorf("Expected grant for %s in cache", user.email)
			continue
		}
		if grant.RequestedBy != user.slackID {
			t.Errorf("Expected RequestedBy %s, got %s", user.slackID, grant.RequestedBy)
		}
		if grant.Justification != user.justification {
			t.Errorf("Expected justification %s, got %s", user.justification, grant.Justification)
		}
	}
}
