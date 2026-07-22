package handler

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"zencoder-2api/internal/service"
)

const maxManualCreditRefreshAccounts = 1000

type creditsRefreshRequest struct {
	IDs        []uint `json:"ids"`
	RefreshAll bool   `json:"refresh_all"`
}

type creditsRefreshItem struct {
	ID                uint                         `json:"id"`
	UsageBasedCredits service.UsageBasedCreditsDTO `json:"usage_based_credits"`
}

// RefreshCredits refreshes one or more account snapshots without changing
// account health. Partial upstream failures are returned per account so a
// healthy credential is never disabled by the optional billing endpoint.
func (h *AccountHandler) RefreshCredits(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request creditsRefreshRequest
	if err := decodeStrictJSON(c, &request, 64<<10); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credit refresh request"})
		return
	}
	if request.RefreshAll == (len(request.IDs) > 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provide either ids or refresh_all"})
		return
	}
	if len(request.IDs) > maxManualCreditRefreshAccounts {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many account ids"})
		return
	}
	for _, id := range request.IDs {
		if id == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ids must contain positive account ids"})
			return
		}
	}

	ids := uniquePositiveAccountIDs(request.IDs)
	if !request.RefreshAll && len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids must contain positive account ids"})
		return
	}
	results, err := service.RefreshAccountsCredits(c.Request.Context(), ids, request.RefreshAll)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to refresh account credits"})
		return
	}

	items := make([]creditsRefreshItem, 0, len(results))
	refreshed, skipped, failed := 0, 0, 0
	for _, result := range results {
		items = append(items, creditsRefreshItem{ID: result.AccountID, UsageBasedCredits: result.Snapshot})
		switch {
		case result.Snapshot.State == service.UsageCreditsStateRefreshing:
			skipped++
		case result.Snapshot.State == service.UsageCreditsStateReady:
			refreshed++
		case result.Err != nil || result.Snapshot.State == service.UsageCreditsStateError:
			failed++
		default:
			skipped++
		}
	}
	requested := len(ids)
	if request.RefreshAll {
		requested = len(results)
	} else if missing := requested - len(results); missing > 0 {
		skipped += missing
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	c.JSON(http.StatusOK, gin.H{
		"items":     items,
		"requested": requested,
		"refreshed": refreshed,
		"skipped":   skipped,
		"failed":    failed,
	})
}

func uniquePositiveAccountIDs(values []uint) []uint {
	seen := make(map[uint]struct{}, len(values))
	ids := make([]uint, 0, len(values))
	for _, id := range values {
		if id == 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
