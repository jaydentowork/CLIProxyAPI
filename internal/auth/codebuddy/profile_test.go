package codebuddy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBaseHeaders(t *testing.T) {
	identity := Identity{
		Endpoint:       "https://www.codebuddy.ai/",
		AccessToken:    "tok-1",
		MachineID:      "machine-1",
		UID:            "u-1",
		EnterpriseID:   "ent-1",
		DepartmentInfo: "dept-1",
	}

	withAuth := identity.BaseHeaders(true)
	expected := map[string]string{
		"User-Agent":        UserAgent,
		"X-Product-Code":    ProductCode,
		"X-IDE-Type":        IDEType,
		"X-IDE-Name":        IDEName,
		"X-IDE-Version":     IDEVersion,
		"X-Product-Version": ProductVersion,
		"X-Machine-Id":      "machine-1",
		"X-Domain":          "www.codebuddy.ai",
		"X-User-Id":         "u-1",
		"X-Enterprise-Id":   "ent-1",
		"X-Tenant-Id":       "ent-1",
		"X-Department-Info": "dept-1",
		"Authorization":     "Bearer tok-1",
	}
	if len(withAuth) != len(expected) {
		t.Fatalf("BaseHeaders(true) = %v, want %d entries", withAuth, len(expected))
	}
	for key, want := range expected {
		if got := withAuth[key]; got != want {
			t.Errorf("BaseHeaders(true)[%q] = %q, want %q", key, got, want)
		}
	}

	withoutAuth := identity.BaseHeaders(false)
	if _, ok := withoutAuth["Authorization"]; ok {
		t.Errorf("BaseHeaders(false) = %v, want no Authorization", withoutAuth)
	}

	anonymous := Identity{Endpoint: DefaultEndpoint}.BaseHeaders(true)
	if _, ok := anonymous["Authorization"]; ok {
		t.Errorf("BaseHeaders(true) with no token = %v, want no Authorization", anonymous)
	}
	if got := anonymous["X-Domain"]; got != EndpointAuthority(DefaultEndpoint) {
		t.Errorf("X-Domain = %q, want the endpoint authority", got)
	}
}

func TestIdentityFromMetadataDecodedFromJSON(t *testing.T) {
	raw := []byte(`{
		"type": "codebuddy",
		"endpoint": "https://www.codebuddy.ai",
		"access_token": "tok-1",
		"refresh_token": "ref-1",
		"expires_at": 1799999999999,
		"expired": "2027-01-25T02:39:59Z",
		"domain": "www.codebuddy.ai",
		"machine_id": "machine-1",
		"uid": "u-1",
		"nickname": "Code Buddy",
		"enterprise_id": "ent-1",
		"department_info": "dept-1"
	}`)
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	identity := IdentityFromMetadata(meta, nil)
	if identity.Endpoint != DefaultEndpoint || identity.AccessToken != "tok-1" || identity.RefreshToken != "ref-1" {
		t.Errorf("identity = %+v", identity)
	}
	if identity.ExpiresAtMS != 1799999999999 {
		t.Errorf("ExpiresAtMS = %d, want the decoded float64 value", identity.ExpiresAtMS)
	}
	if identity.Domain != "www.codebuddy.ai" || identity.MachineID != "machine-1" || identity.UID != "u-1" {
		t.Errorf("identity identity fields = %+v", identity)
	}
	if identity.Nickname != "Code Buddy" || identity.EnterpriseID != "ent-1" || identity.DepartmentInfo != "dept-1" {
		t.Errorf("identity account fields = %+v", identity)
	}
}

func TestIdentityFromMetadataAttributesOverride(t *testing.T) {
	identity := IdentityFromMetadata(map[string]any{}, map[string]string{"base_url": "https://codebuddy.example/"})
	if identity.Endpoint != "https://codebuddy.example" {
		t.Errorf("Endpoint = %q, want the base_url attribute", identity.Endpoint)
	}

	meta := map[string]any{MetaEndpoint: "https://meta.example"}
	identity = IdentityFromMetadata(meta, map[string]string{MetaEndpoint: "https://attrs.example"})
	if identity.Endpoint != "https://meta.example" {
		t.Errorf("Endpoint = %q, want the metadata endpoint to win", identity.Endpoint)
	}
}

func TestIdentityFromMetadataFallsBackToExpired(t *testing.T) {
	expired := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	identity := IdentityFromMetadata(map[string]any{MetaExpired: expired.Format(time.RFC3339)}, nil)
	if identity.ExpiresAtMS != expired.UnixMilli() {
		t.Errorf("ExpiresAtMS = %d, want %d", identity.ExpiresAtMS, expired.UnixMilli())
	}
}

func TestConversationID(t *testing.T) {
	id := ConversationID("hello codebuddy")
	if len(id) != 32 {
		t.Fatalf("ConversationID() = %q, want 32 characters", id)
	}
	if id != ConversationID("hello codebuddy") {
		t.Error("ConversationID() must be deterministic")
	}
	if id == ConversationID("hello codebuddy!") {
		t.Error("ConversationID() must depend on the seed")
	}
	if strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("ConversationID() = %q, want lowercase hex", id)
	}
}
