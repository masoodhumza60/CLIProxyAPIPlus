//go:build freebuffmock

package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/freebuff"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/claude"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const freebuffE2EModel = "deepseek/deepseek-v4-pro"

// freebuffE2EService boots the real request pipeline - HTTP handler, auth
// manager, conductor and executor - against a local protocol fixture.
//
// It deliberately does not start the real server binary: the provider has no
// operator-configurable endpoint, so a process launched from config.yaml cannot
// reach Freebuff at all. Everything above the transport is still the production
// code path, which is what this test is for.
func freebuffE2EService(t *testing.T) *cliproxyauth.Manager {
	t.Helper()

	upstream := freebuff.NewMockUpstream()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)

	cfg := &config.Config{}
	manager.RegisterExecutor(runtimeexecutor.NewFreebuffExecutorForTest(cfg, upstreamServer.URL))

	authID := "freebuff-e2e"
	registry.GetGlobalRegistry().RegisterClient(authID, "freebuff", []*registry.ModelInfo{{ID: freebuffE2EModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{
		ID:       authID,
		Provider: "freebuff",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": upstreamServer.URL,
			"api_key":  freebuff.MockAPIKey,
		},
		Metadata: map[string]any{"disable_cooling": true},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return manager
}

// TestFreebuffEndToEndChatCompletion drives a real /v1/chat/completions request
// through the whole proxy stack and asserts the model actually answers.
func TestFreebuffEndToEndChatCompletion(t *testing.T) {
	manager := freebuffE2EService(t)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+freebuffE2EModel+`","messages":[{"role":"user","content":"hello"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	base := handlers.NewBaseAPIHandlers(&(&config.Config{}).SDKConfig, manager)
	openaihandlers.NewOpenAIAPIHandler(base).ChatCompletions(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}

	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not an OpenAI completion object: %v; body: %s", err, recorder.Body.String())
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got.Object)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1; body: %s", len(got.Choices), recorder.Body.String())
	}
	if got.Choices[0].Message.Content != freebuff.MockReply {
		t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, freebuff.MockReply)
	}
	if got.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", got.Choices[0].Message.Role)
	}
	if got.Usage.TotalTokens <= 0 {
		t.Errorf("usage.total_tokens = %d, want a positive count so the request is accounted for", got.Usage.TotalTokens)
	}
}

// TestFreebuffEndToEndStreamingChat drives a streaming request and asserts the
// SSE frames arrive and the stream terminates with [DONE] rather than hanging.
func TestFreebuffEndToEndStreamingChat(t *testing.T) {
	manager := freebuffE2EService(t)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+freebuffE2EModel+`","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	base := handlers.NewBaseAPIHandlers(&(&config.Config{}).SDKConfig, manager)
	openaihandlers.NewOpenAIAPIHandler(base).ChatCompletions(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, freebuff.MockReply) {
		t.Errorf("stream did not carry the model reply; body: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream did not terminate with [DONE]; body: %s", body)
	}
}

// TestFreebuffEndToEndAnthropicMessages drives the same provider through the
// Anthropic client interface, proving the shared OpenAI-shaped upstream serves
// more than one client protocol.
func TestFreebuffEndToEndAnthropicMessages(t *testing.T) {
	manager := freebuffE2EService(t)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+freebuffE2EModel+`","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("x-api-key", "test")

	base := handlers.NewBaseAPIHandlers(&(&config.Config{}).SDKConfig, manager)
	claudehandlers.NewClaudeCodeAPIHandler(base).ClaudeMessages(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), freebuff.MockReply) {
		t.Errorf("Anthropic response did not carry the model reply; body: %s", recorder.Body.String())
	}
}
