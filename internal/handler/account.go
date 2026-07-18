package handler

import (
	"net/http"
	"strconv"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/model"
	"zencoder-2api/internal/service"

	"github.com/gin-gonic/gin"
)

type AccountHandler struct{}

func NewAccountHandler() *AccountHandler {
	return &AccountHandler{}
}

func (h *AccountHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "10"))

	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 10
	}

	var accounts []model.Account
	var total int64

	// Account status is a legacy internal field. The management API always
	// shows every account instead of splitting users into status categories.
	query := database.GetDB().Model(&model.Account{}).
		Where("credential_type = ? AND access_token != ''", model.CredentialOAuth)

	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	offset := (page - 1) * size
	if err := query.Offset(offset).Limit(size).Order("id desc").Find(&accounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Calculate Stats
	var stats struct {
		TotalAccounts int64   `json:"total_accounts"`
		TodayUsage    float64 `json:"today_usage"`
		TotalUsage    float64 `json:"total_usage"`
	}

	db := database.GetDB()

	db.Model(&model.Account{}).Where("credential_type = ? AND access_token != ''", model.CredentialOAuth).Count(&stats.TotalAccounts)

	// Dashboard usage is intentionally local: one request adds the model's
	// multiplier, regardless of whether the gateway returns quota metadata.
	db.Model(&model.Account{}).Where("credential_type = ? AND access_token != ''", model.CredentialOAuth).Select("COALESCE(SUM(daily_used), 0)").Scan(&stats.TodayUsage)
	db.Model(&model.Account{}).Where("credential_type = ? AND access_token != ''", model.CredentialOAuth).Select("COALESCE(SUM(total_used), 0)").Scan(&stats.TotalUsage)

	statsMap := map[string]interface{}{
		"total_accounts": stats.TotalAccounts,
		"today_usage":    stats.TodayUsage,
		"total_usage":    stats.TotalUsage,
	}

	c.JSON(http.StatusOK, gin.H{
		"items": accounts,
		"total": total,
		"page":  page,
		"size":  size,
		"stats": statsMap,
	})
}

type BatchDeleteRequest struct {
	IDs       []uint `json:"ids"`
	DeleteAll bool   `json:"delete_all"`
}

// BatchDelete 批量删除账号
func (h *AccountHandler) BatchDelete(c *gin.Context) {
	var req BatchDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var deletedCount int64

	if req.DeleteAll {
		result := database.GetDB().Delete(&model.Account{})
		if result.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": result.Error.Error()})
			return
		}

		deletedCount = result.RowsAffected

	} else {
		// 删除选中的账号
		if len(req.IDs) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "未选择要删除的账号"})
			return
		}

		// 执行删除操作
		result := database.GetDB().Where("id IN ?", req.IDs).Delete(&model.Account{})
		if result.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": result.Error.Error()})
			return
		}

		deletedCount = result.RowsAffected
	}

	service.RefreshAccountPool()

	c.JSON(http.StatusOK, gin.H{
		"message":       "批量删除成功",
		"deleted_count": deletedCount,
	})
}

func (h *AccountHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if err := database.GetDB().Delete(&model.Account{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	service.RefreshAccountPool()
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}
