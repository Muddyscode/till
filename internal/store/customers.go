package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Customer struct {
	ID           uuid.UUID
	MerchantID   uuid.UUID
	Name         string
	Phone        string
	BalanceMinor int64
	CreatedAt    time.Time
}

func (s *Store) CreateCustomer(ctx context.Context, db DBTX, c Customer) (Customer, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.Phone = strings.TrimSpace(c.Phone)
	if c.Name == "" || c.Phone == "" {
		return Customer{}, ledger.Err(ledger.CodeInvalidOrder, "customer name and phone are required")
	}
	if len(c.Name) > 80 || len(c.Phone) > 32 {
		return Customer{}, ledger.Err(ledger.CodeInvalidOrder, "customer name or phone is too long")
	}
	err := db.QueryRow(ctx, `
		INSERT INTO customers (id, merchant_id, name, phone)
		VALUES ($1, $2, $3, $4)
		RETURNING id, merchant_id, name, phone, balance_minor, created_at
	`, c.ID, c.MerchantID, c.Name, c.Phone).Scan(&c.ID, &c.MerchantID, &c.Name, &c.Phone, &c.BalanceMinor, &c.CreatedAt)
	return c, err
}

func scanCustomer(sc interface{ Scan(...any) error }) (Customer, error) {
	var c Customer
	err := sc.Scan(&c.ID, &c.MerchantID, &c.Name, &c.Phone, &c.BalanceMinor, &c.CreatedAt)
	return c, err
}

func (s *Store) GetCustomer(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Customer, error) {
	c, err := scanCustomer(db.QueryRow(ctx, `
		SELECT id, merchant_id, name, phone, balance_minor, created_at
		FROM customers WHERE id = $1 AND merchant_id = $2
	`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ledger.Err(ledger.CodeNotFound, "customer not found")
	}
	return c, err
}

func (s *Store) LockCustomer(ctx context.Context, tx pgx.Tx, merchantID, id uuid.UUID) (Customer, error) {
	c, err := scanCustomer(tx.QueryRow(ctx, `
		SELECT id, merchant_id, name, phone, balance_minor, created_at
		FROM customers WHERE id = $1 AND merchant_id = $2
		FOR UPDATE
	`, id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ledger.Err(ledger.CodeNotFound, "customer not found")
	}
	return c, err
}

func (s *Store) ListCustomers(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]Customer, error) {
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, name, phone, balance_minor, created_at
		FROM customers WHERE merchant_id = $1
		ORDER BY name
	`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Customer
	for rows.Next() {
		c, err := scanCustomer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddWalletBalance adds delta kobo. A negative delta that would pass zero is insufficient_wallet.
func (s *Store) AddWalletBalance(ctx context.Context, tx pgx.Tx, merchantID, id uuid.UUID, delta int64) (int64, error) {
	var bal int64
	err := tx.QueryRow(ctx, `
		UPDATE customers
		SET balance_minor = balance_minor + $3
		WHERE id = $1 AND merchant_id = $2 AND balance_minor + $3 >= 0
		RETURNING balance_minor
	`, id, merchantID, delta).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM customers WHERE id = $1 AND merchant_id = $2)`, id, merchantID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, ledger.Err(ledger.CodeNotFound, "customer not found")
		}
		return 0, ledger.Err(ledger.CodeInsufficientWallet, "wallet balance is too small")
	}
	return bal, err
}

func (s *Store) WalletBalanceSum(ctx context.Context, db DBTX, merchantID uuid.UUID) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `SELECT COALESCE(SUM(balance_minor), 0) FROM customers WHERE merchant_id = $1`, merchantID).Scan(&n)
	return n, err
}
