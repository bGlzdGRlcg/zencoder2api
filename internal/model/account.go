package model

import (
	"time"
)

const CredentialOAuth = "oauth"

type Account struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// Internal identifier used to de-duplicate OAuth identities. It is intentionally
	// not exposed by the management API.
	ClientID            string    `json:"-" gorm:"uniqueIndex;not null"`
	CredentialType      string    `json:"credential_type" gorm:"default:'oauth';index"`
	OAuthProvider       string    `json:"oauth_provider,omitempty" gorm:"index"`
	OAuthEmail          string    `json:"oauth_email,omitempty" gorm:"index"`
	OAuthUserID         string    `json:"-" gorm:"index"`
	OAuthTenantID       string    `json:"-"`
	OAuthAnonymousID    string    `json:"-"`
	AccessToken         string    `json:"-" gorm:"type:text"`
	RefreshToken        string    `json:"-" gorm:"type:text"`
	TokenExpiresAt      time.Time `json:"-"`
	QuotaLimit          float64   `json:"quota_limit" gorm:"default:0"`
	QuotaLimitAvailable bool      `json:"quota_limit_available" gorm:"default:false"`
	UsageDataAvailable  bool      `json:"usage_data_available" gorm:"default:false"`
	QuotaUsed           float64   `json:"-" gorm:"default:0"`
	CreditRefreshTime   time.Time `json:"credit_refresh_time"` // 积分刷新时间（来自Zen-Pricing-Period-End）
	DailyUsed           float64   `json:"daily_used" gorm:"default:0"`
	TotalUsed           float64   `json:"total_used" gorm:"default:0"`
	LastResetDate       string    `json:"last_reset_date"`
	LastUsed            time.Time `json:"last_used"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}
