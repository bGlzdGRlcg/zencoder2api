package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/secret"
	"zencoder-2api/internal/service"
)

type AccountHandler struct{}

func NewAccountHandler() *AccountHandler { return &AccountHandler{} }

type accountListItem struct {
	model.Account
	UsageBasedCredits service.UsageBasedCreditsDTO `json:"usage_based_credits"`
}

func (h *AccountHandler) List(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}
	size, err := strconv.Atoi(c.DefaultQuery("size", "10"))
	if err != nil || size < 1 || size > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "size must be between 1 and 100"})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	var accounts []model.Account
	var total int64
	query := db.Model(&model.Account{}).Where("(credential_type = ? AND access_token != '') OR (credential_type = ? AND api_key != '')", model.CredentialOAuth, model.CredentialAPIKey)
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query accounts"})
		return
	}
	if err := query.Offset((page - 1) * size).Limit(size).Order("id desc").Find(&accounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query accounts"})
		return
	}

	var stats struct {
		TotalAccounts int64   `json:"total_accounts"`
		TodayUsage    float64 `json:"today_usage"`
		TotalUsage    float64 `json:"total_usage"`
	}
	base := db.Model(&model.Account{}).Where("(credential_type = ? AND access_token != '') OR (credential_type = ? AND api_key != '')", model.CredentialOAuth, model.CredentialAPIKey)
	if err := base.Count(&stats.TotalAccounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query account statistics"})
		return
	}
	if err := base.Select("COALESCE(SUM(daily_used), 0)").Scan(&stats.TodayUsage).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query account statistics"})
		return
	}
	if err := base.Select("COALESCE(SUM(total_used), 0)").Scan(&stats.TotalUsage).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query account statistics"})
		return
	}

	items := make([]accountListItem, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, accountListItem{
			Account:           account,
			UsageBasedCredits: service.CreditSnapshotForAccount(account),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"total": total,
		"page":  page,
		"size":  size,
		"stats": stats,
	})
}

type BatchDeleteRequest struct {
	IDs       []uint `json:"ids"`
	DeleteAll bool   `json:"delete_all"`
}

func (h *AccountHandler) BatchDelete(c *gin.Context) {
	var req BatchDeleteRequest
	if err := decodeStrictJSON(c, &req, 64<<10); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid delete request"})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	var deletedCount int64
	if req.DeleteAll {
		var accounts []model.Account
		if err := db.Select("id").Find(&accounts).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query accounts"})
			return
		}
		deleteResult := db.Where("1 = 1").Delete(&model.Account{})
		if deleteResult.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete accounts"})
			return
		}
		deletedCount = deleteResult.RowsAffected
		for _, account := range accounts {
			service.InvalidateAccount(account.ID)
		}
	} else {
		if len(req.IDs) == 0 || len(req.IDs) > 1000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ids must contain between 1 and 1000 items"})
			return
		}
		seen := make(map[uint]struct{}, len(req.IDs))
		for _, id := range req.IDs {
			if id == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "ids must be positive"})
				return
			}
			seen[id] = struct{}{}
		}
		ids := make([]uint, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		deleteResult := db.Where("id IN ?", ids).Delete(&model.Account{})
		if deleteResult.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete accounts"})
			return
		}
		deletedCount = deleteResult.RowsAffected
		for _, id := range ids {
			service.InvalidateAccount(id)
		}
	}
	service.RefreshAccountPool()
	c.JSON(http.StatusOK, gin.H{"message": "批量删除成功", "deleted_count": deletedCount})
}

func (h *AccountHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}
	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	result := db.Delete(&model.Account{}, uint(id))
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete account"})
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return
	}
	service.InvalidateAccount(uint(id))
	service.RefreshAccountPool()
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

type apiKeyRequest struct {
	APIKey string `json:"api_key"`
	Name   string `json:"name"`
}

func apiKeyClientID(key string) (string, error) {
	index, err := secret.BlindIndex("zencoder-api-key", key)
	if err != nil {
		return "", err
	}
	return "api-key-" + index, nil
}

