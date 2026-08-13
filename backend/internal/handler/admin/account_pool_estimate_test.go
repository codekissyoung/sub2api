package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupOpenAIPoolEstimateRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.GET("/api/v1/admin/accounts/openai-pool-estimate", handler.GetOpenAIPoolEstimate)
	return router, adminSvc
}

func TestAccountHandlerGetOpenAIPoolEstimate(t *testing.T) {
	router, adminSvc := setupOpenAIPoolEstimateRouter()
	adminSvc.accounts = []service.Account{
		{
			ID:     1,
			Name:   "openai-a",
			Status: service.StatusActive,
			Extra: map[string]any{
				"codex_primary_used_percent": 25.0, // 剩余 75% → $1500
				"codex_usage_updated_at":     "2026-08-13T00:30:00Z",
			},
		},
		{
			ID:     2,
			Name:   "openai-b",
			Status: service.StatusActive,
			Extra: map[string]any{
				"codex_7d_used_percent": 100.0, // 用尽 → $0
			},
		},
		{
			ID:     3,
			Name:   "openai-no-data",
			Status: service.StatusActive,
			Extra:  map[string]any{}, // 缺数据
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/openai-pool-estimate", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, adminSvc.schedulerScoreFilterCalls)

	var payload struct {
		Data struct {
			TotalUSD             float64 `json:"total_usd"`
			AccountCount         int     `json:"account_count"`
			MissingDataCount     int     `json:"missing_data_count"`
			NewestUsageUpdatedAt *string `json:"newest_usage_updated_at"`
			OldestUsageUpdatedAt *string `json:"oldest_usage_updated_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.InEpsilon(t, 1500.0, payload.Data.TotalUSD, 1e-9)
	require.Equal(t, 2, payload.Data.AccountCount)
	require.Equal(t, 1, payload.Data.MissingDataCount)
	require.NotNil(t, payload.Data.NewestUsageUpdatedAt)
	require.Equal(t, "2026-08-13T00:30:00Z", *payload.Data.NewestUsageUpdatedAt)
	require.NotNil(t, payload.Data.OldestUsageUpdatedAt)
	require.Equal(t, "2026-08-13T00:30:00Z", *payload.Data.OldestUsageUpdatedAt)
}

func TestAccountHandlerGetOpenAIPoolEstimateEmptyPool(t *testing.T) {
	router, adminSvc := setupOpenAIPoolEstimateRouter()
	adminSvc.accounts = []service.Account{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/openai-pool-estimate", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data struct {
			TotalUSD             float64 `json:"total_usd"`
			AccountCount         int     `json:"account_count"`
			MissingDataCount     int     `json:"missing_data_count"`
			NewestUsageUpdatedAt *string `json:"newest_usage_updated_at"`
			OldestUsageUpdatedAt *string `json:"oldest_usage_updated_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Zero(t, payload.Data.TotalUSD)
	require.Zero(t, payload.Data.AccountCount)
	require.Zero(t, payload.Data.MissingDataCount)
	require.Nil(t, payload.Data.NewestUsageUpdatedAt)
	require.Nil(t, payload.Data.OldestUsageUpdatedAt)
}
