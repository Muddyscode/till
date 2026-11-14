package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func IsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

func IsUnique(err error, target **pgconn.PgError) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if target != nil {
			*target = pgErr
		}
		return true
	}
	return false
}

func RetryDeadlock(fn func() error) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = fn()
		if err == nil || !IsDeadlock(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return err
}

func (s *Store) CompleteIdempotency(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string, status int, body []byte, journalID *uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', http_status = $3, response_body = $4, journal_id = $5
		WHERE merchant_id = $1 AND key = $2
	`, merchantID, key, status, body, journalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("idempotency complete: expected 1 row")
	}
	return nil
}

func (s *Store) CreateAccount(ctx context.Context, db DBTX, a ledger.Account) (ledger.Account, error) {
	err := db.QueryRow(ctx, `
		INSERT INTO accounts (id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor, created_at
	`, a.ID, a.MerchantID, a.Code, a.Name, a.Type, a.Currency, a.AllowNegative, a.Status, a.BalanceMinor).
		Scan(&a.ID, &a.MerchantID, &a.Code, &a.Name, &a.Type, &a.Currency, &a.AllowNegative, &a.Status, &a.BalanceMinor, &a.CreatedAt)
	return a, err
}

func (s *Store) GetAccount(ctx context.Context, db DBTX, operatorID, accountID uuid.UUID) (ledger.Account, error) {
	var a ledger.Account
	err := db.QueryRow(ctx, `
		SELECT id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor, created_at
		FROM accounts WHERE id = $1 AND merchant_id = $2
	`, accountID, operatorID).Scan(
		&a.ID, &a.MerchantID, &a.Code, &a.Name, &a.Type, &a.Currency, &a.AllowNegative, &a.Status, &a.BalanceMinor, &a.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Account{}, ledger.Err(ledger.CodeNotFound, "account not found")
	}
	return a, err
}

func (s *Store) ListAccounts(ctx context.Context, db DBTX, operatorID uuid.UUID, typeFilter string) ([]ledger.Account, error) {
	q := `
		SELECT id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor, created_at
		FROM accounts WHERE merchant_id = $1
	`
	args := []any{operatorID}
	if typeFilter != "" {
		q += ` AND type = $2`
		args = append(args, typeFilter)
	}
	q += ` ORDER BY code`
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Account
	for rows.Next() {
		var a ledger.Account
		if err := rows.Scan(&a.ID, &a.MerchantID, &a.Code, &a.Name, &a.Type, &a.Currency, &a.AllowNegative, &a.Status, &a.BalanceMinor, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) LockAccounts(ctx context.Context, tx pgx.Tx, operatorID uuid.UUID, ids []uuid.UUID) ([]ledger.Account, error) {
	if len(ids) == 0 {
		return nil, ledger.Err(ledger.CodeInvalidJournal, "no accounts")
	}
	rows, err := tx.Query(ctx, `
		SELECT id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor, created_at
		FROM accounts
		WHERE merchant_id = $1 AND id = ANY($2)
		ORDER BY id
		FOR UPDATE
	`, operatorID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Account
	for rows.Next() {
		var a ledger.Account
		if err := rows.Scan(&a.ID, &a.MerchantID, &a.Code, &a.Name, &a.Type, &a.Currency, &a.AllowNegative, &a.Status, &a.BalanceMinor, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(uniq(ids)) {
		return nil, ledger.Err(ledger.CodeNotFound, "one or more accounts were not found")
	}
	return out, nil
}

func uniq(ids []uuid.UUID) []uuid.UUID {
	m := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, id := range ids {
		if _, ok := m[id]; ok {
			continue
		}
		m[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Poster adapts the store for ledger.Post.
func (s *Store) Poster() ledger.StorePoster {
	return ledger.StorePoster{
		LockAccounts:  s.LockAccounts,
		InsertJournal: s.InsertJournal,
		InsertPosting: s.InsertPosting,
		UpdateBalance: s.UpdateBalance,
	}
}

func (s *Store) InsertJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO journals (id, merchant_id, status, description, idempotency_key, reverses_journal_id)
		VALUES ($1,$2,'posted',$3,NULLIF($4,''),$5)
	`, j.ID, j.MerchantID, j.Description, j.IdempotencyKey, j.ReversesJournalID)
	return err
}

func (s *Store) InsertPosting(ctx context.Context, tx pgx.Tx, p ledger.Posting) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO postings (id, journal_id, account_id, direction, amount_minor, currency)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, p.ID, p.JournalID, p.AccountID, p.Direction, p.Amount, p.Currency)
	return err
}

func (s *Store) UpdateBalance(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, balance int64) error {
	_, err := tx.Exec(ctx, `UPDATE accounts SET balance_minor = $2 WHERE id = $1`, accountID, balance)
	return err
}

func (s *Store) GetJournal(ctx context.Context, db DBTX, operatorID, journalID uuid.UUID) (ledger.Journal, error) {
	var j ledger.Journal
	err := db.QueryRow(ctx, `
		SELECT id, merchant_id, status, description, COALESCE(idempotency_key, ''), reverses_journal_id, created_at
		FROM journals WHERE id = $1 AND merchant_id = $2
	`, journalID, operatorID).Scan(&j.ID, &j.MerchantID, &j.Status, &j.Description, &j.IdempotencyKey, &j.ReversesJournalID, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Journal{}, ledger.Err(ledger.CodeNotFound, "journal not found")
	}
	if err != nil {
		return ledger.Journal{}, err
	}
	posts, err := s.ListPostings(ctx, db, []uuid.UUID{j.ID})
	if err != nil {
		return ledger.Journal{}, err
	}
	j.Postings = posts[j.ID]
	return j, nil
}

