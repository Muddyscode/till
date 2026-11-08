package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StaffAuth is a staff row plus the columns login and token checks need.
type StaffAuth struct {
	Staff
	TokenVersion int
	FailedCount  int
	LockedUntil  *time.Time
	IsService    bool
}

const staffAuthCols = `id, merchant_id, email, password_hash, role, disabled_at, created_at,
	token_version, failed_login_count, locked_until, is_service`

func scanStaffAuth(sc interface{ Scan(...any) error }) (StaffAuth, error) {
	var a StaffAuth
	err := sc.Scan(&a.ID, &a.MerchantID, &a.Email, &a.PasswordHash, &a.Role, &a.DisabledAt, &a.CreatedAt,
		&a.TokenVersion, &a.FailedCount, &a.LockedUntil, &a.IsService)
	return a, err
}

func (s *Store) GetStaffAuthByEmail(ctx context.Context, db DBTX, email string) (StaffAuth, error) {
	a, err := scanStaffAuth(db.QueryRow(ctx, `SELECT `+staffAuthCols+` FROM staff WHERE email = $1`, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffAuth{}, ledger.Err(ledger.CodeNotFound, "staff not found")
	}
	return a, err
}

func (s *Store) GetStaffAuthByID(ctx context.Context, db DBTX, id uuid.UUID) (StaffAuth, error) {
	a, err := scanStaffAuth(db.QueryRow(ctx, `SELECT `+staffAuthCols+` FROM staff WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffAuth{}, ledger.Err(ledger.CodeNotFound, "staff not found")
	}
	return a, err
}

const (
	LoginFailureLimit  = 10
	LoginFailureWindow = 15 * time.Minute
)

// RecordLoginFailure counts a failed attempt inside a rolling window and locks the
// account for the window length once LoginFailureLimit is reached. One atomic UPDATE.
func (s *Store) RecordLoginFailure(ctx context.Context, db DBTX, id uuid.UUID) (lockedUntil *time.Time, err error) {
	err = db.QueryRow(ctx, `
		UPDATE staff SET
			failed_login_count = CASE WHEN last_failed_login_at IS NULL OR last_failed_login_at < now() - $2::interval
				THEN 1 ELSE failed_login_count + 1 END,
			locked_until = CASE WHEN (CASE WHEN last_failed_login_at IS NULL OR last_failed_login_at < now() - $2::interval
				THEN 1 ELSE failed_login_count + 1 END) >= $3
				THEN now() + $2::interval ELSE locked_until END,
			last_failed_login_at = now()
		WHERE id = $1
		RETURNING locked_until
	`, id, fmt.Sprintf("%d seconds", int(LoginFailureWindow.Seconds())), LoginFailureLimit).Scan(&lockedUntil)
	return
}

func (s *Store) ClearLoginFailures(ctx context.Context, db DBTX, id uuid.UUID) error {
	_, err := db.Exec(ctx, `
		UPDATE staff SET failed_login_count = 0, last_failed_login_at = NULL, locked_until = NULL
		WHERE id = $1 AND (failed_login_count <> 0 OR locked_until IS NOT NULL)
	`, id)
	return err
}

// BumpTokenVersion revokes every token issued so far for the staff member.
func (s *Store) BumpTokenVersion(ctx context.Context, db DBTX, id uuid.UUID) (int, error) {
	var v int
	err := db.QueryRow(ctx, `UPDATE staff SET token_version = token_version + 1 WHERE id = $1 RETURNING token_version`, id).Scan(&v)
	return v, err
}

// SetPassword stores a new hash, revokes old tokens and clears any lockout. Returns the new version.
func (s *Store) SetPassword(ctx context.Context, db DBTX, merchantID, id uuid.UUID, hash string) (int, error) {
	var v int
	err := db.QueryRow(ctx, `
		UPDATE staff SET password_hash = $3, token_version = token_version + 1,
			failed_login_count = 0, last_failed_login_at = NULL, locked_until = NULL
		WHERE id = $2 AND merchant_id = $1 AND NOT is_service
		RETURNING token_version
	`, merchantID, id, hash).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ledger.Err(ledger.CodeNotFound, "staff not found")
	}
	return v, err
}

// ListLoginStaff lists staff who can log in (service rows behind API keys are hidden).
func (s *Store) ListLoginStaff(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]Staff, error) {
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, email, password_hash, role, disabled_at, created_at
		FROM staff WHERE merchant_id = $1 AND NOT is_service ORDER BY created_at, email
	`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Staff
	for rows.Next() {
		var st Staff
		if err := rows.Scan(&st.ID, &st.MerchantID, &st.Email, &st.PasswordHash, &st.Role, &st.DisabledAt, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---- API keys ----

type APIKey struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	StaffID    uuid.UUID
	Name       string
	Prefix     string
	Hash       []byte
	Role       string
	OutletIDs  []uuid.UUID
	CreatedBy  uuid.UUID
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

const apiKeyCols = `id, merchant_id, staff_id, name, prefix, hash, role, outlet_ids, created_by, created_at, last_used_at, revoked_at`

func scanAPIKey(sc interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	err := sc.Scan(&k.ID, &k.MerchantID, &k.StaffID, &k.Name, &k.Prefix, &k.Hash, &k.Role, &k.OutletIDs,
		&k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	return k, err
}

// HashAPIKeySecret is the stored digest of a key secret.
func HashAPIKeySecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreateAPIKey inserts the backing service staff, its outlet assignments and the key.
func (s *Store) CreateAPIKey(ctx context.Context, tx pgx.Tx, k APIKey) (APIKey, error) {
	staffID, err := uuid.NewV7()
	if err != nil {
		return APIKey{}, err
	}
	// "!" is not an argon2 hash, so password login can never succeed for this row.
	if _, err := tx.Exec(ctx, `
		INSERT INTO staff (id, merchant_id, email, password_hash, role, is_service)
		VALUES ($1, $2, $3, '!', $4, true)
	`, staffID, k.MerchantID, "apikey-"+k.ID.String()+"@api-key.invalid", k.Role); err != nil {
		return APIKey{}, err
	}
	for _, oid := range k.OutletIDs {
		if err := s.AssignStaffOutlet(ctx, tx, staffID, oid); err != nil {
			return APIKey{}, err
		}
	}
	return scanAPIKey(tx.QueryRow(ctx, `
		INSERT INTO api_keys (id, merchant_id, staff_id, name, prefix, hash, role, outlet_ids, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING `+apiKeyCols,
		k.ID, k.MerchantID, staffID, k.Name, k.Prefix, k.Hash, k.Role, k.OutletIDs, k.CreatedBy))
}

func (s *Store) GetAPIKeyByPrefix(ctx context.Context, db DBTX, prefix string) (APIKey, error) {
	k, err := scanAPIKey(db.QueryRow(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE prefix = $1`, prefix))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ledger.Err(ledger.CodeNotFound, "api key not found")
	}
	return k, err
}

func (s *Store) ListAPIKeys(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]APIKey, error) {
	rows, err := db.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE merchant_id = $1 ORDER BY created_at DESC, id`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey marks the key revoked and disables its service staff row.
func (s *Store) RevokeAPIKey(ctx context.Context, tx pgx.Tx, merchantID, id uuid.UUID) (APIKey, error) {
	k, err := scanAPIKey(tx.QueryRow(ctx, `
		UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1 AND merchant_id = $2 RETURNING `+apiKeyCols, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ledger.Err(ledger.CodeNotFound, "api key not found")
	}
	if err != nil {
		return APIKey{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE staff SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1`, k.StaffID)
	return k, err
}

// TouchAPIKey records use at most once a minute per key.
func (s *Store) TouchAPIKey(ctx context.Context, db DBTX, id uuid.UUID) {
	_, _ = db.Exec(ctx, `UPDATE api_keys SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
}
