/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

const ahpTokensAction = "ahpTokens"

// CreateAHPTokenRequest asks for an AHP connection token.
type CreateAHPTokenRequest struct {
	// TTL is a Go duration such as "720h"; empty means the default lifetime.
	TTL         string `json:"ttl,omitempty"`
	Description string `json:"description,omitempty"`
}

// AHPTokenResponse describes an AHP connection token. Token is set only in
// the response that minted it.
type AHPTokenResponse struct {
	ID          string     `json:"id"`
	Token       string     `json:"token,omitempty"`
	Description string     `json:"description,omitempty"`
	Namespace   string     `json:"namespace,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	Delegated   bool       `json:"delegated"`
}

// ahpTokenIdentity admits only the identities that may own AHP tokens: a
// verified OIDC or context-token person. A delegated context token must
// carry an AHP token scope.
func (h *Handlers) ahpTokenIdentity(c fiber.Ctx) (*UserInfo, string, error) {
	if h.ahp == nil {
		return nil, "", fiber.NewError(fiber.StatusNotImplemented, "the AHP API is not enabled on this controller")
	}
	ui := GetUserInfo(c)
	if ui == nil {
		return nil, "", fiber.NewError(fiber.StatusUnauthorized, "missing authenticated identity")
	}
	if ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken {
		return nil, "", fiber.NewError(fiber.StatusForbidden, "AHP tokens require a verified OIDC or context-token identity")
	}
	if strings.TrimSpace(ui.Subject) == "" || strings.TrimSpace(ui.Issuer) == "" {
		return nil, "", fiber.NewError(fiber.StatusForbidden, "AHP tokens require an identity with issuer and subject")
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return nil, "", err
	}
	if err := h.authorizeContextTokenAction(c, ahpTokensAction, h.contextTokenAuthorization.AHPTokenScopes); err != nil {
		return nil, "", err
	}
	return ui, namespace, nil
}

// CreateAHPToken mints an AHP connection token for the caller. A token minted
// from a context token keeps that token's scopes and transaction context and
// expires no later than it; the context token itself is never stored.
func (h *Handlers) CreateAHPToken(c fiber.Ctx) error {
	ui, namespace, err := h.ahpTokenIdentity(c)
	if err != nil {
		return err
	}
	var req CreateAHPTokenRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	var ttl time.Duration
	if strings.TrimSpace(req.TTL) != "" {
		if ttl, err = time.ParseDuration(req.TTL); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "ttl must be a duration such as 720h")
		}
	}
	mint := ahphost.MintRequest{
		Issuer: ui.Issuer, Subject: ui.Subject, Username: ui.Username, Namespace: namespace,
		TTL: ttl, Description: req.Description,
	}
	if ui.AuthType == AuthTypeContextToken {
		if ui.ContextToken == nil {
			return fiber.NewError(fiber.StatusForbidden, "the context token is unavailable")
		}
		expiry, ok := contextTokenExpiry(ui.ContextToken)
		if !ok {
			return fiber.NewError(fiber.StatusForbidden, "the context token has no usable expiry")
		}
		mint.NotAfter = &expiry
		mint.Constraints = ahphost.Constraints{
			Delegated:          true,
			Scopes:             append([]string{}, ui.ContextToken.Scopes...),
			TransactionContext: cloneMap(ui.ContextToken.TransactionContext),
		}
	}
	minted, err := h.ahp.Tokens().Mint(c.Context(), mint)
	if errors.Is(err, ahphost.ErrInvalidMintRequest) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to mint AHP token")
	}
	return c.Status(fiber.StatusCreated).JSON(AHPTokenResponse{
		ID: minted.ID, Token: minted.Token, Description: req.Description, Namespace: namespace,
		ExpiresAt: minted.ExpiresAt, Delegated: mint.Constraints.Delegated,
	})
}

// ListAHPTokens lists the caller's AHP tokens without their secrets.
func (h *Handlers) ListAHPTokens(c fiber.Ctx) error {
	ui, _, err := h.ahpTokenIdentity(c)
	if err != nil {
		return err
	}
	tokens, err := h.ahp.Tokens().List(c.Context(), ui.Issuer, ui.Subject)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list AHP tokens")
	}
	items := make([]AHPTokenResponse, 0, len(tokens))
	for _, token := range tokens {
		created := token.CreatedAt
		items = append(items, AHPTokenResponse{
			ID: token.ID, Description: token.Description, Namespace: token.Namespace,
			CreatedAt: &created, ExpiresAt: token.ExpiresAt, RevokedAt: token.RevokedAt, Delegated: token.Delegated,
		})
	}
	return c.JSON(fiber.Map{apiFieldItems: items})
}

// RevokeAHPToken revokes one of the caller's AHP tokens and closes every
// connection that uses it. Another person's token is reported as missing.
func (h *Handlers) RevokeAHPToken(c fiber.Ctx) error {
	ui, _, err := h.ahpTokenIdentity(c)
	if err != nil {
		return err
	}
	err = h.ahp.Tokens().Revoke(c.Context(), ui.Issuer, ui.Subject, c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "AHP token not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to revoke AHP token")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// contextTokenExpiry reads the verified context token's exp claim.
func contextTokenExpiry(token *ContextToken) (time.Time, bool) {
	var seconds float64
	switch exp := token.Claims["exp"].(type) {
	case json.Number:
		value, err := exp.Float64()
		if err != nil {
			return time.Time{}, false
		}
		seconds = value
	case float64:
		seconds = exp
	case int64:
		seconds = float64(exp)
	default:
		return time.Time{}, false
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > math.MaxInt32*4 {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0).UTC(), true
}

const ahpEndpointPath = "/ahp"

// ahpEndpoint serves /ahp through the host's net/http handler. The request
// URI can carry the connection token, so a URI the adaptor cannot parse is
// refused here rather than reaching the adaptor, which would log it.
func ahpEndpoint(host *ahphost.Host) fiber.Handler {
	serve := adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.ServeHTTP(&switchingProtocolsWriter{ResponseWriter: w}, r)
	}))
	return func(c fiber.Ctx) error {
		if _, err := url.ParseRequestURI(string(c.Request().RequestURI())); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request URI")
		}
		return serve(c)
	}
}

// switchingProtocolsWriter sends the 101 response a WebSocket handshake wrote
// when the handler hijacks the connection. The fasthttp adaptor buffers the
// status and headers and drops them on hijack, so without this the client
// would wait for a handshake response that never comes.
type switchingProtocolsWriter struct {
	http.ResponseWriter
	status int
}

func (w *switchingProtocolsWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *switchingProtocolsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil || w.status != http.StatusSwitchingProtocols {
		return conn, rw, err
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_ = w.Header().Write(rw)
	_, _ = rw.WriteString("\r\n")
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}
