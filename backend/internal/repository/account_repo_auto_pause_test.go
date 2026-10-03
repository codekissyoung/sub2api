package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type accountIDsPayloadMatcher struct {
	want []int64
}

func (m accountIDsPayloadMatcher) Match(value driver.Value) bool {
	raw, ok := value.([]byte)
	if !ok {
		return false
	}
	var payload struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return reflect.DeepEqual(m.want, payload.AccountIDs)
}

func TestAutoPauseExpiredAccountsEnqueuesAffectedAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	mock.ExpectQuery(`(?s)UPDATE accounts.*subscription_expires_at.*RETURNING id`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(11)).AddRow(int64(29)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)")).
		WithArgs(service.SchedulerOutboxEventAccountBulkChanged, nil, nil, accountIDsPayloadMatcher{want: []int64{11, 29}}).
		WillReturnResult(sqlmock.NewResult(1, 1))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	updated, err := repo.AutoPauseExpiredAccounts(context.Background(), now)

	require.NoError(t, err)
	require.EqualValues(t, 2, updated)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAutoPauseExpiredAccountsSkipsOutboxWithoutChanges(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	mock.ExpectQuery(`(?s)UPDATE accounts.*subscription_expires_at.*RETURNING id`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	updated, err := repo.AutoPauseExpiredAccounts(context.Background(), now)

	require.NoError(t, err)
	require.Zero(t, updated)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 2026-10-04 事故锁：ent 裸谓词里的 `?` 占位符不会被转换为 $n，会原样进 SQL
// 撞 PG 语法错误（当时调度桶重建全挂）。这里渲染 ListSchedulable 的真实 SQL，
// 断言凭据证活子句存在且不含任何 `?` 占位符。
func TestListSchedulableSQLContainsCredentialVetoAndNoRawPlaceholder(t *testing.T) {
	var captured []string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(_, actual string) error {
		captured = append(captured, actual)
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cols := []string{
		"id", "created_at", "updated_at", "deleted_at", "name", "notes", "platform", "type",
		"credentials", "extra", "proxy_id", "proxy_fallback_origin_id", "concurrency", "load_factor",
		"priority", "rate_multiplier", "status", "error_message", "last_used_at", "expires_at",
		"auto_pause_on_expired", "schedulable", "rate_limited_at", "rate_limit_reset_at",
		"overload_until", "temp_unschedulable_until", "temp_unschedulable_reason",
		"session_window_start", "session_window_end", "session_window_status",
		"parent_account_id", "quota_dimension",
	}
	mock.ExpectQuery(".*").WillReturnRows(sqlmock.NewRows(cols))

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newAccountRepositoryWithSQL(client, db, nil)
	_, err = repo.ListSchedulable(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, captured, "no SQL captured")
	for _, q := range captured {
		require.NotContains(t, q, "?", "raw `?` placeholder must never reach the wire: %s", q)
	}
	require.True(t, strings.Contains(captured[0], "pg_input_is_valid"), "credential-alive veto missing: %s", captured[0])
}
