package codebuddy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
)

// CodeBuddyTokenStorage persists a CodeBuddy credential. The JSON field names
// match the Meta* metadata keys so the persisted file carries everything the
// runtime executor needs to rebuild an Identity.
type CodeBuddyTokenStorage struct {
	// Type is always Provider.
	Type string `json:"type"`
	// Endpoint is the normalized CodeBuddy gateway.
	Endpoint string `json:"endpoint,omitempty"`
	// AccessToken authenticates chat requests.
	AccessToken string `json:"access_token"`
	// RefreshToken is exchanged for a new access token.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is the access token expiry in epoch milliseconds.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// Expired is the RFC3339 expiry, for generic tooling.
	Expired string `json:"expired,omitempty"`
	// Domain is the X-Domain value.
	Domain string `json:"domain,omitempty"`
	// MachineID is the stable per-credential machine identifier.
	MachineID string `json:"machine_id,omitempty"`
	// UID is the CodeBuddy account identifier.
	UID string `json:"uid,omitempty"`
	// Nickname is displayed only.
	Nickname string `json:"nickname,omitempty"`
	// EnterpriseID is sent as X-Enterprise-Id and X-Tenant-Id.
	EnterpriseID string `json:"enterprise_id,omitempty"`
	// DepartmentInfo is sent as X-Department-Info.
	DepartmentInfo string `json:"department_info,omitempty"`
	// LastRefresh is the RFC3339 time the token was last written.
	LastRefresh string `json:"last_refresh,omitempty"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	// It is not exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

// NewTokenStorage builds the credential record for a completed login.
func NewTokenStorage(token *TokenResult, account *Account, machineID, endpoint string) *CodeBuddyTokenStorage {
	endpoint = NormalizeEndpoint(endpoint)
	storage := &CodeBuddyTokenStorage{
		Type:      Provider,
		Endpoint:  endpoint,
		MachineID: strings.TrimSpace(machineID),
	}
	if token != nil {
		storage.AccessToken = token.AccessToken
		storage.RefreshToken = token.RefreshToken
		storage.ExpiresAt = token.ExpiresAtMS
		storage.Domain = strings.TrimSpace(token.Domain)
		if token.ExpiresAtMS > 0 {
			storage.Expired = time.UnixMilli(token.ExpiresAtMS).UTC().Format(time.RFC3339)
		}
	}
	if storage.Domain == "" {
		storage.Domain = EndpointAuthority(endpoint)
	}
	if account != nil {
		storage.UID = account.UID
		storage.Nickname = account.Nickname
		storage.EnterpriseID = account.EnterpriseID
		storage.DepartmentInfo = account.DepartmentInfo
	}
	storage.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	return storage
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *CodeBuddyTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serializes the CodeBuddy credential to a permission-restricted
// JSON file.
func (ts *CodeBuddyTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = Provider
	ts.Endpoint = NormalizeEndpoint(ts.Endpoint)
	if ts.Domain == "" {
		ts.Domain = EndpointAuthority(ts.Endpoint)
	}
	if errMkdirAll := os.MkdirAll(filepath.Dir(authFilePath), 0o700); errMkdirAll != nil {
		return fmt.Errorf("codebuddy token storage: create directory: %w", errMkdirAll)
	}

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("codebuddy token storage: merge metadata: %w", errMerge)
	}
	encoded, errMarshal := json.MarshalIndent(data, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("codebuddy token storage: encode token file: %w", errMarshal)
	}
	encoded = append(encoded, '\n')
	if errWrite := os.WriteFile(authFilePath, encoded, 0o600); errWrite != nil {
		return fmt.Errorf("codebuddy token storage: write token file: %w", errWrite)
	}
	return nil
}
