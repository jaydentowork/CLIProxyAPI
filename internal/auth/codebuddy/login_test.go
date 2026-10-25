package codebuddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testLoginState = "state a+b/c=1"

func TestStartLoginFollowsLoginProtocol(t *testing.T) {
	var (
		gotMethod  string
		gotPath    string
		gotQuery   string
		gotBody    string
		gotHeaders http.Header
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHeaders = r.Header.Clone()
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"authUrl":"https://www.codebuddy.ai/auth?code=1","state":"st-1"}}}`))
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL+"/")
	authURL, state, err := client.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin() error = %v", err)
	}
	if authURL != "https://www.codebuddy.ai/auth?code=1" {
		t.Errorf("authURL = %q", authURL)
	}
	if state != "st-1" {
		t.Errorf("state = %q, want st-1", state)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != AuthStatePath {
		t.Errorf("path = %q, want %q", gotPath, AuthStatePath)
	}
	if gotQuery != "platform="+LoginPlatform {
		t.Errorf("query = %q, want platform=%s", gotQuery, LoginPlatform)
	}
	if gotBody != "{}" {
		t.Errorf("body = %q, want {}", gotBody)
	}
	if got := gotHeaders.Get("User-Agent"); got != UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, UserAgent)
	}
	if got := gotHeaders.Get("X-Product-Code"); got != ProductCode {
		t.Errorf("X-Product-Code = %q, want %q", got, ProductCode)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	authority := EndpointAuthority(server.URL)
	if got := gotHeaders.Get("X-Domain"); got != authority {
		t.Errorf("X-Domain = %q, want %q", got, authority)
	}
	for _, header := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := gotHeaders.Get(header); got != "true" {
			t.Errorf("%s = %q, want true", header, got)
		}
	}
}

func TestStartLoginRequiresAuthURLAndState(t *testing.T) {
	responses := []string{
		`{"data":{"data":{"state":"st-1"}}}`,
		`{"data":{"authUrl":"https://example.test/auth"}}`,
		`{"data":{"data":{}}}`,
	}
	for _, response := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(response))
		}))
		client := NewLoginClient(server.Client(), server.URL)
		if _, _, err := client.StartLogin(context.Background()); err == nil {
			t.Errorf("StartLogin() with %s: expected error", response)
		}
		server.Close()
	}
}

func TestStartLoginReportsUpstreamStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"upstream down"}`))
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL)
	_, _, err := client.StartLogin(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StartLogin() error = %v, want HTTPError 502", err)
	}
}

