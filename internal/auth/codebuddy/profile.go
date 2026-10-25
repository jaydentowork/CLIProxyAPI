// Package codebuddy implements authentication and request identity helpers for
// Tencent CodeBuddy (global edition, www.codebuddy.ai).
//
// This file is the shared contract between the auth package, the SDK
// authenticator, and the runtime executor. Keep it free of network I/O.
package codebuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Provider is the canonical provider identifier used for auth records,
// executors, and the model registry.
const Provider = "codebuddy"

// Upstream defaults.
const (
	// DefaultEndpoint is the global CodeBuddy gateway.
	DefaultEndpoint = "https://www.codebuddy.ai"

	// ChatPath is the OpenAI-compatible chat completions path.
	ChatPath = "/v2/chat/completions"
	// AuthStatePath starts a browser login and returns authUrl + state.
	AuthStatePath = "/v2/plugin/auth/state"
	// AuthTokenPath is polled with ?state= until an access token is issued.
	AuthTokenPath = "/v2/plugin/auth/token"
	// AccountPath is polled with ?state= until the account contains a uid.
	AccountPath = "/v2/plugin/login/account"
	// RefreshPath exchanges X-Refresh-Token for a new token object.
	RefreshPath = "/v2/plugin/auth/token/refresh"

	// LoginPlatform is sent as ?platform= on AuthStatePath.
	LoginPlatform = "VSCode"
)

// Client identity values mirrored from the official VS Code plugin.
const (
	UserAgent      = "Mozilla/5.0 (compatible; Genie-IDE/1.0)"
	ProductCode    = "codebuddy"
	IDEType        = "vscode"
	IDEName        = "Visual Studio Code"
	IDEVersion     = "1.70.2"
	ProductVersion = "4.10.33259736"
)

// RefreshSkew is how long before expiry an access token is considered stale.
const RefreshSkew = 60 * time.Second

// Metadata keys persisted in the auth JSON file and mirrored in coreauth.Auth.Metadata.
const (
	MetaType           = "type"            // always Provider
	MetaEndpoint       = "endpoint"        // normalized base URL, no trailing slash
	MetaAccessToken    = "access_token"    // bearer token
	MetaRefreshToken   = "refresh_token"   // sent as X-Refresh-Token
	MetaExpiresAt      = "expires_at"      // epoch milliseconds (number)
	MetaExpired        = "expired"         // RFC3339 expiry, for generic tooling
	MetaDomain         = "domain"          // X-Domain value (token.domain or endpoint host)
	MetaMachineID      = "machine_id"      // stable per credential, X-Machine-Id
	MetaUID            = "uid"             // X-User-Id
	MetaNickname       = "nickname"        // display only
	MetaEnterpriseID   = "enterprise_id"   // X-Enterprise-Id and X-Tenant-Id
	MetaDepartmentInfo = "department_info" // X-Department-Info
	MetaLastRefresh    = "last_refresh"    // RFC3339
)

// Identity is everything needed to build CodeBuddy request headers.
type Identity struct {
	Endpoint       string
	AccessToken    string
	RefreshToken   string
	ExpiresAtMS    int64
	Domain         string
	MachineID      string
	UID            string
	Nickname       string
	EnterpriseID   string
	DepartmentInfo string
}

// NormalizeEndpoint trims whitespace and trailing slashes; empty input yields
// DefaultEndpoint.
func NormalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return DefaultEndpoint
	}
	return endpoint
}

// BuildURL joins the endpoint and an absolute path.
func BuildURL(endpoint, path string) string {
	return strings.TrimRight(endpoint, "/") + "/" + strings.TrimLeft(path, "/")
}

// EndpointAuthority returns the host[:port] of an endpoint, used as the
// fallback X-Domain value.
func EndpointAuthority(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// DomainOrDefault returns the X-Domain value for an identity.
func (id Identity) DomainOrDefault() string {
	if d := strings.TrimSpace(id.Domain); d != "" {
		return d
	}
	return EndpointAuthority(NormalizeEndpoint(id.Endpoint))
}

// NeedsRefresh reports whether the access token is missing or within RefreshSkew of expiry.
func (id Identity) NeedsRefresh(now time.Time) bool {
	if strings.TrimSpace(id.AccessToken) == "" {
		return true
	}
	if id.ExpiresAtMS <= 0 {
		return false
	}
	return id.ExpiresAtMS <= now.Add(RefreshSkew).UnixMilli()
}

// BaseHeaders returns the identity headers shared by chat and refresh requests.
// withAuthorization controls whether "Authorization: Bearer" is included.
func (id Identity) BaseHeaders(withAuthorization bool) map[string]string {
	h := map[string]string{
		"User-Agent":        UserAgent,
		"X-Product-Code":    ProductCode,
		"X-IDE-Type":        IDEType,
		"X-IDE-Name":        IDEName,
		"X-IDE-Version":     IDEVersion,
		"X-Product-Version": ProductVersion,
		"X-Machine-Id":      id.MachineID,
		"X-Domain":          id.DomainOrDefault(),
	}
	if id.UID != "" {
		h["X-User-Id"] = id.UID
	}
	if withAuthorization && id.AccessToken != "" {
		h["Authorization"] = "Bearer " + id.AccessToken
	}
	if id.EnterpriseID != "" {
		h["X-Enterprise-Id"] = id.EnterpriseID
		h["X-Tenant-Id"] = id.EnterpriseID
	}
	if id.DepartmentInfo != "" {
		h["X-Department-Info"] = id.DepartmentInfo
	}
	return h
}

// ConversationID derives the stable X-Conversation-ID (32 hex chars) from a
// seed, normally the first message's content. The global gateway rejects chat
// requests without the X-Conversation-* header family (HTTP 400, code 11128).
func ConversationID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:32]
}

// IdentityFromMetadata reads an Identity from auth metadata. Attributes may
// supply an endpoint override; pass nil when unavailable.
func IdentityFromMetadata(meta map[string]any, attrs map[string]string) Identity {
	id := Identity{
		Endpoint:       metaString(meta, MetaEndpoint),
		AccessToken:    metaString(meta, MetaAccessToken),
		RefreshToken:   metaString(meta, MetaRefreshToken),
		ExpiresAtMS:    metaInt64(meta, MetaExpiresAt),
		Domain:         metaString(meta, MetaDomain),
		MachineID:      metaString(meta, MetaMachineID),
		UID:            metaString(meta, MetaUID),
		Nickname:       metaString(meta, MetaNickname),
		EnterpriseID:   metaString(meta, MetaEnterpriseID),
		DepartmentInfo: metaString(meta, MetaDepartmentInfo),
	}
	if attrs != nil && id.Endpoint == "" {
		if v := strings.TrimSpace(attrs[MetaEndpoint]); v != "" {
			id.Endpoint = v
		} else if v = strings.TrimSpace(attrs["base_url"]); v != "" {
			id.Endpoint = v
		}
	}
	id.Endpoint = NormalizeEndpoint(id.Endpoint)
	if id.ExpiresAtMS <= 0 {
		if exp := metaString(meta, MetaExpired); exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				id.ExpiresAtMS = t.UnixMilli()
			}
		}
	}
	return id
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	switch v := meta[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return ""
	}
}

func metaInt64(meta map[string]any, key string) int64 {
	if meta == nil {
		return 0
	}
	switch v := meta[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}
