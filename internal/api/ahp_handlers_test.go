/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gofiber/fiber/v3"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const ahpTestIssuer = "https://issuer.example"

type ahpTestHarness struct {
	t        *testing.T
	host     *ahphost.Host
	app      *fiber.App
	identity *UserInfo
}

func newAHPTestHarness(t *testing.T, authz ContextTokenAuthorizationConfig) *ahpTestHarness {
	t.Helper()
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	backends, err := ahphost.NewRegistry()
	require.NoError(t, err)
	tokens := &ahphost.TokenService{Store: s}
	host, err := ahphost.New(ahphost.Config{Store: s, Tokens: tokens, Backends: backends, Logger: logr.Discard()})
	require.NoError(t, err)
	tokens.OnRevoke = host.CloseToken

	h := &ahpTestHarness{t: t, host: host}
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "alice", Subject: "alice", Issuer: ahpTestIssuer}
	handlers := NewHandlers(HandlersConfig{ContextTokenAuthorization: authz, AHP: host})
	app := fiber.New()
	app.Get(ahpEndpointPath, ahpEndpoint(host))
	app.Use(func(c fiber.Ctx) error {
		if h.identity != nil {
			c.Locals(UserInfoContextKey, h.identity)
		}
		return c.Next()
	})
	app.Post("/api/v1/ahp-tokens", handlers.CreateAHPToken)
	app.Get("/api/v1/ahp-tokens", handlers.ListAHPTokens)
	app.Delete("/api/v1/ahp-tokens/:id", handlers.RevokeAHPToken)
	h.app = app
	return h
}

func (h *ahpTestHarness) do(method, path, body string) (int, []byte) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.app.Test(req)
	require.NoError(h.t, err)
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp.StatusCode, raw
}

func (h *ahpTestHarness) mint(body string) AHPTokenResponse {
	h.t.Helper()
	status, raw := h.do(http.MethodPost, "/api/v1/ahp-tokens", body)
	require.Equal(h.t, http.StatusCreated, status, string(raw))
	var minted AHPTokenResponse
	require.NoError(h.t, json.Unmarshal(raw, &minted))
	return minted
}

func TestAHPTokenRoutesMintListAndRevoke(t *testing.T) {
	h := newAHPTestHarness(t, ContextTokenAuthorizationConfig{})
	minted := h.mint(`{"ttl":"2h","description":"laptop"}`)
	require.True(t, strings.HasPrefix(minted.Token, ahphost.TokenPrefix))
	require.WithinDuration(t, time.Now().Add(2*time.Hour), minted.ExpiresAt, time.Minute)
	require.False(t, minted.Delegated)

	status, raw := h.do(http.MethodGet, "/api/v1/ahp-tokens", "")
	require.Equal(t, http.StatusOK, status)
	require.NotContains(t, string(raw), minted.Token, "a listing never returns a secret")
	var list struct{ Items []AHPTokenResponse }
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, minted.ID, list.Items[0].ID)
	require.Equal(t, "laptop", list.Items[0].Description)

	for name, body := range map[string]string{
		"unknown field": `{"subject":"bob"}`,
		"bad ttl":       `{"ttl":"forever"}`,
		"ttl too long":  `{"ttl":"100000h"}`,
	} {
		status, raw := h.do(http.MethodPost, "/api/v1/ahp-tokens", body)
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", name, raw)
	}

	// Another person sees none of alice's tokens and cannot revoke them.
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "bob", Subject: "bob", Issuer: ahpTestIssuer}
	status, raw = h.do(http.MethodGet, "/api/v1/ahp-tokens", "")
	require.Equal(t, http.StatusOK, status)
	require.NotContains(t, string(raw), minted.ID)
	status, _ = h.do(http.MethodDelete, "/api/v1/ahp-tokens/"+minted.ID, "")
	require.Equal(t, http.StatusNotFound, status)

	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "alice", Subject: "alice", Issuer: ahpTestIssuer}
	status, _ = h.do(http.MethodDelete, "/api/v1/ahp-tokens/"+minted.ID, "")
	require.Equal(t, http.StatusNoContent, status)
	_, err := h.host.Tokens().Verify(t.Context(), minted.Token)
	require.ErrorIs(t, err, ahphost.ErrInvalidToken)
	_, raw = h.do(http.MethodGet, "/api/v1/ahp-tokens", "")
	require.NoError(t, json.Unmarshal(raw, &list))
	require.NotNil(t, list.Items[0].RevokedAt)
}