// CreateAPIKey stores a Zen CLI zencoder-api-key credential. The key is
// accepted only through this explicit authenticated endpoint; it is never
// imported from local analysis artifacts or returned in the response.
func (h *AccountHandler) CreateAPIKey(c *gin.Context) {
	var req apiKeyRequest
	if err := decodeStrictJSON(c, &req, 64<<10); err != nil || len(strings.TrimSpace(req.APIKey)) < 16 || len(req.APIKey) > 4096 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key must contain between 16 and 4096 bytes"})
		return
	}
	key := strings.TrimSpace(req.APIKey)
	clientID, err := apiKeyClientID(key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to index API key"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "API key"
	}
	encrypted, err := secret.Encrypt(key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt API key"})
		return
	}
	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	values := map[string]interface{}{
		"client_id": clientID, "credential_type": model.CredentialAPIKey, "api_key": encrypted,
		"o_auth_provider": "", "o_auth_email": req.Name, "o_auth_user_id": "",
		"o_auth_tenant_id": "", "o_auth_anonymous_id": "", "access_token": "",
		"refresh_token": "", "token_expires_at": time.Time{},
		"health_state": model.AccountHealthHealthy, "cooldown_until": gorm.Expr("NULL"),
		"last_error_class": "", "last_error_at": gorm.Expr("NULL"), "failure_count": 0,
		"reauth_required": false, "credential_revision": gorm.Expr("credential_revision + 1"), "updated_at": time.Now(),
	}
	var accountID uint
	var conflict bool
	err = db.Transaction(func(tx *gorm.DB) error {
		var existing model.Account
		findErr := tx.Where("client_id = ?", clientID).First(&existing).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			created := &model.Account{
				ClientID: clientID, CredentialType: model.CredentialAPIKey,
				APIKey: encrypted, CredentialRevision: 1,
				OAuthEmail: req.Name, HealthState: model.AccountHealthHealthy,
			}
			if err := tx.Create(created).Error; err != nil {
				return err
			}
			accountID = created.ID
			return nil
		}
		if findErr != nil {
			return findErr
		}
		if existing.CredentialType != model.CredentialAPIKey {
			conflict = true
			return nil
		}
		result := tx.Model(&model.Account{}).
			Where("id = ? AND client_id = ? AND credential_type = ?", existing.ID, clientID, model.CredentialAPIKey).
			Updates(values)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("API-key credential changed during update")
		}
		accountID = existing.ID
		return nil
	})
	if conflict {
		c.JSON(http.StatusConflict, gin.H{"error": "credential identity already belongs to an OAuth account"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store API key"})
		return
	}
	var account model.Account
	if err := db.First(&account, accountID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load API key account"})
		return
	}
	service.RefreshAccountPool()
	service.TriggerAccountCreditsRefresh(c.Request.Context(), &account, "")
	c.JSON(http.StatusCreated, account)
}

// RotateAPIKey replaces only the encrypted API-key field and preserves usage
// counters and identity. OAuth accounts cannot be silently converted.
func (h *AccountHandler) RotateAPIKey(c *gin.Context) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}
	var req apiKeyRequest
	if err := decodeStrictJSON(c, &req, 64<<10); err != nil || len(strings.TrimSpace(req.APIKey)) < 16 || len(req.APIKey) > 4096 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key must contain between 16 and 4096 bytes"})
		return
	}
	key := strings.TrimSpace(req.APIKey)
	clientID, err := apiKeyClientID(key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to index API key"})
		return
	}
	encrypted, err := secret.Encrypt(key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt API key"})
		return
	}
	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	updates := map[string]interface{}{
		"client_id": clientID, "api_key": encrypted, "health_state": model.AccountHealthHealthy,
		"cooldown_until": gorm.Expr("NULL"), "last_error_class": "", "last_error_at": gorm.Expr("NULL"),
		"failure_count": 0, "reauth_required": false, "credential_revision": gorm.Expr("credential_revision + 1"), "updated_at": time.Now(),
		"usage_credits_operation_credits": 0, "usage_credits_turns": 0,
		"usage_credits_operation_exists": false, "usage_credits_consumed": 0,
		"usage_credits_budget": 0, "usage_credits_remaining": 0,
		"usage_credits_available": false, "usage_credits_status": service.UsageCreditsStateUnknown, "usage_credits_source": "",
		"usage_credits_updated_at": gorm.Expr("NULL"), "usage_credits_period_end": gorm.Expr("NULL"), "usage_credits_last_attempt_at": gorm.Expr("NULL"),
		"usage_credits_operation_id": "", "usage_credits_credential_revision": 0,
		"usage_credits_query_revision": gorm.Expr("usage_credits_query_revision + 1"),
		"usage_credits_lease_id":       "", "usage_credits_lease_until": gorm.Expr("NULL"),
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		updates["o_auth_email"] = name
	}
	var conflict, updated bool
	err = db.Transaction(func(tx *gorm.DB) error {
		var target model.Account
		if err := tx.Select("id").Where("id = ? AND credential_type = ?", id, model.CredentialAPIKey).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var duplicateCount int64
		if err := tx.Model(&model.Account{}).Where("client_id = ? AND id != ?", clientID, id).Count(&duplicateCount).Error; err != nil {
			return err
		}
		if duplicateCount > 0 {
			conflict = true
			return nil
		}
		result := tx.Model(&model.Account{}).
			Where("id = ? AND credential_type = ?", id, model.CredentialAPIKey).
			Updates(updates)
		updated = result.RowsAffected == 1
		return result.Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate API key"})
		return
	}
	if conflict {
		c.JSON(http.StatusConflict, gin.H{"error": "API key is already assigned to another account"})
		return
	}
	if !updated {
		c.JSON(http.StatusNotFound, gin.H{"error": "API-key account not found"})
		return
	}
	service.InvalidateAccount(uint(id))
	service.RefreshAccountPool()
	service.TriggerAccountCreditsRefresh(c.Request.Context(), &model.Account{ID: uint(id)}, "")
	c.JSON(http.StatusOK, gin.H{"message": "API key rotated"})
}
