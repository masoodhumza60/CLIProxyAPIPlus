package management

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// freebuffProviderID is the session provider name recorded for Freebuff logins.
const freebuffProviderID = "freebuff"

// freebuffLoginTimeout bounds the whole sign-in, from requesting a code to the
// browser handing one back. The user is doing the work, so it is generous.
const freebuffLoginTimeout = 5 * time.Minute

// freebuffLoginURLTimeout bounds how long we wait for the login to hand back the
// URL the user has to open. The URL is produced before any polling begins, so a
// delay here means the code request itself is failing or hanging.
const freebuffLoginURLTimeout = 30 * time.Second

// freebuffLoginService is the part of the Freebuff authenticator this handler
// needs. Narrowed to an interface so the flow can be exercised without a network.
type freebuffLoginService interface {
	Login(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error)
}

var newFreebuffLoginService = func() freebuffLoginService {
	return sdkAuth.NewFreebuffAuthenticator()
}

// RequestFreebuffToken starts a Freebuff sign-in and returns the URL to open.
//
// Freebuff has no redirect to intercept: it hands back a URL and a fingerprint,
// and the user signs in with whatever account they like. That makes the response
// shape identical to the other providers - a URL plus a state to poll - even
// though no callback server is involved.
func (h *Handler) RequestFreebuffToken(c *gin.Context) {
	if strings.TrimSpace(h.cfg.Freebuff.BaseURL) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "freebuff is not configured; set upstream.freebuff.base-url first"})
		return
	}

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	RegisterOAuthSession(state, freebuffProviderID)
	// Do not inherit the HTTP request cancellation: login continues after returning the URL.
	ctx := PopulateAuthContext(context.Background(), c)

	// Buffered so the login goroutine never blocks on a caller that already
	// gave up and returned.
	urlCh := make(chan string, 1)
	go h.completeFreebuffOAuth(ctx, state, urlCh, newFreebuffLoginService())

	timer := time.NewTimer(freebuffLoginURLTimeout)
	defer timer.Stop()
	select {
	case loginURL := <-urlCh:
		c.JSON(http.StatusOK, gin.H{"status": "ok", "url": loginURL, "state": state})
	case <-timer.C:
		if IsOAuthSessionPending(state, freebuffProviderID) {
			SetOAuthSessionError(state, "Freebuff did not return a sign-in URL")
		}
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "freebuff sign-in did not start"})
	}
}

// completeFreebuffOAuth drives the login and records the outcome on the session.
func (h *Handler) completeFreebuffOAuth(ctx context.Context, state string, urlCh chan<- string, svc freebuffLoginService) {
	ctx, cancel := context.WithTimeout(ctx, freebuffLoginTimeout)
	defer cancel()
	go watchOAuthSessionCancel(ctx, cancel, state, freebuffProviderID)

	record, errLogin := svc.Login(ctx, h.cfg, &sdkAuth.LoginOptions{
		// This process is a server. It has no browser to open and no console the
		// user is watching, so the URL is handed back over HTTP instead.
		NoBrowser: true,
		OnLoginURL: func(loginURL string) {
			if strings.TrimSpace(loginURL) == "" {
				return
			}
			select {
			case urlCh <- loginURL:
			default:
			}
		},
	})
	if errLogin != nil {
		if IsOAuthSessionPending(state, freebuffProviderID) {
			SetOAuthSessionError(state, freebuffLoginErrorMessage(errLogin))
		}
		return
	}
	if errGuard := guardOAuthSessionPendingForSave(state, freebuffProviderID); errGuard != nil {
		return
	}
	if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
		SetOAuthSessionError(state, "Failed to save Freebuff authentication tokens")
		return
	}
	CompleteOAuthSession(state)
	log.Info("Freebuff authentication successful")
}

// freebuffLoginErrorMessage turns a login failure into something safe to show.
//
// The Freebuff client wraps upstream responses verbatim, and those can quote the
// request that produced them. Access gates in particular arrive as prose that is
// worth reading, so the message is kept rather than replaced - but the token is
// stripped, because a user reading a login error has no need for it and it would
// end up in their logs.
func freebuffLoginErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Freebuff sign-in timed out"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "Freebuff sign-in failed"
	}
	return message
}
