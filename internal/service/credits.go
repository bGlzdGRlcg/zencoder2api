package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"zencoder-2api/internal/database"
	"zencoder-2api/internal/logging"
	"zencoder-2api/internal/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	UsageCreditsStateUnknown     = "unknown"
	UsageCreditsStateRefreshing  = "refreshing"
	UsageCreditsStateReady       = "ready"
	UsageCreditsStateNoOperation = "no_operation"
	UsageCreditsStateUnsupported = "unsupported"
	UsageCreditsStateError       = "error"
	UsageCreditsStateStale       = "stale"

	creditRefreshQueueSize      = 64
	maxCreditResponseBytes      = 1 << 20
	creditCleanupTimeout        = 5 * time.Second
	usageCreditsSourceTokens    = "tokens"
	usageCreditsSourceOperation = "operation"
)

var (
	creditRequestTimeout        = 12 * time.Second
	creditRefreshLeaseDuration  = 30 * time.Second
	creditRefreshBusyRetryDelay = 250 * time.Millisecond
	creditRefreshWorkerMu       sync.Mutex
	creditRefreshQueue          chan creditRefreshJob
	creditRefreshPending        map[uint]struct{}
	creditRefreshRunning        map[uint]struct{}
	creditRefreshFollowup       map[uint]struct{}
	creditRefreshWorkerCancel   context.CancelFunc
	creditRefreshWorkerDone     chan struct{}
)

var (
	errCreditRefreshBusy       = errors.New("usage-based credit refresh is already running")
	errCreditRefreshSuperseded = errors.New("usage-based credit refresh was superseded")
)

