package model

import (
	"time"
)

const (
	CredentialOAuth  = "oauth"
	CredentialAPIKey = "api_key"

	AccountHealthHealthy = "healthy"
)

type Account struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// Internal identifier used to de-duplicate OAuth identities. It is intentionally
	// not exposed by the management API.
	ClientID         string `json:"-" gorm:"uniqueIndex;not null"`
	CredentialType   string `json:"credential_type" gorm:"default:'oauth';index"`
	OAuthProvider    string `json:"oauth_provider,omitempty" gorm:"index"`
	OAuthEmail       string `json:"oauth_email,omitempty" gorm:"index"`
	OAuthUserID      string `json:"-" gorm:"index"`
	OAuthTenantID    string `json:"-"`
	OAuthAnonymousID string `json:"-"`
	AccessToken      string `json:"-" gorm:"type:text"`
	RefreshToken     string `json:"-" gorm:"type:text"`
	// APIKey is encrypted at rest by internal/secret. It is never serialized
	// through the management API and is only decrypted at the final request
	// authentication boundary.
	APIKey              string     `json:"-" gorm:"type:text"`
	CredentialRevision  uint64     `json:"-" gorm:"default:1"`
	TokenExpiresAt      time.Time  `json:"-"`
	HealthRevision      uint64     `json:"-" gorm:"default:1;not null"`
	HealthState         string     `json:"health_state" gorm:"default:'healthy';index"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty" gorm:"index"`
	LastErrorClass      string     `json:"last_error_class,omitempty"`
	LastErrorAt         *time.Time `json:"-"`
	FailureCount        int        `json:"failure_count" gorm:"default:0"`
	ReauthRequired      bool       `json:"reauth_required" gorm:"default:false;index"`
	RefreshLeaseID      string     `json:"-" gorm:"index"`
	RefreshLeaseUntil   *time.Time `json:"-" gorm:"index"`
	QuotaLimit          float64    `json:"quota_limit" gorm:"default:0"`
	QuotaLimitAvailable bool       `json:"quota_limit_available" gorm:"default:false"`
	UsageDataAvailable  bool       `json:"usage_data_available" gorm:"default:false"`
	QuotaUsed           float64    `json:"-" gorm:"default:0"`
	CreditRefreshTime   time.Time  `json:"credit_refresh_time"` // 积分刷新时间（来自Zen-Pricing-Period-End）
	DailyUsed           float64    `json:"daily_used" gorm:"default:0"`
	TotalUsed           float64    `json:"total_used" gorm:"default:0"`
	// UsageCredits* is the independent usage-based billing snapshot. It must
	// never be confused with the Premium LLM-call quota fields above.
	UsageCreditsOperationCredits int64  `json:"-" gorm:"default:0"`
	UsageCreditsTurns            int64  `json:"-" gorm:"default:0"`
	UsageCreditsOperationExists  bool   `json:"-" gorm:"default:false"`
	UsageCreditsConsumed         int64  `json:"-" gorm:"default:0"`
	UsageCreditsBudget           int64  `json:"-" gorm:"default:0"`
	UsageCreditsRemaining        int64  `json:"-" gorm:"default:0"`
	UsageCreditsAvailable        bool   `json:"-" gorm:"default:false"`
	UsageCreditsStatus           string `json:"-" gorm:"default:'unknown';index"`
	// UsageCreditsSource identifies which endpoint supplied the account-level balance.
	// An empty value means the provenance is unknown (for pre-v5 rows).
	UsageCreditsSource             string     `json:"-" gorm:"default:''"`
	UsageCreditsUpdatedAt          *time.Time `json:"-"`
	UsageCreditsPeriodEnd          *time.Time `json:"-"`
	UsageCreditsLastAttemptAt      *time.Time `json:"-"`
	UsageCreditsOperationID        string     `json:"-" gorm:"index"`
	UsageCreditsCredentialRevision uint64     `json:"-" gorm:"default:0"`
	UsageCreditsQueryRevision      uint64     `json:"-" gorm:"default:0"`
	UsageCreditsLeaseID            string     `json:"-" gorm:"index"`
	UsageCreditsLeaseUntil         *time.Time `json:"-" gorm:"index"`
	LastResetDate                  string     `json:"last_reset_date"`
	LastUsed                       time.Time  `json:"last_used"`
	CreatedAt                      time.Time  `json:"created_at"`
	UpdatedAt                      time.Time  `json:"updated_at"`
}

// OAuthSession persists a short-lived PKCE login so callbacks survive process
// restarts. CodeVerifier is encrypted before persistence. ClaimID is the
// ownership token; ClaimedAt is a renewable lease timestamp, and ConsumedAt
// makes successful callbacks permanently one-time.
type OAuthSession struct {
	ID           uint       `json:"-" gorm:"primaryKey"`
	State        string     `json:"-" gorm:"uniqueIndex;not null"`
	CodeVerifier string     `json:"-" gorm:"type:text"`
	AnonymousID  string     `json:"-" gorm:"not null"`
	Origin       string     `json:"-" gorm:"not null"`
	RedirectURL  string     `json:"-" gorm:"not null"`
	ExpiresAt    time.Time  `json:"-" gorm:"index;not null"`
	ClaimID      string     `json:"-" gorm:"index"`
	ClaimedAt    *time.Time `json:"-" gorm:"index"`
	ConsumedAt   *time.Time `json:"-" gorm:"index"`
	CreatedAt    time.Time  `json:"-"`
}

// AdminSession stores only a hash of the random session nonce. Keeping this
// server-side record makes logout and expiry effective across instances while
// the encrypted browser cookie remains opaque and HttpOnly.
type AdminSession struct {
	NonceHash string    `json:"-" gorm:"primaryKey;size:64"`
	ExpiresAt time.Time `json:"-" gorm:"index;not null"`
	CreatedAt time.Time `json:"-"`
}

// SchedulerLease is a small database-backed CAS record. It prevents two
// service instances from running the same daily maintenance job.
type SchedulerLease struct {
	Name        string    `json:"-" gorm:"primaryKey"`
	Holder      string    `json:"-"`
	LeaseUntil  time.Time `json:"-" gorm:"index"`
	LastRunDate string    `json:"-" gorm:"index"`
	UpdatedAt   time.Time `json:"-"`
}