func (s *Store) ListPostings(ctx context.Context, db DBTX, journalIDs []uuid.UUID) (map[uuid.UUID][]ledger.Posting, error) {
	out := map[uuid.UUID][]ledger.Posting{}
	if len(journalIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `
		SELECT id, journal_id, account_id, direction, amount_minor, currency
		FROM postings WHERE journal_id = ANY($1)
		ORDER BY direction DESC, id
	`, journalIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ledger.Posting
		if err := rows.Scan(&p.ID, &p.JournalID, &p.AccountID, &p.Direction, &p.Amount, &p.Currency); err != nil {
			return nil, err
		}
		out[p.JournalID] = append(out[p.JournalID], p)
	}
	return out, rows.Err()
}

func (s *Store) ReversalOf(ctx context.Context, db DBTX, operatorID, originalID uuid.UUID) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `
		SELECT id FROM journals WHERE merchant_id = $1 AND reverses_journal_id = $2
	`, operatorID, originalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

type StatementRow struct {
	Posting     ledger.Posting
	Description string
	CreatedAt   time.Time
	Running     int64
}

func (s *Store) Statement(ctx context.Context, db DBTX, operatorID, accountID uuid.UUID, limit int) ([]StatementRow, int64, error) {
	acct, err := s.GetAccount(ctx, db, operatorID, accountID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := db.Query(ctx, `
		SELECT p.id, p.journal_id, p.account_id, p.direction, p.amount_minor, p.currency,
		       j.description, j.created_at
		FROM postings p
		JOIN journals j ON j.id = p.journal_id
		WHERE p.account_id = $1 AND j.merchant_id = $2
		ORDER BY j.created_at ASC, p.id ASC
		LIMIT $3
	`, accountID, operatorID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []StatementRow
	var running int64
	for rows.Next() {
		var r StatementRow
		if err := rows.Scan(
			&r.Posting.ID, &r.Posting.JournalID, &r.Posting.AccountID, &r.Posting.Direction, &r.Posting.Amount, &r.Posting.Currency,
			&r.Description, &r.CreatedAt,
		); err != nil {
			return nil, 0, err
		}
		running += ledger.NaturalDelta(acct.Type, r.Posting.Direction, r.Posting.Amount)
		r.Running = running
		out = append(out, r)
	}
	return out, acct.BalanceMinor, rows.Err()
}

type Identity struct {
	IdentityMinor int64
	CacheDrift    int64
	Accounts      int64
	Journals      int64
}

func (s *Store) Identity(ctx context.Context, db DBTX, operatorID uuid.UUID) (Identity, error) {
	var idn Identity
	err := db.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN type IN ('asset','expense') THEN balance_minor ELSE -balance_minor END), 0),
			COUNT(*)
		FROM accounts WHERE merchant_id = $1
	`, operatorID).Scan(&idn.IdentityMinor, &idn.Accounts)
	if err != nil {
		return Identity{}, err
	}
	err = db.QueryRow(ctx, `SELECT COUNT(*) FROM journals WHERE merchant_id = $1`, operatorID).Scan(&idn.Journals)
	if err != nil {
		return Identity{}, err
	}
	err = db.QueryRow(ctx, `
		SELECT COALESCE(SUM(ABS(a.balance_minor - v.balance_minor)), 0)
		FROM accounts a
		JOIN account_balances_from_journal v ON v.id = a.id
		WHERE a.merchant_id = $1
	`, operatorID).Scan(&idn.CacheDrift)
	return idn, err
}

func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

type IdempotencyRecord struct {
	Replay     bool
	HTTPStatus int
	Body       []byte
	JournalID  *uuid.UUID
}

func (s *Store) ClaimIdempotency(ctx context.Context, tx pgx.Tx, operatorID uuid.UUID, key string, hash []byte) (IdempotencyRecord, error) {
	var claimed uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency_keys (merchant_id, key, request_hash, status)
		VALUES ($1, $2, $3, 'processing')
		ON CONFLICT (merchant_id, key) DO NOTHING
		RETURNING merchant_id
	`, operatorID, key, hash).Scan(&claimed)
	if err == nil {
		return IdempotencyRecord{}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return IdempotencyRecord{}, err
	}
	var storedHash []byte
	var status string
	var httpStatus *int
	var body []byte
	var journalID *uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT request_hash, status, http_status, response_body, journal_id
		FROM idempotency_keys
		WHERE merchant_id = $1 AND key = $2
		FOR UPDATE
	`, operatorID, key).Scan(&storedHash, &status, &httpStatus, &body, &journalID)
	if err != nil {
		return IdempotencyRecord{}, err
	}
	if subtle.ConstantTimeCompare(storedHash, hash) != 1 {
		return IdempotencyRecord{}, ledger.Err(ledger.CodeIdempotencyConflict, "Idempotency-Key reused with a different request body")
	}
	if status == "processing" {
		return IdempotencyRecord{}, ledger.Err(ledger.CodeIdempotencyInProgress, "a request with this Idempotency-Key is still processing")
	}
	st := 0
	if httpStatus != nil {
		st = *httpStatus
	}
	return IdempotencyRecord{Replay: true, HTTPStatus: st, Body: body, JournalID: journalID}, nil
}