// UsageBasedCreditsDTO is the management-safe usage-based billing snapshot.
// Pricing-period quota fields and internal operation IDs are deliberately absent.
type UsageBasedCreditsDTO struct {
	State            string     `json:"state"`
	Revision         uint64     `json:"revision"`
	OperationCredits int64      `json:"operation_credits"`
	OperationExists  bool       `json:"operation_exists"`
	Turns            int64      `json:"turns"`
	Consumed         int64      `json:"consumed"`
	Budget           int64      `json:"budget"`
	Remaining        int64      `json:"remaining"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	PeriodEnd        *time.Time `json:"period_end,omitempty"`
}

type CreditRefreshResult struct {
	AccountID       uint                 `json:"account_id"`
	Snapshot        UsageBasedCreditsDTO `json:"usage_based_credits"`
	OperationID     string               `json:"-"`
	Err             error                `json:"-"`
	attemptRevision uint64
}

type creditRefreshJob struct {
	accountID uint
}

type accountCreditsRefreshBody struct {
	io.ReadCloser
	account     *model.Account
	ctx         context.Context
	operationID string
	once        sync.Once
}

func (body *accountCreditsRefreshBody) queueRefresh() {
	body.once.Do(func() {
		if body.operationID != "" {
			// The response body is the completion boundary. A refresh that started
			// while the upstream request was still running must be superseded too.
			completeAccountCreditsOperation(body.ctx, body.account, body.operationID)
		}
		enqueueAccountCreditsRefresh(body.account)
	})
}

func (body *accountCreditsRefreshBody) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if errors.Is(err, io.EOF) {
		body.queueRefresh()
	}
	return n, err
}

func (body *accountCreditsRefreshBody) Close() error {
	err := body.ReadCloser.Close()
	body.queueRefresh()
	return err
}

func deferAccountCreditsRefresh(ctx context.Context, account *model.Account, resp *http.Response, operationID string) {
	if account == nil {
		return
	}
	if normalized, ok := validCreditOperationID(operationID); ok {
		operationID = normalized
		rememberAccountCreditsOperation(ctx, account, operationID)
	} else {
		operationID = ""
	}
	if resp == nil || resp.Body == nil {
		enqueueAccountCreditsRefresh(account)
		return
	}
	resp.Body = &accountCreditsRefreshBody{
		ReadCloser:  resp.Body,
		account:     account,
		ctx:         ctx,
		operationID: operationID,
	}
}

// StartUsageCreditsWorker starts the bounded asynchronous refresh worker. The
// caller owns the returned cancellation function and should defer it during
// process shutdown. Keeping startup explicit prevents tests and short-lived
// tools from spawning an unowned goroutine.
func StartUsageCreditsWorker() context.CancelFunc {
	creditRefreshWorkerMu.Lock()
	defer creditRefreshWorkerMu.Unlock()
	if creditRefreshWorkerCancel != nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	queue := make(chan creditRefreshJob, creditRefreshQueueSize)
	done := make(chan struct{})
	creditRefreshQueue = queue
	creditRefreshPending = make(map[uint]struct{})
	creditRefreshRunning = make(map[uint]struct{})
	creditRefreshFollowup = make(map[uint]struct{})
	creditRefreshWorkerCancel = cancel
	creditRefreshWorkerDone = done
	go runUsageCreditsWorker(ctx, queue, done)
	return func() { stopUsageCreditsWorker(cancel, done) }
}

// StopUsageCreditsWorker stops the worker and waits for an in-flight query to
// finish. It is safe to call more than once.
func StopUsageCreditsWorker() {
	creditRefreshWorkerMu.Lock()
	cancel := creditRefreshWorkerCancel
	done := creditRefreshWorkerDone
	creditRefreshWorkerCancel = nil
	creditRefreshWorkerDone = nil
	creditRefreshQueue = nil
	creditRefreshPending = nil
	creditRefreshRunning = nil
	creditRefreshFollowup = nil
	creditRefreshWorkerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func stopUsageCreditsWorker(cancel context.CancelFunc, done chan struct{}) {
	creditRefreshWorkerMu.Lock()
	if creditRefreshWorkerDone == done {
		creditRefreshWorkerCancel = nil
		creditRefreshWorkerDone = nil
		creditRefreshQueue = nil
		creditRefreshPending = nil
		creditRefreshRunning = nil
		creditRefreshFollowup = nil
	}
	creditRefreshWorkerMu.Unlock()
	cancel()
	if done != nil {
		<-done
	}
}

type operationCreditsResponse struct {
	OperationID              string          `json:"operationId"`
	TotalOperationCreditsRaw json.RawMessage `json:"totalOperationCredits"`
	TurnsRaw                 json.RawMessage `json:"turns"`
	TotalConsumedRaw         json.RawMessage `json:"totalUserConsumedCredits"`
	TotalBudgetRaw           json.RawMessage `json:"totalUserBudget"`
}

type parsedOperationCredits struct {
	OperationID      string
	OperationCredits int64
	Turns            int64
	Consumed         int64
	Budget           int64
	Remaining        int64
}

type tokenCreditsResponse struct {
	PeriodEndRaw        json.RawMessage `json:"periodEnd"`
	RemainingRaw        json.RawMessage `json:"remaining"`
	TotalConsumedByUser json.RawMessage `json:"totalConsumedByUser"`
	TotalUserBudget     json.RawMessage `json:"totalUserBudget"`
}

type parsedTokenCredits struct {
	Consumed  int64
	Budget    int64
	Remaining int64
	PeriodEnd *time.Time
}

type creditHTTPError struct {
	status int
	state  string
}

func (e *creditHTTPError) Error() string {
	return fmt.Sprintf("fetch Zencoder usage-based credits: HTTP %d", e.status)
}

func CreditSnapshotForAccount(account model.Account) UsageBasedCreditsDTO {
	state := strings.TrimSpace(account.UsageCreditsStatus)
	if state == "" {
		state = UsageCreditsStateUnknown
	}
	if account.UsageCreditsAvailable {
		switch {
		case state == UsageCreditsStateReady && !usageCreditsSnapshotFresh(account, time.Now()):
			state = UsageCreditsStateStale
		case state == UsageCreditsStateNoOperation || state == UsageCreditsStateUnsupported:
			state = UsageCreditsStateStale
		}
	}
	return UsageBasedCreditsDTO{
		State:            state,
		Revision:         account.UsageCreditsQueryRevision,
		OperationCredits: account.UsageCreditsOperationCredits,
		OperationExists:  account.UsageCreditsOperationExists,
		Turns:            account.UsageCreditsTurns,
		Consumed:         account.UsageCreditsConsumed,
		Budget:           account.UsageCreditsBudget,
		Remaining:        account.UsageCreditsRemaining,
		UpdatedAt:        account.UsageCreditsUpdatedAt,
		PeriodEnd:        account.UsageCreditsPeriodEnd,
	}
}

// RefreshAccountCredits synchronously refreshes the account-level usage balance.
// The tokens endpoint does not require a completed operation. The operation
// endpoint remains a compatibility fallback for older gateways.
func RefreshAccountCredits(ctx context.Context, accountID uint) (CreditRefreshResult, error) {
	result := CreditRefreshResult{AccountID: accountID}
	db := database.GetDB()
	if db == nil {
		return result, errDatabaseUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var account model.Account
	if err := db.WithContext(ctx).First(&account, accountID).Error; err != nil {
		return result, err
	}
	result.OperationID = account.UsageCreditsOperationID

	oauthRecoveryUsed := false
	tokenResult, tokenErr := refreshAccountCreditsFromTokens(ctx, &account)
	if account.CredentialType == model.CredentialOAuth && creditHTTPStatus(tokenErr) == http.StatusUnauthorized {
		oauthRecoveryUsed = true
		if refreshErr := forceOAuthRefreshWithoutHealthMutation(ctx, &account); refreshErr != nil {
			return tokenResult, refreshErr
		}
		if err := db.WithContext(ctx).First(&account, account.ID).Error; err != nil {
			return tokenResult, err
		}
		tokenResult, tokenErr = refreshAccountCreditsFromTokens(ctx, &account)
	}
	if tokenErr == nil || errors.Is(tokenErr, errCreditRefreshBusy) || errors.Is(tokenErr, errCreditRefreshSuperseded) {
		return tokenResult, tokenErr
	}
	// Older gateway builds may not expose /tokens. Keep the operation endpoint
	// as a best-effort compatibility path, but never make it the primary source
	// of account-level balance data.
	var latest model.Account
	if err := db.WithContext(ctx).First(&latest, account.ID).Error; err == nil {
		account = latest
	}
	if !shouldFallbackToOperationCredits(tokenErr) {
		return tokenResult, tokenErr
	}
	if operationID, ok := validCreditOperationID(account.UsageCreditsOperationID); ok {
		// A gateway that does not expose the account-level endpoint may still
		// expose the legacy operation endpoint. Preserve its error if that
		// fallback is also unavailable; do not mislabel an existing operation
		// as "no operation".
		operationResult, operationErr := refreshAccountCreditsForOperation(ctx, account, operationID, account.CredentialRevision)
		if account.CredentialType != model.CredentialOAuth || oauthRecoveryUsed || creditHTTPStatus(operationErr) != http.StatusUnauthorized {
			return operationResult, operationErr
		}
		if refreshErr := forceOAuthRefreshWithoutHealthMutation(ctx, &account); refreshErr != nil {
			return operationResult, refreshErr
		}
		if err := db.WithContext(ctx).First(&account, account.ID).Error; err != nil {
			return operationResult, err
		}
		operationID, ok = validCreditOperationID(account.UsageCreditsOperationID)
		if !ok {
			return operationResult, errCreditRefreshSuperseded
		}
		return refreshAccountCreditsForOperation(ctx, account, operationID, account.CredentialRevision)
	}
	// Older gateways may not expose /tokens. If no operation exists yet,
	// preserve that distinction instead of reporting an optional endpoint
	// failure; the next completed request will attach an operation ID.
	if _, marked, markErr := markCreditNoOperation(ctx, account, tokenResult.attemptRevision); markErr != nil {
		return tokenResult, markErr
	} else if marked {
		tokenResult.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, account)
		return tokenResult, nil
	}
	return tokenResult, tokenErr
}

func shouldFallbackToOperationCredits(err error) bool {
	var httpErr *creditHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.status == http.StatusNotFound || httpErr.status == http.StatusMethodNotAllowed || httpErr.status == http.StatusNotImplemented
	}
	return false
}

func creditHTTPStatus(err error) int {
	var httpErr *creditHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.status
	}
	return 0
}

func markCreditNoOperation(ctx context.Context, account model.Account, attemptRevision uint64) (*time.Time, bool, error) {
	db := database.GetDB()
	if db == nil {
		return nil, false, errDatabaseUnavailable
	}
	now := time.Now().UTC()
	result := db.WithContext(ctx).Model(&model.Account{}).
		Where("id = ? AND credential_revision = ? AND usage_credits_query_revision = ? AND (usage_credits_operation_id IS NULL OR usage_credits_operation_id = '')",
			account.ID, account.CredentialRevision, attemptRevision).
		Updates(map[string]interface{}{
			"usage_credits_status":            UsageCreditsStateNoOperation,
			"usage_credits_operation_credits": 0,
			"usage_credits_turns":             0,
			"usage_credits_operation_exists":  false,
			"usage_credits_last_attempt_at":   now,
			"usage_credits_query_revision":    gorm.Expr("usage_credits_query_revision + 1"),
		})
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 1 {
		updatePoolCredits(account.ID)
	}
	return &now, result.RowsAffected == 1, nil
}

// RefreshAccountsCredits refreshes selected accounts, or every credential when
// all is true. Per-account failures are reported in each result and do not stop
// other accounts.
func RefreshAccountsCredits(ctx context.Context, ids []uint, all bool) ([]CreditRefreshResult, error) {
	db := database.GetDB()
	if db == nil {
		return nil, errDatabaseUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	query := db.WithContext(ctx).Model(&model.Account{}).
		Select("id").
		Where("(credential_type = ? AND access_token != '') OR (credential_type = ? AND api_key != '')", model.CredentialOAuth, model.CredentialAPIKey)
	if !all {
		if len(ids) == 0 {
			return []CreditRefreshResult{}, nil
		}
		query = query.Where("id IN ?", ids)
	}
	var accounts []model.Account
	if err := query.Order("id ASC").Find(&accounts).Error; err != nil {
		return nil, err
	}
	results := make([]CreditRefreshResult, len(accounts))
	jobs := make(chan int)
	workers := len(accounts)
	if workers > 4 {
		workers = 4
	}
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				result, err := RefreshAccountCredits(ctx, accounts[index].ID)
				result.Err = err
				results[index] = result
			}
		}()
	}
	for index := range accounts {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			wait.Wait()
			return results[:index], ctx.Err()
		}
	}
	close(jobs)
	wait.Wait()
	return results, ctx.Err()
}

// TriggerAccountCreditsRefresh enqueues a bounded, non-blocking account-level
// balance refresh. A valid operation ID is retained only for compatibility
// with gateways that do not expose /tokens.
func TriggerAccountCreditsRefresh(ctx context.Context, account *model.Account, operationID string) {
	if account == nil || account.ID == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if normalized, ok := validCreditOperationID(operationID); ok {
		operationID = normalized
		rememberAccountCreditsOperation(ctx, account, operationID)
	} else {
		operationID = ""
	}
	enqueueAccountCreditsRefresh(account)
}

func rememberAccountCreditsOperation(ctx context.Context, account *model.Account, operationID string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return rememberCreditOperation(recordCtx, account, operationID)
}

func completeAccountCreditsOperation(ctx context.Context, account *model.Account, operationID string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return completeCreditOperation(recordCtx, account, operationID)
}

func enqueueAccountCreditsRefresh(account *model.Account) {
	if account == nil || account.ID == 0 {
		return
	}
	creditRefreshWorkerMu.Lock()
	queue := creditRefreshQueue
	if queue != nil {
		if creditRefreshPending == nil {
			creditRefreshPending = make(map[uint]struct{})
		}
		if creditRefreshRunning == nil {
			creditRefreshRunning = make(map[uint]struct{})
		}
		if creditRefreshFollowup == nil {
			creditRefreshFollowup = make(map[uint]struct{})
		}
		if _, running := creditRefreshRunning[account.ID]; running {
			// A request completed while this account was being refreshed. Keep
			// one follow-up job so the account snapshot cannot lag indefinitely.
			creditRefreshFollowup[account.ID] = struct{}{}
			creditRefreshWorkerMu.Unlock()
			return
		}
		if _, exists := creditRefreshPending[account.ID]; exists {
			creditRefreshWorkerMu.Unlock()
			return
		}
		creditRefreshPending[account.ID] = struct{}{}
	}
	creditRefreshWorkerMu.Unlock()
	if queue == nil {
		return
	}
	job := creditRefreshJob{accountID: account.ID}
	select {
	case queue <- job:
	default:
		creditRefreshWorkerMu.Lock()
		delete(creditRefreshPending, account.ID)
		creditRefreshWorkerMu.Unlock()
		logging.Warnf("Usage-based credit refresh queue is full; account_id=%d", account.ID)
	}
}

func beginCreditRefreshJob(accountID uint) {
	creditRefreshWorkerMu.Lock()
	defer creditRefreshWorkerMu.Unlock()
	if creditRefreshPending != nil {
		delete(creditRefreshPending, accountID)
	}
	if creditRefreshRunning == nil {
		creditRefreshRunning = make(map[uint]struct{})
	}
	creditRefreshRunning[accountID] = struct{}{}
}

func finishCreditRefreshJob(accountID uint) {
	creditRefreshWorkerMu.Lock()
	queue := creditRefreshQueue
	delete(creditRefreshRunning, accountID)
	_, followup := creditRefreshFollowup[accountID]
	if followup {
		delete(creditRefreshFollowup, accountID)
	}
	queueFollowup := followup && queue != nil
	if queueFollowup {
		if creditRefreshPending == nil {
			creditRefreshPending = make(map[uint]struct{})
		}
		creditRefreshPending[accountID] = struct{}{}
	}
	creditRefreshWorkerMu.Unlock()
	if !queueFollowup {
		return
	}
	select {
	case queue <- creditRefreshJob{accountID: accountID}:
	default:
		creditRefreshWorkerMu.Lock()
		if creditRefreshQueue == queue && creditRefreshPending != nil {
			delete(creditRefreshPending, accountID)
		}
		creditRefreshWorkerMu.Unlock()
		logging.Warnf("Usage-based credit follow-up queue is full; account_id=%d", accountID)
	}
}

func runUsageCreditsWorker(workerCtx context.Context, queue <-chan creditRefreshJob, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-workerCtx.Done():
			return
		case job := <-queue:
			if workerCtx.Err() != nil {
				return
			}
			beginCreditRefreshJob(job.accountID)
			ctx, cancel := context.WithTimeout(workerCtx, creditRequestTimeout+creditCleanupTimeout)
			var account model.Account
			db := database.GetDB()
			if db != nil && db.WithContext(ctx).First(&account, job.accountID).Error == nil {
				if _, err := RefreshAccountCredits(ctx, account.ID); errors.Is(err, errCreditRefreshBusy) {
					// A manual/scheduled refresh can own the database lease without
					// appearing in creditRefreshRunning. Preserve the request-complete
					// event and retry after that bounded query has a chance to finish.
					enqueueAccountCreditsRefresh(&account)
					timer := time.NewTimer(creditRefreshBusyRetryDelay)
					select {
					case <-timer.C:
					case <-ctx.Done():
						if !timer.Stop() {
							<-timer.C
						}
					}
				} else if err != nil && !errors.Is(err, errCreditRefreshSuperseded) {
					logging.Debugf("Usage-based credit refresh failed: account_id=%d error=%v", job.accountID, err)
				}
			}
			cancel()
			finishCreditRefreshJob(job.accountID)
		}
	}
}

func rememberCreditOperation(ctx context.Context, account *model.Account, operationID string) bool {
	return updateCreditOperation(ctx, account, operationID, "")
}

func completeCreditOperation(ctx context.Context, account *model.Account, operationID string) bool {
	return updateCreditOperation(ctx, account, "", operationID)
}

func updateCreditOperation(ctx context.Context, account *model.Account, operationID, expectedOperationID string) bool {
	db := database.GetDB()
	if db == nil {
		return false
	}
	query := db.WithContext(ctx).Model(&model.Account{}).
		Where("id = ? AND credential_revision = ?", account.ID, account.CredentialRevision)
	if expectedOperationID != "" {
		query = query.Where("usage_credits_operation_id = ?", expectedOperationID)
	}
	updates := map[string]interface{}{
		"usage_credits_operation_credits": 0,
		"usage_credits_turns":             0,
		"usage_credits_operation_exists":  false,
		// Keep the last account-level balance, but make it a fallback until
		// /tokens observes the completed request. The request account is a
		// value copy, so derive the visible state from the current row.
		"usage_credits_status": gorm.Expr(
			"CASE WHEN usage_credits_available THEN ? ELSE ? END",
			UsageCreditsStateStale, UsageCreditsStateUnknown,
		),
		// Every operation boundary supersedes an earlier balance query. Its
		// holder can no longer satisfy the revision+lease CAS below.
		"usage_credits_query_revision": gorm.Expr("usage_credits_query_revision + 1"),
		"usage_credits_lease_id":       "",
		"usage_credits_lease_until":    gorm.Expr("NULL"),
	}
	if operationID != "" {
		updates["usage_credits_operation_id"] = operationID
	}
	result := query.Updates(updates)
	if result.Error != nil {
		logging.Debugf("Update usage-based credit operation failed: account_id=%d error=%v", account.ID, result.Error)
		return false
	}
	if result.RowsAffected != 1 {
		return false
	}
	// Read back the row instead of publishing fields from the stale request copy.
	updatePoolCredits(account.ID)
	return true
}

func refreshAccountCreditsFromTokens(ctx context.Context, account *model.Account) (CreditRefreshResult, error) {
	result := CreditRefreshResult{AccountID: account.ID, OperationID: account.UsageCreditsOperationID}
	queryCtx, cancel := context.WithTimeout(ctx, creditRequestTimeout)
	defer cancel()
	req, err := newTokenCreditsRequest(queryCtx, account)
	if err != nil {
		markCreditAttemptWithoutLease(context.WithoutCancel(ctx), *account, "", UsageCreditsStateError)
		result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, *account)
		return result, err
	}

	holder := uuid.NewString()
	queryRevision, claimed, err := claimCreditRefreshLease(queryCtx, *account, "", holder)
	if err != nil {
		return result, err
	}
	if !claimed {
		result.Snapshot = loadCreditSnapshot(queryCtx, account.ID, *account)
		return result, errCreditRefreshBusy
	}
	result.attemptRevision = queryRevision
	defer releaseCreditRefreshLease(context.WithoutCancel(ctx), account.ID, holder)

	credits, err := fetchTokenCredits(req)
	if err != nil {
		if creditCredentialRevisionChanged(context.WithoutCancel(ctx), account.ID, account.CredentialRevision) {
			return result, errCreditRefreshSuperseded
		}
		state := UsageCreditsStateError
		var httpErr *creditHTTPError
		if errors.As(err, &httpErr) {
			state = httpErr.state
		}
		persistCreditAttempt(context.WithoutCancel(ctx), *account, "", holder, queryRevision, state, nil)
		result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, *account)
		return result, err
	}
	if err := persistTokenCreditSnapshot(context.WithoutCancel(ctx), *account, holder, queryRevision, credits); err != nil {
		if errors.Is(err, errCreditRefreshSuperseded) {
			// A superseded response must not restore the state from the
			// request-start account copy. Let the CAS update derive state from
			// the current database row instead.
			persistCreditAttempt(context.WithoutCancel(ctx), *account, "", holder, queryRevision, "", nil)
		}
		result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, *account)
		return result, err
	}
	result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, *account)
	return result, nil
}

func creditCredentialRevisionChanged(ctx context.Context, accountID uint, expected uint64) bool {
	db := database.GetDB()
	if db == nil {
		return false
	}
	var current model.Account
	if err := db.WithContext(ctx).Select("credential_revision").First(&current, accountID).Error; err != nil {
		return false
	}
	return current.CredentialRevision != expected
}

func refreshAccountCreditsForOperation(ctx context.Context, account model.Account, operationID string, expectedCredentialRevision uint64) (CreditRefreshResult, error) {
	result := CreditRefreshResult{AccountID: account.ID, OperationID: operationID}
	operationID, ok := validCreditOperationID(operationID)
	if !ok {
		result.Snapshot = CreditSnapshotForAccount(account)
		result.Snapshot.State = UsageCreditsStateNoOperation
		return result, nil
	}
	if expectedCredentialRevision != 0 && account.CredentialRevision != expectedCredentialRevision {
		result.Snapshot = CreditSnapshotForAccount(account)
		return result, errCreditRefreshSuperseded
	}
	if account.UsageCreditsOperationID != operationID {
		result.Snapshot = CreditSnapshotForAccount(account)
		return result, errCreditRefreshSuperseded
	}

	queryCtx, cancel := context.WithTimeout(ctx, creditRequestTimeout)
	defer cancel()
	req, err := newOperationCreditsRequest(queryCtx, &account, operationID)
	if err != nil {
		markCreditAttemptWithoutLease(queryCtx, account, operationID, UsageCreditsStateError)
		return result, err
	}

	holder := uuid.NewString()
	queryRevision, claimed, err := claimCreditRefreshLease(queryCtx, account, operationID, holder)
	if err != nil {
		return result, err
	}
	if !claimed {
		result.Snapshot = loadCreditSnapshot(queryCtx, account.ID, account)
		return result, errCreditRefreshBusy
	}
	defer releaseCreditRefreshLease(context.WithoutCancel(ctx), account.ID, holder)

	credits, err := fetchOperationCredits(req)
	if err != nil {
		state := UsageCreditsStateError
		var httpErr *creditHTTPError
		if errors.As(err, &httpErr) {
			state = httpErr.state
		}
		persistCreditAttempt(context.WithoutCancel(ctx), account, operationID, holder, queryRevision, state, nil)
		result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, account)
		return result, err
	}
	if credits.OperationID != operationID {
		if credits.OperationID == "" {
			// The endpoint is scoped by the operation ID in the request path;
			// older serializers may omit the optional response field.
			credits.OperationID = operationID
		} else {
			err := errors.New("zencoder usage-based credits response operation ID does not match request")
			persistCreditAttempt(context.WithoutCancel(ctx), account, operationID, holder, queryRevision, UsageCreditsStateError, nil)
			result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, account)
			return result, err
		}
	}
	operationExists := credits.Turns > 0 || credits.OperationCredits > 0
	if err := persistCreditSnapshot(context.WithoutCancel(ctx), account, operationID, holder, queryRevision, credits, operationExists); err != nil {
		result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, account)
		return result, err
	}
	result.Snapshot = loadCreditSnapshot(context.WithoutCancel(ctx), account.ID, account)
	return result, nil
}

func newOperationCreditsRequest(ctx context.Context, account *model.Account, operationID string) (*http.Request, error) {
	endpoint := strings.TrimRight(zencoderGatewayBaseURL(), "/") + "/api/v1/quotas/me/operations/" + url.PathEscape(operationID) + "/credits"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zen-cli/"+gatewayMetadata("ZENCODER_CLIENT_VERSION", zencoderVersion))
	if err := applyZencoderAuthWithoutHealthMutation(ctx, req, account); err != nil {
		return nil, fmt.Errorf("authenticate zencoder usage-based credits request: %w", err)
	}
	return req, nil
}

func newTokenCreditsRequest(ctx context.Context, account *model.Account) (*http.Request, error) {
	endpoint := strings.TrimRight(zencoderGatewayBaseURL(), "/") + "/api/v1/quotas/me/tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zen-cli/"+gatewayMetadata("ZENCODER_CLIENT_VERSION", zencoderVersion))
	if err := applyZencoderAuthWithoutHealthMutation(ctx, req, account); err != nil {
		return nil, fmt.Errorf("authenticate Zencoder usage-based token request: %w", err)
	}
	return req, nil
}

func fetchOperationCredits(req *http.Request) (parsedOperationCredits, error) {
	var credits parsedOperationCredits
	resp, err := newDirectHTTPClient(creditRequestTimeout).Do(req)
	if err != nil {
		return credits, fmt.Errorf("fetch zencoder usage-based credits: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		state := UsageCreditsStateError
		if resp.StatusCode == http.StatusNotFound {
			state = UsageCreditsStateUnsupported
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCreditResponseBytes))
		return credits, &creditHTTPError{status: resp.StatusCode, state: state}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCreditResponseBytes+1))
	if err != nil {
		return credits, fmt.Errorf("read zencoder usage-based credits: %w", err)
	}
	if len(body) > maxCreditResponseBytes {
		return credits, errors.New("zencoder usage-based credits response is too large")
	}
	return parseOperationCredits(body)
}

func fetchTokenCredits(req *http.Request) (parsedTokenCredits, error) {
	var credits parsedTokenCredits
	resp, err := newDirectHTTPClient(creditRequestTimeout).Do(req)
	if err != nil {
		return credits, fmt.Errorf("fetch Zencoder usage-based token balance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		state := UsageCreditsStateError
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
			state = UsageCreditsStateUnsupported
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCreditResponseBytes))
		return credits, &creditHTTPError{status: resp.StatusCode, state: state}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCreditResponseBytes+1))
	if err != nil {
		return credits, fmt.Errorf("read zencoder usage-based token balance: %w", err)
	}
	if len(body) > maxCreditResponseBytes {
		return credits, errors.New("zencoder usage-based token response is too large")
	}
	return parseTokenCredits(body)
}

func parseOperationCredits(body []byte) (parsedOperationCredits, error) {
	var raw operationCreditsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedOperationCredits{}, fmt.Errorf("decode zencoder usage-based credits: %w", err)
	}
	operationCredits, err := parseNonNegativeJSONInt(raw.TotalOperationCreditsRaw, "totalOperationCredits")
	if err != nil {
		return parsedOperationCredits{}, err
	}
	turns, err := parseOptionalNonNegativeJSONInt(raw.TurnsRaw, "turns")
	if err != nil {
		return parsedOperationCredits{}, err
	}
	consumed, err := parseNonNegativeJSONInt(raw.TotalConsumedRaw, "totalUserConsumedCredits")
	if err != nil {
		return parsedOperationCredits{}, err
	}
	budget, err := parseNonNegativeJSONInt(raw.TotalBudgetRaw, "totalUserBudget")
	if err != nil {
		return parsedOperationCredits{}, err
	}
	remaining := budget - consumed
	if remaining < 0 {
		remaining = 0
	}
	return parsedOperationCredits{
		OperationID:      strings.TrimSpace(raw.OperationID),
		OperationCredits: operationCredits,
		Turns:            turns,
		Consumed:         consumed,
		Budget:           budget,
		Remaining:        remaining,
	}, nil
}

func parseTokenCredits(body []byte) (parsedTokenCredits, error) {
	var raw tokenCreditsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedTokenCredits{}, fmt.Errorf("decode Zencoder usage-based token balance: %w", err)
	}
	consumed, err := parseNonNegativeJSONInt(raw.TotalConsumedByUser, "totalConsumedByUser")
	if err != nil {
		return parsedTokenCredits{}, err
	}
	budget, err := parseNonNegativeJSONInt(raw.TotalUserBudget, "totalUserBudget")
	if err != nil {
		return parsedTokenCredits{}, err
	}
	remaining := budget - consumed
	if remaining < 0 {
		remaining = 0
	}
	if value := strings.TrimSpace(string(raw.RemainingRaw)); value != "" && value != "null" {
		remaining, err = parseNonNegativeJSONInt(raw.RemainingRaw, "remaining")
		if err != nil {
			return parsedTokenCredits{}, err
		}
		// The endpoint's explicit remaining value is authoritative. Real
		// responses can differ from budget-consumed, so do not derive or clamp it.
	}
	periodEnd, err := parseOptionalRFC3339JSONTime(raw.PeriodEndRaw, "periodEnd")
	if err != nil {
		return parsedTokenCredits{}, err
	}
	return parsedTokenCredits{Consumed: consumed, Budget: budget, Remaining: remaining, PeriodEnd: periodEnd}, nil
}

func parseNonNegativeJSONInt(raw json.RawMessage, field string) (int64, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, fmt.Errorf("zencoder usage-based credits response is missing %s", field)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("zencoder usage-based credits response has invalid %s", field)
	}
	return parsed, nil
}

func parseOptionalNonNegativeJSONInt(raw json.RawMessage, field string) (int64, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, nil
	}
	return parseNonNegativeJSONInt(raw, field)
}

func parseOptionalRFC3339JSONTime(raw json.RawMessage, field string) (*time.Time, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return nil, nil
	}
	var textValue string
	if err := json.Unmarshal(raw, &textValue); err != nil || strings.TrimSpace(textValue) == "" {
		return nil, fmt.Errorf("zencoder usage-based credits response has invalid %s", field)
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(textValue))
	if err != nil {
		return nil, fmt.Errorf("zencoder usage-based credits response has invalid %s", field)
	}
	return &parsed, nil
}

func claimCreditRefreshLease(ctx context.Context, account model.Account, operationID, holder string) (uint64, bool, error) {
	db := database.GetDB()
	if db == nil {
		return 0, false, errDatabaseUnavailable
	}
	now := time.Now().UTC()
	queryRevision := account.UsageCreditsQueryRevision + 1
	query := db.WithContext(ctx).Model(&model.Account{}).
		Where("id = ? AND credential_revision = ? AND usage_credits_query_revision = ? AND (usage_credits_lease_id IS NULL OR usage_credits_lease_id = '' OR usage_credits_lease_until IS NULL OR julianday(usage_credits_lease_until) <= julianday(?))",
			account.ID, account.CredentialRevision, account.UsageCreditsQueryRevision, now)
	if operationID != "" {
		query = query.Where("usage_credits_operation_id = ?", operationID)
	}
	result := query.
		Updates(map[string]interface{}{
			"usage_credits_query_revision":  queryRevision,
			"usage_credits_lease_id":        holder,
			"usage_credits_lease_until":     now.Add(creditRefreshLeaseDuration),
			"usage_credits_status":          UsageCreditsStateRefreshing,
			"usage_credits_last_attempt_at": now,
		})
	if result.Error != nil {
		return 0, false, fmt.Errorf("claim usage-based credit refresh lease: %w", result.Error)
	}
	return queryRevision, result.RowsAffected == 1, nil
}

func persistTokenCreditSnapshot(ctx context.Context, account model.Account, holder string, queryRevision uint64, credits parsedTokenCredits) error {
	db := database.GetDB()
	if db == nil {
		return errDatabaseUnavailable
	}
	now := time.Now().UTC()
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	query := db.WithContext(cleanupCtx).Model(&model.Account{}).
		Where("id = ? AND credential_revision = ? AND usage_credits_query_revision = ? AND usage_credits_lease_id = ?",
			account.ID, account.CredentialRevision, queryRevision, holder)
	if credits.PeriodEnd != nil {
		// periodEnd is the only upstream snapshot ordering signal. Within a
		// known period, total consumed is a high-water mark; remaining may move
		// in either direction because the service can release reservations or
		// apply a balance adjustment.
		query = query.Where(`(
			usage_credits_available = ? OR
			usage_credits_period_end IS NULL OR
			julianday(usage_credits_period_end) < julianday(?) OR
			(julianday(usage_credits_period_end) = julianday(?) AND usage_credits_consumed <= ?)
		)`, false, *credits.PeriodEnd, *credits.PeriodEnd, credits.Consumed)
	} else {
		// Some compatible gateways omit periodEnd. Preserve a still-active
		// known period and apply the same high-water check; an expired period is
		// treated as a reset and cleared below.
		query = query.Where(`(
			usage_credits_available = ? OR
			usage_credits_period_end IS NULL OR
			julianday(usage_credits_period_end) <= julianday(?) OR
			usage_credits_consumed <= ?
		)`, false, now, credits.Consumed)
	}
	updates := map[string]interface{}{
		// /tokens is the authoritative account-level snapshot. Any
		// operation-specific details from an older operation are no longer
		// associated with this balance.
		"usage_credits_operation_credits":   0,
		"usage_credits_turns":               0,
		"usage_credits_operation_exists":    false,
		"usage_credits_consumed":            credits.Consumed,
		"usage_credits_budget":              credits.Budget,
		"usage_credits_remaining":           credits.Remaining,
		"usage_credits_available":           true,
		"usage_credits_status":              UsageCreditsStateReady,
		"usage_credits_source":              usageCreditsSourceTokens,
		"usage_credits_updated_at":          now,
		"usage_credits_last_attempt_at":     now,
		"usage_credits_credential_revision": account.CredentialRevision,
		"usage_credits_lease_id":            "",
		"usage_credits_lease_until":         gorm.Expr("NULL"),
	}
	if credits.PeriodEnd != nil {
		updates["usage_credits_period_end"] = credits.PeriodEnd
	} else {
		updates["usage_credits_period_end"] = gorm.Expr(
			"CASE WHEN usage_credits_period_end IS NOT NULL AND julianday(usage_credits_period_end) <= julianday(?) THEN NULL ELSE usage_credits_period_end END",
			now,
		)
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("persist usage-based token snapshot: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errCreditRefreshSuperseded
	}
	// Read back the accepted row so an omitted/expired periodEnd and any
	// concurrent state transition are reflected exactly in the scheduler cache.
	updatePoolCredits(account.ID)
	return nil
}

func persistCreditSnapshot(ctx context.Context, account model.Account, operationID, holder string, queryRevision uint64, credits parsedOperationCredits, operationExists bool) error {
	db := database.GetDB()
	if db == nil {
		return errDatabaseUnavailable
	}
	now := time.Now().UTC()
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	tokenSnapshot := "usage_credits_source = ?"
	result := creditRefreshRowQuery(db.WithContext(cleanupCtx).Model(&model.Account{}), account, operationID, queryRevision, holder).
		Updates(map[string]interface{}{
			"usage_credits_operation_credits": credits.OperationCredits,
			"usage_credits_turns":             credits.Turns,
			"usage_credits_operation_exists":  operationExists,
			// /tokens is authoritative even when a compatible gateway omits
			// periodEnd. Legacy operation values may only fill the balance when
			// no token snapshot has ever been accepted for this credential.
			"usage_credits_consumed": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_consumed ELSE ? END",
				usageCreditsSourceTokens, credits.Consumed,
			),
			"usage_credits_budget": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_budget ELSE ? END",
				usageCreditsSourceTokens, credits.Budget,
			),
			"usage_credits_remaining": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_remaining ELSE ? END",
				usageCreditsSourceTokens, credits.Remaining,
			),
			"usage_credits_available": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_available ELSE ? END",
				usageCreditsSourceTokens, true,
			),
			"usage_credits_status": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN CASE WHEN usage_credits_available THEN ? ELSE ? END ELSE ? END",
				usageCreditsSourceTokens, UsageCreditsStateStale, UsageCreditsStateUnknown, UsageCreditsStateReady,
			),
			"usage_credits_source": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_source ELSE ? END",
				usageCreditsSourceTokens, usageCreditsSourceOperation,
			),
			"usage_credits_updated_at": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_updated_at ELSE ? END",
				usageCreditsSourceTokens, now,
			),
			"usage_credits_period_end": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_period_end ELSE NULL END",
				usageCreditsSourceTokens,
			),
			"usage_credits_last_attempt_at": now,
			"usage_credits_credential_revision": gorm.Expr(
				"CASE WHEN "+tokenSnapshot+" THEN usage_credits_credential_revision ELSE ? END",
				usageCreditsSourceTokens, account.CredentialRevision,
			),
			"usage_credits_lease_id":    "",
			"usage_credits_lease_until": gorm.Expr("NULL"),
		})
	if result.Error != nil {
		return fmt.Errorf("persist usage-based credit snapshot: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errCreditRefreshSuperseded
	}
	updatePoolCredits(account.ID)
	return nil
}

func persistCreditAttempt(ctx context.Context, account model.Account, operationID, holder string, queryRevision uint64, state string, credits *parsedOperationCredits) {
	db := database.GetDB()
	if db == nil {
		return
	}
	now := time.Now().UTC()
	updates := map[string]interface{}{
		"usage_credits_status":          state,
		"usage_credits_last_attempt_at": now,
		"usage_credits_lease_id":        "",
		"usage_credits_lease_until":     gorm.Expr("NULL"),
	}
	if state == "" {
		updates["usage_credits_status"] = gorm.Expr(
			"CASE WHEN usage_credits_available THEN ? ELSE ? END",
			UsageCreditsStateStale, UsageCreditsStateUnknown,
		)
	}
	if credits != nil {
		updates["usage_credits_operation_credits"] = credits.OperationCredits
		updates["usage_credits_turns"] = credits.Turns
		updates["usage_credits_operation_exists"] = false
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	result := creditRefreshRowQuery(db.WithContext(cleanupCtx).Model(&model.Account{}), account, operationID, queryRevision, holder).Updates(updates)
	if result.Error != nil {
		logging.Debugf("Persist usage-based credit attempt failed: account_id=%d error=%v", account.ID, result.Error)
	}
	updatePoolCredits(account.ID)
}

func markCreditAttemptWithoutLease(ctx context.Context, account model.Account, operationID, state string) {
	db := database.GetDB()
	if db == nil {
		return
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	now := time.Now().UTC()
	_ = creditRefreshRowQuery(db.WithContext(cleanupCtx).Model(&model.Account{}), account, operationID, 0, "").
		Where("usage_credits_query_revision = ?", account.UsageCreditsQueryRevision).
		Where("(usage_credits_lease_id IS NULL OR usage_credits_lease_id = '' OR usage_credits_lease_until IS NULL OR julianday(usage_credits_lease_until) <= julianday(?))", now).
		Updates(map[string]interface{}{
			"usage_credits_status":          state,
			"usage_credits_last_attempt_at": now,
			"usage_credits_query_revision":  gorm.Expr("usage_credits_query_revision + 1"),
		}).Error
	updatePoolCredits(account.ID)
}

func creditRefreshRowQuery(query *gorm.DB, account model.Account, operationID string, queryRevision uint64, holder string) *gorm.DB {
	query = query.Where("id = ? AND credential_revision = ?", account.ID, account.CredentialRevision)
	if operationID != "" {
		query = query.Where("usage_credits_operation_id = ?", operationID)
	}
	if queryRevision != 0 {
		query = query.Where("usage_credits_query_revision = ? AND usage_credits_lease_id = ?", queryRevision, holder)
	}
	return query
}

func releaseCreditRefreshLease(ctx context.Context, accountID uint, holder string) {
	db := database.GetDB()
	if db == nil {
		return
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	result := db.WithContext(cleanupCtx).Model(&model.Account{}).
		Where("id = ? AND usage_credits_lease_id = ?", accountID, holder).
		Updates(map[string]interface{}{
			"usage_credits_lease_id":    "",
			"usage_credits_lease_until": gorm.Expr("NULL"),
			"usage_credits_status": gorm.Expr(
				"CASE WHEN usage_credits_status = ? THEN CASE WHEN usage_credits_available THEN ? ELSE ? END ELSE usage_credits_status END",
				UsageCreditsStateRefreshing, UsageCreditsStateStale, UsageCreditsStateUnknown,
			),
			"usage_credits_last_attempt_at": time.Now().UTC(),
		})
	if result.Error != nil {
		logging.Debugf("Release usage-based credit refresh lease failed: account_id=%d error=%v", accountID, result.Error)
		return
	}
	if result.RowsAffected == 1 {
		updatePoolCredits(accountID)
	}
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), creditCleanupTimeout)
}

func loadCreditSnapshot(ctx context.Context, accountID uint, fallback model.Account) UsageBasedCreditsDTO {
	db := database.GetDB()
	if db == nil {
		return CreditSnapshotForAccount(fallback)
	}
	var account model.Account
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := db.WithContext(cleanupCtx).First(&account, accountID).Error; err != nil {
		return CreditSnapshotForAccount(fallback)
	}
	return CreditSnapshotForAccount(account)
}

func updatePoolCredits(accountID uint) {
	if pool == nil || accountID == 0 {
		return
	}
	db := database.GetDB()
	if db == nil {
		return
	}
	var latest model.Account
	ctx, cancel := context.WithTimeout(context.Background(), creditCleanupTimeout)
	defer cancel()
	if err := db.WithContext(ctx).Select(
		"id", "credential_revision", "usage_credits_operation_credits", "usage_credits_turns", "usage_credits_operation_exists",
		"usage_credits_consumed", "usage_credits_budget", "usage_credits_remaining", "usage_credits_available",
		"usage_credits_status", "usage_credits_source", "usage_credits_updated_at", "usage_credits_period_end", "usage_credits_last_attempt_at", "usage_credits_operation_id",
		"usage_credits_credential_revision", "usage_credits_query_revision", "usage_credits_lease_id", "usage_credits_lease_until",
	).First(&latest, accountID).Error; err != nil {
		return
	}
	syncPoolUsageCredits(latest)
}

func usageCreditsSnapshotTimestamp(account model.Account) time.Time {
	latest := time.Time{}
	if account.UsageCreditsUpdatedAt != nil && account.UsageCreditsUpdatedAt.After(latest) {
		latest = *account.UsageCreditsUpdatedAt
	}
	if account.UsageCreditsLastAttemptAt != nil && account.UsageCreditsLastAttemptAt.After(latest) {
		latest = *account.UsageCreditsLastAttemptAt
	}
	return latest
}

func usageCreditsSnapshotNewer(candidate, current model.Account) bool {
	if candidate.UsageCreditsQueryRevision != current.UsageCreditsQueryRevision {
		return candidate.UsageCreditsQueryRevision > current.UsageCreditsQueryRevision
	}
	return usageCreditsSnapshotTimestamp(candidate).After(usageCreditsSnapshotTimestamp(current))
}

func copyUsageCreditsState(dst *model.Account, src model.Account) {
	dst.UsageCreditsOperationCredits = src.UsageCreditsOperationCredits
	dst.UsageCreditsTurns = src.UsageCreditsTurns
	dst.UsageCreditsOperationExists = src.UsageCreditsOperationExists
	dst.UsageCreditsConsumed = src.UsageCreditsConsumed
	dst.UsageCreditsBudget = src.UsageCreditsBudget
	dst.UsageCreditsRemaining = src.UsageCreditsRemaining
	dst.UsageCreditsAvailable = src.UsageCreditsAvailable
	dst.UsageCreditsStatus = src.UsageCreditsStatus
	dst.UsageCreditsSource = src.UsageCreditsSource
	dst.UsageCreditsUpdatedAt = src.UsageCreditsUpdatedAt
	dst.UsageCreditsPeriodEnd = src.UsageCreditsPeriodEnd
	dst.UsageCreditsLastAttemptAt = src.UsageCreditsLastAttemptAt
	dst.UsageCreditsOperationID = src.UsageCreditsOperationID
	dst.UsageCreditsCredentialRevision = src.UsageCreditsCredentialRevision
	dst.UsageCreditsQueryRevision = src.UsageCreditsQueryRevision
	dst.UsageCreditsLeaseID = src.UsageCreditsLeaseID
	dst.UsageCreditsLeaseUntil = src.UsageCreditsLeaseUntil
}

// syncPoolUsageCredits applies a known database snapshot to the in-memory
// scheduler cache without another database round trip. This is used on the
// response path after an operation is recorded, where a read-back could hold
// up the client response behind a busy SQLite writer.
func syncPoolUsageCredits(latest model.Account) {
	if pool == nil || latest.ID == 0 {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for index := range pool.accounts {
		if pool.accounts[index].ID != latest.ID {
			continue
		}
		if pool.accounts[index].CredentialRevision != latest.CredentialRevision ||
			pool.accounts[index].UsageCreditsQueryRevision > latest.UsageCreditsQueryRevision ||
			(pool.accounts[index].UsageCreditsQueryRevision == latest.UsageCreditsQueryRevision &&
				usageCreditsSnapshotTimestamp(latest).Before(usageCreditsSnapshotTimestamp(pool.accounts[index]))) {
			continue
		}
		copyUsageCreditsState(&pool.accounts[index], latest)
	}
}

func validCreditOperationID(operationID string) (string, bool) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" || len(operationID) > 200 || strings.ContainsAny(operationID, "\r\n\x00") {
		return "", false
	}
	return operationID, true
}

func operationIDIfPresent(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	operationID, ok := ctx.Value(operationIDContextKey{}).(string)
	if !ok {
		return "", false
	}
	return validCreditOperationID(operationID)
}