func TestWaitForLoginPollsTokenThenAccount(t *testing.T) {
	var (
		tokenCalls        atomic.Int32
		accountCalls      atomic.Int32
		accountAuth       atomic.Value
		accountDomain     atomic.Value
		accountNoAuthFlag atomic.Value
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case AuthTokenPath:
			if got := r.URL.Query().Get("state"); got != testLoginState {
				t.Errorf("token poll state = %q, want %q", got, testLoginState)
			}
			if got := r.Header.Get("X-No-Authorization"); got != "true" {
				t.Errorf("token poll X-No-Authorization = %q, want true", got)
			}
			if tokenCalls.Add(1) < 3 {
				// Pending logins answer with HTTP errors and a data envelope without a token.
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"data":{"error":"pending"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"data":{"accessToken":"tok-1","refreshToken":"ref-1","expiresIn":1800,"domain":"custom.example"}}}`))
		case AccountPath:
			accountAuth.Store(r.Header.Get("Authorization"))
			accountDomain.Store(r.Header.Get("X-Domain"))
			accountNoAuthFlag.Store(r.Header.Get("X-No-Authorization"))
			if accountCalls.Add(1) < 2 {
				_, _ = w.Write([]byte(`{"data":{"uid":"","nickname":"pending"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"uid":"u-1","nickname":"Code Buddy","enterpriseId":12345,"departmentInfo":678}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL)
	client.PollInterval = 10 * time.Millisecond
	client.Deadline = 5 * time.Second

	token, account, err := client.WaitForLogin(context.Background(), testLoginState)
	if err != nil {
		t.Fatalf("WaitForLogin() error = %v", err)
	}
	if token == nil || token.AccessToken != "tok-1" || token.RefreshToken != "ref-1" {
		t.Fatalf("token = %+v", token)
	}
	if token.ExpiresAtMS <= time.Now().UnixMilli() {
		t.Errorf("ExpiresAtMS = %d, want a future expiry", token.ExpiresAtMS)
	}
	if account == nil || account.UID != "u-1" || account.Nickname != "Code Buddy" {
		t.Fatalf("account = %+v", account)
	}
	if account.EnterpriseID != "12345" || account.DepartmentInfo != "678" {
		t.Errorf("numeric account fields = %q/%q", account.EnterpriseID, account.DepartmentInfo)
	}
	if got := accountAuth.Load(); got != "Bearer tok-1" {
		t.Errorf("account Authorization = %v, want Bearer tok-1", got)
	}
	if got := accountDomain.Load(); got != "custom.example" {
		t.Errorf("account X-Domain = %v, want custom.example", got)
	}
	if got := accountNoAuthFlag.Load(); got != "" {
		t.Errorf("account X-No-Authorization = %v, want empty", got)
	}
	if tokenCalls.Load() < 3 || accountCalls.Load() < 2 {
		t.Errorf("polls = token %d, account %d", tokenCalls.Load(), accountCalls.Load())
	}
}

func TestWaitForLoginFallsBackToEndpointDomain(t *testing.T) {
	var accountDomain atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case AuthTokenPath:
			_, _ = w.Write([]byte(`{"data":{"accessToken":"tok-1"}}`))
		case AccountPath:
			accountDomain.Store(r.Header.Get("X-Domain"))
			_, _ = w.Write([]byte(`{"data":{"uid":"u-1"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL)
	client.PollInterval = 10 * time.Millisecond
	client.Deadline = 5 * time.Second

	if _, _, err := client.WaitForLogin(context.Background(), testLoginState); err != nil {
		t.Fatalf("WaitForLogin() error = %v", err)
	}
	if got := accountDomain.Load(); got != EndpointAuthority(server.URL) {
		t.Errorf("account X-Domain = %v, want %q", got, EndpointAuthority(server.URL))
	}
}

// errorTransport fails the first failures requests to exercise poll retry on transport errors.
type errorTransport struct {
	base     http.RoundTripper
	failures int32
}

func (rt *errorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&rt.failures, -1) >= 0 {
		return nil, errors.New("temporary transport failure")
	}
	return rt.base.RoundTrip(req)
}

func TestWaitForLoginIgnoresTransportErrors(t *testing.T) {
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case AuthTokenPath:
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"data":{"accessToken":"tok-1"}}`))
		case AccountPath:
			_, _ = w.Write([]byte(`{"data":{"uid":"u-1"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	transport := &errorTransport{base: server.Client().Transport, failures: 2}
	client := NewLoginClient(&http.Client{Transport: transport}, server.URL)
	client.PollInterval = 10 * time.Millisecond
	client.Deadline = 5 * time.Second

	token, account, err := client.WaitForLogin(context.Background(), testLoginState)
	if err != nil {
		t.Fatalf("WaitForLogin() error = %v", err)
	}
	if token.AccessToken != "tok-1" || account.UID != "u-1" {
		t.Fatalf("token/account = %+v/%+v", token, account)
	}
	if tokenCalls.Load() != 1 {
		t.Errorf("token polls = %d, want 1", tokenCalls.Load())
	}
}

func TestWaitForLoginHonoursDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"status":"pending"}}`))
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL)
	client.PollInterval = 10 * time.Millisecond
	client.Deadline = 40 * time.Millisecond

	start := time.Now()
	_, _, err := client.WaitForLogin(context.Background(), testLoginState)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("WaitForLogin() error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("WaitForLogin() took %s, want an early timeout", elapsed)
	}
}

func TestWaitForLoginHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"status":"pending"}}`))
		once.Do(cancel)
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL)
	client.PollInterval = 10 * time.Millisecond
	client.Deadline = 5 * time.Second

	_, _, err := client.WaitForLogin(ctx, testLoginState)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForLogin() error = %v, want context.Canceled", err)
	}
}

func TestWaitForLoginRequiresState(t *testing.T) {
	client := NewLoginClient(nil, DefaultEndpoint)
	if _, _, err := client.WaitForLogin(context.Background(), "  "); err == nil {
		t.Fatal("WaitForLogin() with an empty state: expected error")
	}
}

func TestParseAccountRequiresUID(t *testing.T) {
	if account := ParseAccount(nil); account != nil {
		t.Fatalf("ParseAccount(nil) = %+v", account)
	}
	if account := ParseAccount(map[string]any{"nickname": "pending"}); account != nil {
		t.Fatalf("ParseAccount(pending) = %+v", account)
	}
	account := ParseAccount(map[string]any{
		"uid":            float64(42),
		"nickname":       "Nick",
		"enterpriseId":   float64(7),
		"departmentInfo": float64(9),
	})
	if account == nil || account.UID != "42" || account.EnterpriseID != "7" || account.DepartmentInfo != "9" {
		t.Fatalf("ParseAccount() = %+v", account)
	}
	if got := account.DisplayName(); got != "Nick" {
		t.Errorf("DisplayName() = %q, want Nick", got)
	}
	if got := (&Account{UID: "u-1"}).DisplayName(); got != "u-1" {
		t.Errorf("DisplayName() = %q, want u-1", got)
	}
	if got := (*Account)(nil).DisplayName(); got != "User" {
		t.Errorf("nil DisplayName() = %q, want User", got)
	}
}

func TestBuildMetadataUsesContractKeys(t *testing.T) {
	expiresAt := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	token := &TokenResult{AccessToken: "tok-1", RefreshToken: "ref-1", ExpiresAtMS: expiresAt.UnixMilli()}
	account := &Account{UID: "u-1", Nickname: "Code Buddy", EnterpriseID: "ent-1", DepartmentInfo: "dept-1"}

	meta := BuildMetadata(token, account, " machine-1 ", "https://www.codebuddy.ai/")
	if got := meta[MetaType]; got != Provider {
		t.Errorf("%s = %v, want %s", MetaType, got, Provider)
	}
	if got := meta[MetaEndpoint]; got != DefaultEndpoint {
		t.Errorf("%s = %v, want %s", MetaEndpoint, got, DefaultEndpoint)
	}
	if got := meta[MetaDomain]; got != EndpointAuthority(DefaultEndpoint) {
		t.Errorf("%s = %v, want the endpoint authority", MetaDomain, got)
	}
	if got := meta[MetaMachineID]; got != "machine-1" {
		t.Errorf("%s = %v, want machine-1", MetaMachineID, got)
	}
	if got := meta[MetaAccessToken]; got != "tok-1" {
		t.Errorf("%s = %v", MetaAccessToken, got)
	}
	if got := meta[MetaRefreshToken]; got != "ref-1" {
		t.Errorf("%s = %v", MetaRefreshToken, got)
	}
	if got := meta[MetaExpiresAt]; got != expiresAt.UnixMilli() {
		t.Errorf("%s = %v, want %d", MetaExpiresAt, got, expiresAt.UnixMilli())
	}
	if got := meta[MetaUID]; got != "u-1" {
		t.Errorf("%s = %v", MetaUID, got)
	}
	if got := meta[MetaNickname]; got != "Code Buddy" {
		t.Errorf("%s = %v", MetaNickname, got)
	}
	if got := meta[MetaEnterpriseID]; got != "ent-1" {
		t.Errorf("%s = %v", MetaEnterpriseID, got)
	}
	if got := meta[MetaDepartmentInfo]; got != "dept-1" {
		t.Errorf("%s = %v", MetaDepartmentInfo, got)
	}
	expired, ok := meta[MetaExpired].(string)
	if !ok {
		t.Fatalf("%s = %v, want an RFC3339 string", MetaExpired, meta[MetaExpired])
	}
	if parsed, errParse := time.Parse(time.RFC3339, expired); errParse != nil || parsed.UnixMilli() != expiresAt.UnixMilli() {
		t.Errorf("%s = %q (err %v), want %s", MetaExpired, expired, errParse, expiresAt.UTC().Format(time.RFC3339))
	}
	if refreshed, okRefresh := meta[MetaLastRefresh].(string); !okRefresh {
		t.Errorf("%s = %v, want an RFC3339 string", MetaLastRefresh, meta[MetaLastRefresh])
	} else if _, errParse := time.Parse(time.RFC3339, refreshed); errParse != nil {
		t.Errorf("%s = %q, want RFC3339", MetaLastRefresh, refreshed)
	}

	identity := IdentityFromMetadata(meta, nil)
	if identity.Endpoint != DefaultEndpoint || identity.AccessToken != "tok-1" || identity.RefreshToken != "ref-1" {
		t.Errorf("identity = %+v", identity)
	}
	if identity.MachineID != "machine-1" || identity.UID != "u-1" || identity.EnterpriseID != "ent-1" || identity.DepartmentInfo != "dept-1" {
		t.Errorf("identity headers = %+v", identity)
	}
	if identity.ExpiresAtMS != expiresAt.UnixMilli() {
		t.Errorf("identity ExpiresAtMS = %d, want %d", identity.ExpiresAtMS, expiresAt.UnixMilli())
	}
	if identity.NeedsRefresh(expiresAt.Add(-10 * time.Minute)) {
		t.Error("identity unexpectedly needs refresh before the skew window")
	}
}

func TestBuildMetadataWithoutTokenUsesEndpointFallback(t *testing.T) {
	meta := BuildMetadata(nil, nil, "", "  ")
	if got := meta[MetaEndpoint]; got != DefaultEndpoint {
		t.Errorf("%s = %v, want %s", MetaEndpoint, got, DefaultEndpoint)
	}
	if got := meta[MetaDomain]; got != EndpointAuthority(DefaultEndpoint) {
		t.Errorf("%s = %v, want the endpoint authority", MetaDomain, got)
	}
	if _, ok := meta[MetaExpiresAt]; ok {
		t.Error("expires_at must be omitted when the expiry is unknown")
	}
	if got := IdentityFromMetadata(meta, nil).Endpoint; got != DefaultEndpoint {
		t.Errorf("identity endpoint = %q, want %s", got, DefaultEndpoint)
	}
}

func TestSaveTokenToFile(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).Truncate(time.Second)
	token := &TokenResult{AccessToken: "tok-1", RefreshToken: "ref-1", ExpiresAtMS: expiresAt.UnixMilli(), Domain: "custom.example"}
	account := &Account{UID: "u-1", Nickname: "Code Buddy"}
	storage := NewTokenStorage(token, account, "machine-1", DefaultEndpoint)
	storage.SetMetadata(map[string]any{"priority": float64(3)})

	path := filepath.Join(t.TempDir(), "auths", "codebuddy-1.json")
	if errSave := storage.SaveTokenToFile(path); errSave != nil {
		t.Fatalf("SaveTokenToFile() error = %v", errSave)
	}
	if runtime.GOOS != "windows" {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatalf("stat token file: %v", errStat)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file mode = %o, want 600", perm)
		}
	}

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read token file: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(raw, &saved); errUnmarshal != nil {
		t.Fatalf("unmarshal token file: %v", errUnmarshal)
	}
	if saved[MetaType] != Provider || saved[MetaEndpoint] != DefaultEndpoint || saved[MetaDomain] != "custom.example" {
		t.Errorf("saved identity fields = %v", saved)
	}
	if saved[MetaAccessToken] != "tok-1" || saved[MetaRefreshToken] != "ref-1" || saved[MetaMachineID] != "machine-1" {
		t.Errorf("saved token fields = %v", saved)
	}
	if saved[MetaUID] != "u-1" || saved[MetaNickname] != "Code Buddy" {
		t.Errorf("saved account fields = %v", saved)
	}
	if saved[MetaExpiresAt] != float64(expiresAt.UnixMilli()) {
		t.Errorf("%s = %v, want %d", MetaExpiresAt, saved[MetaExpiresAt], expiresAt.UnixMilli())
	}
	if saved["priority"] != float64(3) {
		t.Errorf("injected metadata lost: %v", saved["priority"])
	}
}

func TestWaitForLoginBoundsAttemptsByDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Hold the request until the client gives up, so a poll attempt that is not
		// bounded by the login deadline would hang forever.
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewLoginClient(&http.Client{}, server.URL)
	client.PollInterval = 10 * time.Millisecond
	client.Deadline = 60 * time.Millisecond

	start := time.Now()
	_, _, err := client.WaitForLogin(context.Background(), testLoginState)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("WaitForLogin() error = %v, want a timeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("WaitForLogin() blocked for %s, want the attempt bounded by the deadline", elapsed)
	}
}

// TestWaitForLoginPollsAccountWithoutInitialWait guards the account phase against an
// interval wait before its first poll: the token phase already consumed part of the
// shared deadline, so the account request must go out immediately.
func TestWaitForLoginPollsAccountWithoutInitialWait(t *testing.T) {
	var accountCalls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case AuthTokenPath:
			_, _ = w.Write([]byte(`{"data":{"data":{"accessToken":"tok-1","refreshToken":"ref-1","expiresIn":1800,"domain":"custom.example"}}}`))
		case AccountPath:
			accountCalls.Add(1)
			_, _ = w.Write([]byte(`{"data":{"uid":"u-1","nickname":"Code Buddy"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewLoginClient(server.Client(), server.URL)
	// The token phase polls after one interval, which leaves less than another full
	// interval before the deadline: the account phase can only succeed immediately.
	client.PollInterval = time.Second
	client.Deadline = 1500 * time.Millisecond

	token, account, err := client.WaitForLogin(context.Background(), testLoginState)
	if err != nil {
		t.Fatalf("WaitForLogin() error = %v", err)
	}
	if token == nil || token.AccessToken != "tok-1" {
		t.Fatalf("token = %+v", token)
	}
	if account == nil || account.UID != "u-1" {
		t.Fatalf("account = %+v", account)
	}
	if got := accountCalls.Load(); got != 1 {
		t.Errorf("account polls = %d, want 1", got)
	}
}
