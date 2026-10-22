// Package handler: DeepSeek Search/Fetch tool APIs.
//
// The upstream DeepSeek platform exposes two read-only JSON APIs at
// `/v1/tools/search` and `/v1/tools/fetch` (see
// https://api-docs.deepseek.com/zh-cn/guides/search). Accounts configured as
// DeepSeek already carry an official API key, so the gateway does not reimplement
// retrieval: it selects an account from the key's group and forwards the request
// verbatim, billing one successful call at a time.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	toolsSearchPath     = "/v1/tools/search"
	toolsFetchPath      = "/v1/tools/fetch"
	toolsRequestLimit   = 256 << 10
	toolsResponseLimit  = 8 << 20
	toolsUpstreamTimout = 30 * time.Second
)

// toolsSearchRequest mirrors the documented Search request; every optional field
// stays a pointer so an omitted value is forwarded as omitted rather than zero.
type toolsSearchRequest struct {
	Query        string  `json:"query"`
	Count        *int    `json:"count"`
	OriQuestion  *string `json:"ori_question"`
	Localization *string `json:"localization"`
	NeedContent  *bool   `json:"need_content"`
}

// toolsFetchRequest mirrors the documented Fetch request.
type toolsFetchRequest struct {
	URL string `json:"url"`
}

// ToolsSearch forwards one Search request to the selected DeepSeek account.
func (h *GatewayHandler) ToolsSearch(c *gin.Context) { h.forwardToolsRequest(c, toolsSearchPath) }

// ToolsFetch forwards one Fetch request to the selected DeepSeek account.
func (h *GatewayHandler) ToolsFetch(c *gin.Context) { h.forwardToolsRequest(c, toolsFetchPath) }

func (h *GatewayHandler) forwardToolsRequest(c *gin.Context, path string) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		c.JSON(http.StatusUnauthorized, toolsError("authentication_error", "API key required"))
		return
	}
	if apiKey.GroupID == nil || apiKey.Group == nil {
		c.JSON(http.StatusBadRequest, toolsError("invalid_request_error", "group required"))
		return
	}
	if apiKey.Group.Platform != service.PlatformDeepseek {
		c.JSON(http.StatusBadRequest, toolsError("invalid_request_error", "tools API is only supported for deepseek groups"))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, toolsRequestLimit))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, toolsError("invalid_request_error", "request body too large"))
		return
	}
	if err := validateToolsBody(path, body); err != nil {
		c.JSON(http.StatusBadRequest, toolsError("invalid_request_error", err.Error()))
		return
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		c.JSON(status, toolsError(code, message))
		return
	}

	// 与其它网关请求一致：先过内容审计，再把查询/URL 发往上游。
	subject, _ := middleware2.GetAuthSubjectFromContext(c)
	reqLog := requestLogger(c, "handler.gateway.tools_api")
	auditPrompt := toolsAuditPrompt(path, body)
	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat,
		toolsUsageModel(path), []byte(`{"messages":[{"role":"user","content":`+strconv.Quote(auditPrompt)+`}]}`)); decision != nil && !decision.AllowNextStage {
		status := decision.HTTPStatus
		if status == 0 {
			status = http.StatusForbidden
		}
		code := decision.ErrorCode
		if code == "" {
			code = "content_policy_violation"
		}
		message := decision.ClientMessage
		if message == "" {
			message = "Request blocked by content policy"
		}
		c.JSON(status, toolsError(code, message))
		return
	}
	selected, err := h.gatewayService.SelectAccountWithLoadAwareness(c.Request.Context(), apiKey.GroupID, "", "", map[int64]struct{}{}, "", 0)
	if err != nil || selected == nil || selected.Account == nil {
		c.JSON(http.StatusServiceUnavailable, toolsError("scheduling_error", "No available accounts"))
		return
	}
	account := selected.Account
	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		c.JSON(http.StatusServiceUnavailable, toolsError("scheduling_error", "The selected account has no API key"))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), toolsUpstreamTimout)
	defer cancel()
	upstream, status, err := callToolsUpstream(ctx, account.GetBaseURL()+strings.TrimPrefix(path, "/v1"), token, body)
	if err != nil {
		c.JSON(http.StatusGatewayTimeout, toolsError("upstream_timeout", "DeepSeek tools API is unreachable"))
		return
	}
	if status != http.StatusOK {
		// Upstream already answers with the OpenAI-compatible error envelope; keep
		// its status and body so callers see the documented 400/401/402/403/404/429.
		c.Data(status, "application/json; charset=utf-8", upstream)
		return
	}
	if !toolsBillable(path, upstream) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", upstream)
		return
	}

	h.recordToolsUsage(c, apiKey, account, subscription, path, body)
	c.Data(http.StatusOK, "application/json; charset=utf-8", upstream)
}

