package handler

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestValidateToolsBody(t *testing.T) {
	require.NoError(t, validateToolsBody(toolsSearchPath, []byte(`{"query":"deepseek"}`)))
	require.NoError(t, validateToolsBody(toolsSearchPath, []byte(`{"query":"deepseek","count":50,"need_content":true}`)))
	require.EqualError(t, validateToolsBody(toolsSearchPath, []byte(`{"query":"   "}`)), "'query' is required")
	require.EqualError(t, validateToolsBody(toolsSearchPath, []byte(`{"count":5}`)), "'query' is required")
	require.EqualError(t, validateToolsBody(toolsSearchPath, []byte(`{"query":"x","count":0}`)), "'count' must be between 1 and 50")
	require.EqualError(t, validateToolsBody(toolsSearchPath, []byte(`{"query":"x","count":51}`)), "'count' must be between 1 and 50")
	require.Error(t, validateToolsBody(toolsSearchPath, []byte(`not json`)))

	require.NoError(t, validateToolsBody(toolsFetchPath, []byte(`{"url":"https://example.com/a"}`)))
	require.EqualError(t, validateToolsBody(toolsFetchPath, []byte(`{"url":""}`)), "'url' is required")
	require.Error(t, validateToolsBody(toolsFetchPath, []byte(`[]`)))
}

func TestToolsBillable(t *testing.T) {
	require.True(t, toolsBillable(toolsSearchPath, []byte(`{"results":[{"url":"https://a"}]}`)))
	require.False(t, toolsBillable(toolsSearchPath, []byte(`{"results":[]}`)))
	require.False(t, toolsBillable(toolsSearchPath, []byte(`{}`)))
	require.False(t, toolsBillable(toolsSearchPath, []byte(`oops`)))

	require.True(t, toolsBillable(toolsFetchPath, []byte(`{"title":"t","content":"# hi"}`)))
	require.False(t, toolsBillable(toolsFetchPath, []byte(`{"title":"t","content":"  "}`)))
	require.False(t, toolsBillable(toolsFetchPath, []byte(`{}`)))
}

func TestToolsUsageModelAndAuditPrompt(t *testing.T) {
	require.Equal(t, "deepseek-tools-search", toolsUsageModel(toolsSearchPath))
	require.Equal(t, "deepseek-tools-fetch", toolsUsageModel(toolsFetchPath))
	require.Equal(t, "deepseek", toolsAuditPrompt(toolsSearchPath, []byte(`{"query":"deepseek"}`)))
	require.Equal(t, "https://example.com", toolsAuditPrompt(toolsFetchPath, []byte(`{"url":"https://example.com"}`)))
	require.Equal(t, "", toolsAuditPrompt(toolsFetchPath, []byte(`bad`)))
}

func TestToolsErrorEnvelope(t *testing.T) {
	envelope := toolsError("invalid_request_error", "boom")
	errorBody, ok := envelope["error"].(gin.H)
	require.True(t, ok)
	require.Equal(t, "invalid_request_error", errorBody["type"])
	require.Equal(t, "boom", errorBody["message"])
	require.Nil(t, errorBody["param"])
	require.Equal(t, "invalid_request_error", errorBody["code"])
}
