package executor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ensure config import is used
var _ = config.QoderKey{}

// Execute performs a non-streaming request through the Qoder CLI.
// This implements the auth.ProviderExecutor interface.
func (e *QoderExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	pluginReq := e.buildPluginRequest(req, auth)
	pluginResp, err := e.ExecutePlugin(ctx, pluginReq)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{
		Payload: pluginResp.Payload,
		Headers: pluginResp.Headers,
	}, nil
}

// ExecuteStream performs a streaming request through the Qoder CLI.
// This implements the auth.ProviderExecutor interface.
func (e *QoderExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	pluginReq := e.buildPluginRequest(req, auth)
	pluginResp, err := e.ExecuteStreamPlugin(ctx, pluginReq)
	if err != nil {
		return nil, err
	}
	chunkCh := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		defer close(chunkCh)
		for chunk := range pluginResp.Chunks {
			chunkCh <- cliproxyexecutor.StreamChunk{Payload: chunk.Payload}
		}
	}()
	return &cliproxyexecutor.StreamResult{
		Headers: pluginResp.Headers,
		Chunks:  chunkCh,
	}, nil
}

// CountTokens is not supported by Qoder CLI.
func (e *QoderExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("qoder: CountTokens not supported")
}

// Refresh is a no-op for Qoder CLI.
func (e *QoderExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

// HttpRequest is not supported by Qoder CLI.
func (e *QoderExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("qoder: HttpRequest not supported")
}

// buildPluginRequest converts a cliproxyexecutor.Request to a pluginapi.ExecutorRequest.
func (e *QoderExecutor) buildPluginRequest(req cliproxyexecutor.Request, auth *cliproxyauth.Auth) pluginapi.ExecutorRequest {
	metadata := map[string]any{}
	if req.Metadata != nil {
		for k, v := range req.Metadata {
			metadata[k] = v
		}
	}
	if auth != nil {
		metadata["auth_id"] = auth.ID
		metadata["auth_provider"] = auth.Provider
	}

	return pluginapi.ExecutorRequest{
		AuthID:          authID(auth),
		AuthProvider:    authProvider(auth),
		Model:           req.Model,
		Format:          req.Format.String(),
		Stream:          false,
		Payload:         req.Payload,
		Metadata:        metadata,
		OriginalRequest: req.Payload,
	}
}

// authID returns the auth ID or empty string.
func authID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.ID
}

// authProvider returns the auth provider or empty string.
func authProvider(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.Provider
}

// compile-time interface check
var _ interface {
	Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
	HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error)
} = (*QoderExecutor)(nil)