// validateToolsBody enforces the documented request constraints before the call.
func validateToolsBody(path string, body []byte) error {
	switch path {
	case toolsSearchPath:
		var req toolsSearchRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		if strings.TrimSpace(req.Query) == "" {
			return errToolsQuery
		}
		if req.Count != nil && (*req.Count < 1 || *req.Count > 50) {
			return errToolsCount
		}
	case toolsFetchPath:
		var req toolsFetchRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		if strings.TrimSpace(req.URL) == "" {
			return errToolsURL
		}
	}
	return nil
}

type toolsValidationError string

func (e toolsValidationError) Error() string { return string(e) }

const (
	errToolsQuery toolsValidationError = "'query' is required"
	errToolsCount toolsValidationError = "'count' must be between 1 and 50"
	errToolsURL   toolsValidationError = "'url' is required"
)

// callToolsUpstream performs the forwarding call and returns the raw body.
func callToolsUpstream(ctx context.Context, endpoint, token string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, toolsResponseLimit))
	if err != nil {
		return nil, 0, err
	}
	return payload, response.StatusCode, nil
}

// toolsBillable reports whether a 200 response produced billable content: Search
// needs a non-empty result list, Fetch needs page content.
func toolsBillable(path string, payload []byte) bool {
	switch path {
	case toolsSearchPath:
		var result struct {
			Results []json.RawMessage `json:"results"`
		}
		if json.Unmarshal(payload, &result) != nil {
			return false
		}
		return len(result.Results) > 0
	case toolsFetchPath:
		var result struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(payload, &result) != nil {
			return false
		}
		return strings.TrimSpace(result.Content) != ""
	}
	return false
}

// recordToolsUsage bills one successful tools call with the group's per-search price.
func (h *GatewayHandler) recordToolsUsage(c *gin.Context, apiKey *service.APIKey, account *service.Account,
	subscription *service.UserSubscription, path string, body []byte) {
	input := &service.RecordUsageInput{
		Result:             &service.ForwardResult{RequestID: path + ":" + uuid.NewString(), Model: toolsUsageModel(path), SearchCount: 1},
		APIKey:             apiKey,
		User:               apiKey.User,
		Account:            account,
		Subscription:       subscription,
		InboundEndpoint:    GetInboundEndpoint(c),
		UpstreamEndpoint:   GetUpstreamEndpoint(c, account.Platform),
		UserAgent:          c.GetHeader("User-Agent"),
		RequestPayloadHash: service.HashUsageRequestPayload(body),
		APIKeyService:      h.apiKeyService,
		QuotaPlatform:      service.QuotaPlatform(c.Request.Context(), apiKey),
	}
	if err := h.gatewayService.RecordUsage(c.Request.Context(), input); err != nil {
		logger.L().With(
			zap.String("component", "handler.gateway.tools_api"),
			zap.Int64("user_id", apiKey.User.ID),
			zap.Int64("api_key_id", apiKey.ID),
			zap.Int64("account_id", account.ID),
			zap.String("path", path),
		).Error("gateway.tools_api.record_usage_failed", zap.Error(err))
	}
}

// toolsUsageModel names the billed operation; keep in sync with recordToolsUsage.
func toolsUsageModel(path string) string {
	if path == toolsFetchPath {
		return "deepseek-tools-fetch"
	}
	return "deepseek-tools-search"
}

// toolsAuditPrompt extracts the user-provided text for content moderation.
func toolsAuditPrompt(path string, body []byte) string {
	if path == toolsFetchPath {
		var req toolsFetchRequest
		if json.Unmarshal(body, &req) == nil {
			return req.URL
		}
		return ""
	}
	var req toolsSearchRequest
	if json.Unmarshal(body, &req) == nil {
		return req.Query
	}
	return ""
}

func toolsError(kind, message string) gin.H {
	return gin.H{"error": gin.H{"type": kind, "message": message, "param": nil, "code": kind}}
}