func TestAHPTokenRoutesRefuseNonHumanIdentities(t *testing.T) {
	h := newAHPTestHarness(t, ContextTokenAuthorizationConfig{})
	for name, identity := range map[string]*UserInfo{
		"service account":      {AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:tenant:bot", Namespace: "tenant"},
		"oidc without subject": {AuthType: AuthTypeOIDC, Username: "x", Issuer: ahpTestIssuer},
		"oidc without issuer":  {AuthType: AuthTypeOIDC, Username: "x", Subject: "x"},
	} {
		h.identity = identity
		status, raw := h.do(http.MethodPost, "/api/v1/ahp-tokens", "")
		require.Equal(t, http.StatusForbidden, status, "%s: %s", name, raw)
	}
	h.identity = nil
	status, _ := h.do(http.MethodGet, "/api/v1/ahp-tokens", "")
	require.Equal(t, http.StatusUnauthorized, status)

	disabled := NewHandlers(HandlersConfig{})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Subject: "alice", Issuer: ahpTestIssuer})
		return c.Next()
	})
	app.Post("/api/v1/ahp-tokens", disabled.CreateAHPToken)
	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/v1/ahp-tokens", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestAHPTokenFromAContextTokenKeepsItsConstraints(t *testing.T) {
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	h := newAHPTestHarness(t, authz)
	exp := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	contextToken := func(claims map[string]any, scopes ...string) *UserInfo {
		return &UserInfo{
			AuthType: AuthTypeContextToken, Username: "carol", Subject: "carol", Issuer: ahpTestIssuer, Namespace: "team",
			ContextToken: &ContextToken{
				Subject: "carol", Issuer: ahpTestIssuer, Scopes: scopes,
				TransactionContext: map[string]any{"namespace": "team"}, Claims: claims,
			},
		}
	}
	withExp := map[string]any{"exp": json.Number(strconv.FormatInt(exp.Unix(), 10))}

	h.identity = contextToken(withExp, "orka:tasks:get")
	status, _ := h.do(http.MethodPost, "/api/v1/ahp-tokens", "")
	require.Equal(t, http.StatusForbidden, status, "an unrelated scope cannot mint")

	h.identity = contextToken(map[string]any{}, ContextTokenScopeAHPTokens)
	status, _ = h.do(http.MethodPost, "/api/v1/ahp-tokens", "")
	require.Equal(t, http.StatusForbidden, status, "a context token without an expiry cannot mint")

	h.identity = contextToken(withExp, ContextTokenScopeAHPTokens)
	status, _ = h.do(http.MethodPost, "/api/v1/ahp-tokens?namespace=other", "")
	require.Equal(t, http.StatusForbidden, status, "the token's namespace constraint applies")

	minted := h.mint(`{"ttl":"720h"}`)
	require.True(t, minted.Delegated)
	require.Equal(t, exp.UTC(), minted.ExpiresAt.UTC(), "expires no later than the context token")
	principal, err := h.host.Tokens().Verify(t.Context(), minted.Token)
	require.NoError(t, err)
	require.Equal(t, ahphost.UserKey(ahpTestIssuer, "carol"), principal.UserKey)
	require.Equal(t, "team", principal.Namespace)
	require.True(t, principal.Constraints.Delegated)
	require.Equal(t, []string{ContextTokenScopeAHPTokens}, principal.Constraints.Scopes)
	require.Equal(t, "team", principal.Constraints.TransactionContext["namespace"])
}

// TestAHPEndpointServesThroughFiber drives /ahp with the official client
// through the Fiber adaptor the API server uses, from minting to revocation.
func TestAHPEndpointServesThroughFiber(t *testing.T) {
	h := newAHPTestHarness(t, ContextTokenAuthorizationConfig{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = h.app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = h.app.Shutdown() })
	base := "http://" + listener.Addr().String() + ahpEndpointPath

	resp, err := http.Get(base + "?tkn=orka_ahp_wrong")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	minted := h.mint("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	transport, err := ahpws.Connect(ctx, "ws"+strings.TrimPrefix(base, "http")+"?tkn="+minted.Token)
	require.NoError(t, err)
	client, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	result, err := client.Initialize(ctx, "fiber-client", ahptypes.SupportedProtocolVersions(), []string{ahptypes.RootResourceURI})
	require.NoError(t, err)
	require.Equal(t, "1.0.0", result.ProtocolVersion)
	require.NoError(t, client.Ping(ctx))

	status, _ := h.do(http.MethodDelete, "/api/v1/ahp-tokens/"+minted.ID, "")
	require.Equal(t, http.StatusNoContent, status)
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatal("revoking the token must close its connection")
	}
}
