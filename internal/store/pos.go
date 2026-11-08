package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Merchant struct {
	ID                     uuid.UUID
	Name                   string
	VATInclusiveBps        int
	ClearingAccountID      *uuid.UUID
	VATAccountID           *uuid.UUID
	EquityAccountID        *uuid.UUID
	InventoryAccountID     *uuid.UUID
	COGSAccountID          *uuid.UUID
	WalletAccountID        *uuid.UUID
	GoodsReceivedAccountID *uuid.UUID
	AdjustmentAccountID    *uuid.UUID
	CreatedAt              time.Time
}

const merchantCols = `id, name, vat_inclusive_bps, clearing_account_id, vat_account_id, equity_account_id,
	inventory_account_id, cogs_account_id, wallet_account_id, goods_received_account_id, adjustment_account_id, created_at`

func scanMerchant(sc interface{ Scan(...any) error }) (Merchant, error) {
	var m Merchant
	err := sc.Scan(
		&m.ID, &m.Name, &m.VATInclusiveBps, &m.ClearingAccountID, &m.VATAccountID, &m.EquityAccountID,
		&m.InventoryAccountID, &m.COGSAccountID, &m.WalletAccountID, &m.GoodsReceivedAccountID, &m.AdjustmentAccountID, &m.CreatedAt,
	)
	return m, err
}

type Staff struct {
	ID           uuid.UUID
	MerchantID   uuid.UUID
	Email        string
	PasswordHash string
	Role         string
	DisabledAt   *time.Time
	CreatedAt    time.Time
}

type Outlet struct {
	ID             uuid.UUID
	MerchantID     uuid.UUID
	Name           string
	Code           string
	CashAccountID  uuid.UUID
	SalesAccountID uuid.UUID
	CreatedAt      time.Time
}

type Product struct {
	ID           uuid.UUID
	MerchantID   uuid.UUID
	SKU          string
	Name         string
	PriceMinor   int64
	AvgCostMinor int64
	ReorderLevel int
	Active       bool
	CreatedAt    time.Time
}

type OrderLine struct {
	ID             uuid.UUID
	OrderID        uuid.UUID
	ProductID      uuid.UUID
	SKU            string
	Qty            int
	UnitPriceMinor int64
	LineTotalMinor int64
}

type Order struct {
	ID             uuid.UUID
	MerchantID     uuid.UUID
	OutletID       uuid.UUID
	StaffID        uuid.UUID
	Status         string
	PaymentMethod  string
	Currency       string
	TotalMinor     int64
	VATMinor       int64
	NetMinor       int64
	DiscountMinor  int64
	CostMinor      int64
	CustomerID     *uuid.UUID
	Reference      string
	IdempotencyKey string
	JournalID      *uuid.UUID
	ExpiresAt      *time.Time
	PaidAt         *time.Time
	CreatedAt      time.Time
	Lines          []OrderLine
}

type Reservation struct {
	ID        uuid.UUID
	ProductID uuid.UUID
	Qty       int
	Status    string
}

type StockRow struct {
	ProductID    uuid.UUID
	SKU          string
	Name         string
	OnHand       int
	Reserved     int
	Available    int
	ReorderLevel int
	PriceMinor   int64
	AvgCostMinor int64
}

type MethodTotal struct {
	Method     string
	OrderCount int
	TotalMinor int64
	VATMinor   int64
	NetMinor   int64
}

type DailySales struct {
	OrderCount int
	TotalMinor int64
	VATMinor   int64
	NetMinor   int64
	BooksDebit int64
	ByMethod   []MethodTotal
}

type AuditEvent struct {
	ID         uuid.UUID
	MerchantID *uuid.UUID
	StaffID    *uuid.UUID
	Action     string
	EntityType string
	EntityID   uuid.UUID
	OutletID   *uuid.UUID
	Metadata   []byte
	CreatedAt  time.Time
}

func (s *Store) CreateMerchant(ctx context.Context, tx pgx.Tx, id uuid.UUID, name string, vat int) (Merchant, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Merchant{}, ledger.Err(ledger.CodeInvalidOrder, "merchant name is required")
	}
	if vat < 0 || vat > 5000 {
		return Merchant{}, ledger.Err(ledger.CodeInvalidOrder, "vat_inclusive_bps must be between 0 and 5000")
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO merchants (id, name, vat_inclusive_bps)
		VALUES ($1, $2, $3)
		RETURNING `+merchantCols+`
	`, id, name, vat)
	return scanMerchant(row)
}

func (s *Store) GetMerchant(ctx context.Context, db DBTX, id uuid.UUID) (Merchant, error) {
	m, err := scanMerchant(db.QueryRow(ctx, `SELECT `+merchantCols+` FROM merchants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ledger.Err(ledger.CodeNotFound, "merchant not found")
	}
	return m, err
}

func (s *Store) EnsureBooks(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) (Merchant, error) {
	m, err := s.GetMerchant(ctx, tx, merchantID)
	if err != nil {
		return Merchant{}, err
	}
	if m.ClearingAccountID != nil && m.VATAccountID != nil && m.EquityAccountID != nil &&
		m.InventoryAccountID != nil && m.COGSAccountID != nil && m.WalletAccountID != nil &&
		m.GoodsReceivedAccountID != nil && m.AdjustmentAccountID != nil {
		return m, nil
	}
	type spec struct {
		code, name string
		typ        ledger.AccountType
		negative   bool
		dest       **uuid.UUID
	}
	specs := []spec{
		{"1100", "Paystack clearing", ledger.Asset, false, &m.ClearingAccountID},
		{"1200", "Inventory", ledger.Asset, false, &m.InventoryAccountID},
		{"2100", "VAT payable", ledger.Liability, false, &m.VATAccountID},
		{"2150", "Goods received", ledger.Liability, false, &m.GoodsReceivedAccountID},
		{"2200", "Customer wallet", ledger.Liability, false, &m.WalletAccountID},
		{"3000", "Opening equity", ledger.Equity, false, &m.EquityAccountID},
		{"5000", "Cost of goods sold", ledger.Expense, false, &m.COGSAccountID},
		{"5200", "Inventory adjustment", ledger.Expense, true, &m.AdjustmentAccountID},
	}
	for _, sp := range specs {
		if *sp.dest != nil {
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return Merchant{}, err
		}
		// Two first sales can enter this together. ON CONFLICT waits for the
		// other transaction, then keeps the row that won. A plain insert
		// would abort the loser's sale with a unique violation.
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounts (id, merchant_id, code, name, type, currency, allow_negative, status, balance_minor)
			VALUES ($1, $2, $3, $4, $5, 'NGN', $6, 'open', 0)
			ON CONFLICT (merchant_id, code) DO NOTHING
		`, id, merchantID, sp.code, sp.name, sp.typ, sp.negative); err != nil {
			return Merchant{}, err
		}
		var acctID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT id FROM accounts WHERE merchant_id = $1 AND code = $2
		`, merchantID, sp.code).Scan(&acctID); err != nil {
			return Merchant{}, err
		}
		kept := acctID
		*sp.dest = &kept
	}
	return scanMerchant(tx.QueryRow(ctx, `
		UPDATE merchants SET
			clearing_account_id = $2,
			vat_account_id = $3,
			equity_account_id = $4,
			inventory_account_id = $5,
			cogs_account_id = $6,
			wallet_account_id = $7,
			goods_received_account_id = $8,
			adjustment_account_id = $9
		WHERE id = $1
		RETURNING `+merchantCols+`
	`, merchantID, m.ClearingAccountID, m.VATAccountID, m.EquityAccountID,
		m.InventoryAccountID, m.COGSAccountID, m.WalletAccountID, m.GoodsReceivedAccountID, m.AdjustmentAccountID))
}

func (s *Store) CreateStaff(ctx context.Context, db DBTX, st Staff) (Staff, error) {
	err := db.QueryRow(ctx, `
		INSERT INTO staff (id, merchant_id, email, password_hash, role)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, merchant_id, email, password_hash, role, disabled_at, created_at
	`, st.ID, st.MerchantID, st.Email, st.PasswordHash, st.Role).Scan(
		&st.ID, &st.MerchantID, &st.Email, &st.PasswordHash, &st.Role, &st.DisabledAt, &st.CreatedAt,
	)
	return st, err
}

func (s *Store) GetStaffByEmail(ctx context.Context, db DBTX, email string) (Staff, error) {
	var st Staff
	err := db.QueryRow(ctx, `
		SELECT id, merchant_id, email, password_hash, role, disabled_at, created_at
		FROM staff WHERE email = $1
	`, email).Scan(&st.ID, &st.MerchantID, &st.Email, &st.PasswordHash, &st.Role, &st.DisabledAt, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ledger.Err(ledger.CodeNotFound, "staff not found")
	}
	return st, err
}

func (s *Store) GetStaffByID(ctx context.Context, db DBTX, id uuid.UUID) (Staff, error) {
	var st Staff
	err := db.QueryRow(ctx, `
		SELECT id, merchant_id, email, password_hash, role, disabled_at, created_at
		FROM staff WHERE id = $1
	`, id).Scan(&st.ID, &st.MerchantID, &st.Email, &st.PasswordHash, &st.Role, &st.DisabledAt, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ledger.Err(ledger.CodeNotFound, "staff not found")
	}
	return st, err
}

func (s *Store) ListStaff(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]Staff, error) {
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, email, password_hash, role, disabled_at, created_at
		FROM staff WHERE merchant_id = $1 ORDER BY created_at, email
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

func (s *Store) CountActiveAdmins(ctx context.Context, db DBTX, merchantID uuid.UUID) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM staff
		WHERE merchant_id = $1 AND role = 'admin' AND disabled_at IS NULL
	`, merchantID).Scan(&n)
	return n, err
}

func (s *Store) DisableStaff(ctx context.Context, db DBTX, merchantID, staffID uuid.UUID, at time.Time) (Staff, error) {
	var st Staff
	err := db.QueryRow(ctx, `
		UPDATE staff SET disabled_at = $3
		WHERE id = $1 AND merchant_id = $2 AND disabled_at IS NULL
		RETURNING id, merchant_id, email, password_hash, role, disabled_at, created_at
	`, staffID, merchantID, at).Scan(&st.ID, &st.MerchantID, &st.Email, &st.PasswordHash, &st.Role, &st.DisabledAt, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ledger.Err(ledger.CodeNotFound, "staff not found")
	}
	return st, err
}

func (s *Store) AssignStaffOutlet(ctx context.Context, db DBTX, staffID, outletID uuid.UUID) error {
	_, err := db.Exec(ctx, `
		INSERT INTO staff_outlets (staff_id, outlet_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, staffID, outletID)
	return err
}

func (s *Store) StaffHasOutlet(ctx context.Context, db DBTX, staffID, outletID, merchantID uuid.UUID) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM staff_outlets so
			JOIN outlets o ON o.id = so.outlet_id
			WHERE so.staff_id = $1 AND so.outlet_id = $2 AND o.merchant_id = $3
		)
	`, staffID, outletID, merchantID).Scan(&ok)
	return ok, err
}

func (s *Store) CreateOutlet(ctx context.Context, tx pgx.Tx, id, merchantID uuid.UUID, name, code string) (Outlet, error) {
	name = strings.TrimSpace(name)
	code = strings.ToUpper(strings.TrimSpace(code))
	if name == "" || code == "" {
		return Outlet{}, ledger.Err(ledger.CodeInvalidOrder, "outlet name and code are required")
	}
	// Outlet code, not a UUID prefix. UUID v7's first 8 hex digits are a
	// coarse timestamp and stay identical for about a minute, so two
	// branches opened together would share 1000-/4000- codes and the
	// second insert would violate UNIQUE (merchant_id, code).
	suffix := code
	cashID, err := uuid.NewV7()
	if err != nil {
		return Outlet{}, err
	}
	salesID, err := uuid.NewV7()
	if err != nil {
		return Outlet{}, err
	}
	if _, err := s.CreateAccount(ctx, tx, ledger.Account{
		ID: cashID, MerchantID: merchantID, Code: "1000-" + suffix, Name: "Cash · " + name,
		Type: ledger.Asset, Currency: "NGN", Status: ledger.AccountOpen,
	}); err != nil {
		return Outlet{}, err
	}
	if _, err := s.CreateAccount(ctx, tx, ledger.Account{
		ID: salesID, MerchantID: merchantID, Code: "4000-" + suffix, Name: "Sales · " + name,
		Type: ledger.Income, Currency: "NGN", Status: ledger.AccountOpen,
	}); err != nil {
		return Outlet{}, err
	}
	var o Outlet
	err = tx.QueryRow(ctx, `
		INSERT INTO outlets (id, merchant_id, name, code, cash_account_id, sales_account_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, merchant_id, name, code, cash_account_id, sales_account_id, created_at
	`, id, merchantID, name, code, cashID, salesID).Scan(
		&o.ID, &o.MerchantID, &o.Name, &o.Code, &o.CashAccountID, &o.SalesAccountID, &o.CreatedAt,
	)
	if err != nil {
		return Outlet{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET outlet_id = $2 WHERE id = $1 OR id = $3`, cashID, id, salesID); err != nil {
		return Outlet{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outlet_counters (outlet_id, n) VALUES ($1, 0)`, id); err != nil {
		return Outlet{}, err
	}
	return o, nil
}

func scanOutlet(sc interface{ Scan(...any) error }) (Outlet, error) {
	var o Outlet
	err := sc.Scan(&o.ID, &o.MerchantID, &o.Name, &o.Code, &o.CashAccountID, &o.SalesAccountID, &o.CreatedAt)
	return o, err
}

func (s *Store) GetOutlet(ctx context.Context, db DBTX, merchantID, outletID uuid.UUID) (Outlet, error) {
	row := db.QueryRow(ctx, `
		SELECT id, merchant_id, name, code, cash_account_id, sales_account_id, created_at
		FROM outlets WHERE id = $1 AND merchant_id = $2
	`, outletID, merchantID)
	o, err := scanOutlet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outlet{}, ledger.Err(ledger.CodeNotFound, "outlet not found")
	}
	return o, err
}

func (s *Store) ListOutlets(ctx context.Context, db DBTX, merchantID, staffID uuid.UUID, role string) ([]Outlet, error) {
	q := `
		SELECT id, merchant_id, name, code, cash_account_id, sales_account_id, created_at
		FROM outlets WHERE merchant_id = $1 ORDER BY code`
	args := []any{merchantID}
	if role != "admin" {
		q = `
			SELECT o.id, o.merchant_id, o.name, o.code, o.cash_account_id, o.sales_account_id, o.created_at
			FROM outlets o
			JOIN staff_outlets so ON so.outlet_id = o.id
			WHERE o.merchant_id = $1 AND so.staff_id = $2
			ORDER BY o.code`
		args = append(args, staffID)
	}
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outlet
	for rows.Next() {
		o, err := scanOutlet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) CreateProduct(ctx context.Context, db DBTX, p Product) (Product, error) {
	err := db.QueryRow(ctx, `
		INSERT INTO products (id, merchant_id, sku, name, price_minor, avg_cost_minor, reorder_level, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, merchant_id, sku, name, price_minor, avg_cost_minor, reorder_level, active, created_at
	`, p.ID, p.MerchantID, p.SKU, p.Name, p.PriceMinor, p.AvgCostMinor, p.ReorderLevel, p.Active).Scan(
		&p.ID, &p.MerchantID, &p.SKU, &p.Name, &p.PriceMinor, &p.AvgCostMinor, &p.ReorderLevel, &p.Active, &p.CreatedAt,
	)
	return p, err
}

const productCols = `id, merchant_id, sku, name, price_minor, avg_cost_minor, reorder_level, active, created_at`

func scanProduct(sc interface{ Scan(...any) error }) (Product, error) {
	var p Product
	err := sc.Scan(&p.ID, &p.MerchantID, &p.SKU, &p.Name, &p.PriceMinor, &p.AvgCostMinor, &p.ReorderLevel, &p.Active, &p.CreatedAt)
	return p, err
}

func (s *Store) GetProductBySKU(ctx context.Context, db DBTX, merchantID uuid.UUID, sku string) (Product, error) {
	row := db.QueryRow(ctx, `
		SELECT `+productCols+`
		FROM products WHERE merchant_id = $1 AND sku = $2
	`, merchantID, sku)
	p, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ledger.Err(ledger.CodeNotFound, "product not found")
	}
	return p, err
}

func (s *Store) GetProduct(ctx context.Context, db DBTX, merchantID, id uuid.UUID) (Product, error) {
	row := db.QueryRow(ctx, `
		SELECT `+productCols+`
		FROM products WHERE id = $1 AND merchant_id = $2
	`, id, merchantID)
	p, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ledger.Err(ledger.CodeNotFound, "product not found")
	}
	return p, err
}

func (s *Store) ListProducts(ctx context.Context, db DBTX, merchantID uuid.UUID) ([]Product, error) {
	rows, err := db.Query(ctx, `
		SELECT `+productCols+`
		FROM products WHERE merchant_id = $1 ORDER BY sku
	`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdateProduct(ctx context.Context, db DBTX, p Product) (Product, error) {
	row := db.QueryRow(ctx, `
		UPDATE products
		SET name = $3, price_minor = $4, avg_cost_minor = $5, reorder_level = $6, active = $7
		WHERE id = $1 AND merchant_id = $2
		RETURNING `+productCols+`
	`, p.ID, p.MerchantID, p.Name, p.PriceMinor, p.AvgCostMinor, p.ReorderLevel, p.Active)
	out, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ledger.Err(ledger.CodeNotFound, "product not found")
	}
	return out, err
}

func (s *Store) ReceiveStock(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, qty int) (int, error) {
	if qty <= 0 {
		return 0, ledger.Err(ledger.CodeInvalidOrder, "qty must be > 0")
	}
	var onHand int
	err := tx.QueryRow(ctx, `
		INSERT INTO stock_levels (outlet_id, product_id, on_hand, reserved)
		VALUES ($1, $2, $3, 0)
		ON CONFLICT (outlet_id, product_id) DO UPDATE
		SET on_hand = stock_levels.on_hand + EXCLUDED.on_hand
		RETURNING on_hand
	`, outletID, productID, qty).Scan(&onHand)
	return onHand, err
}

func (s *Store) AdjustStock(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, delta int) (int, error) {
	var onHand int
	err := tx.QueryRow(ctx, `
		UPDATE stock_levels
		SET on_hand = on_hand + $3
		WHERE outlet_id = $1 AND product_id = $2
		  AND on_hand + $3 >= reserved
		  AND on_hand + $3 >= 0
		RETURNING on_hand
	`, outletID, productID, delta).Scan(&onHand)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM stock_levels WHERE outlet_id=$1 AND product_id=$2)`, outletID, productID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, ledger.Err(ledger.CodeNotFound, "stock row not found")
		}
		return 0, ledger.Err(ledger.CodeInsufficientStock, "adjustment would make stock unusable")
	}
	return onHand, err
}

func (s *Store) LockStock(ctx context.Context, tx pgx.Tx, outletID uuid.UUID, productIDs []uuid.UUID) error {
	if len(productIDs) == 0 {
		return ledger.Err(ledger.CodeInvalidOrder, "no stock to lock")
	}
	ids := append([]uuid.UUID(nil), productIDs...)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	rows, err := tx.Query(ctx, `
		SELECT product_id FROM stock_levels
		WHERE outlet_id = $1 AND product_id = ANY($2)
		ORDER BY product_id
		FOR UPDATE
	`, outletID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) DeductStock(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, qty int) error {
	tag, err := tx.Exec(ctx, `
		UPDATE stock_levels
		SET on_hand = on_hand - $3
		WHERE outlet_id = $1 AND product_id = $2
		  AND on_hand - reserved >= $3
	`, outletID, productID, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInsufficientStock, "not enough stock on hand")
	}
	return nil
}

func (s *Store) ReserveStock(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, qty int) error {
	tag, err := tx.Exec(ctx, `
		UPDATE stock_levels
		SET reserved = reserved + $3
		WHERE outlet_id = $1 AND product_id = $2
		  AND on_hand - reserved >= $3
	`, outletID, productID, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInsufficientStock, "not enough stock to reserve")
	}
	return nil
}

func (s *Store) CommitReserved(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, qty int) error {
	tag, err := tx.Exec(ctx, `
		UPDATE stock_levels
		SET on_hand = on_hand - $3, reserved = reserved - $3
		WHERE outlet_id = $1 AND product_id = $2
		  AND reserved >= $3 AND on_hand >= $3
	`, outletID, productID, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInsufficientStock, "reservation could not be committed")
	}
	return nil
}

func (s *Store) ReleaseReserved(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, qty int) error {
	tag, err := tx.Exec(ctx, `
		UPDATE stock_levels
		SET reserved = reserved - $3
		WHERE outlet_id = $1 AND product_id = $2 AND reserved >= $3
	`, outletID, productID, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInsufficientStock, "reservation could not be released")
	}
	return nil
}

func (s *Store) Restock(ctx context.Context, tx pgx.Tx, outletID, productID uuid.UUID, qty int) error {
	tag, err := tx.Exec(ctx, `
		UPDATE stock_levels SET on_hand = on_hand + $3
		WHERE outlet_id = $1 AND product_id = $2
	`, outletID, productID, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeNotFound, "stock row not found")
	}
	return nil
}

func (s *Store) NextReference(ctx context.Context, tx pgx.Tx, outletID uuid.UUID, code string) (string, error) {
	var n int
	err := tx.QueryRow(ctx, `
		INSERT INTO outlet_counters (outlet_id, n) VALUES ($1, 1)
		ON CONFLICT (outlet_id) DO UPDATE SET n = outlet_counters.n + 1
		RETURNING n
	`, outletID).Scan(&n)
	if err != nil {
		return "", err
	}
	// Outlet codes repeat across merchants (every shop can have IKJ).
	// The reference is globally unique because Paystack sends it back
	// with no merchant id. The last 8 hex digits of a UUID v7 are random,
	// so two Ikeja tills do not both mint IKJ-000001.
	compact := strings.ReplaceAll(outletID.String(), "-", "")
	tail := compact
	if len(compact) >= 8 {
		tail = compact[len(compact)-8:]
	}
	return fmt.Sprintf("%s-%s-%06d", code, strings.ToUpper(tail), n), nil
}

func scanOrder(sc interface{ Scan(...any) error }) (Order, error) {
	var o Order
	err := sc.Scan(
		&o.ID, &o.MerchantID, &o.OutletID, &o.StaffID, &o.Status, &o.PaymentMethod, &o.Currency,
		&o.TotalMinor, &o.VATMinor, &o.NetMinor, &o.DiscountMinor, &o.CostMinor, &o.CustomerID,
		&o.Reference, &o.IdempotencyKey, &o.JournalID,
		&o.ExpiresAt, &o.PaidAt, &o.CreatedAt,
	)
	return o, err
}

const orderSelect = `id, merchant_id, outlet_id, staff_id, status, payment_method, currency,
	total_minor, vat_minor, net_minor, discount_minor, cost_minor, customer_id,
	reference, COALESCE(idempotency_key, ''), journal_id,
	expires_at, paid_at, created_at`

func (s *Store) InsertOrder(ctx context.Context, tx pgx.Tx, o Order) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO orders (
			id, merchant_id, outlet_id, staff_id, status, payment_method, currency,
			total_minor, vat_minor, net_minor, discount_minor, cost_minor, customer_id,
			reference, idempotency_key, journal_id, expires_at, paid_at, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,'NGN',$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),$15,$16,$17,$18)
	`, o.ID, o.MerchantID, o.OutletID, o.StaffID, o.Status, o.PaymentMethod,
		o.TotalMinor, o.VATMinor, o.NetMinor, o.DiscountMinor, o.CostMinor, o.CustomerID,
		o.Reference, o.IdempotencyKey, o.JournalID, o.ExpiresAt, o.PaidAt, o.CreatedAt)
	return err
}

func (s *Store) InsertOrderLine(ctx context.Context, tx pgx.Tx, ln OrderLine) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO order_lines (id, order_id, product_id, sku, qty, unit_price_minor, line_total_minor)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, ln.ID, ln.OrderID, ln.ProductID, ln.SKU, ln.Qty, ln.UnitPriceMinor, ln.LineTotalMinor)
	return err
}

func (s *Store) AttachJournal(ctx context.Context, tx pgx.Tx, orderID, journalID uuid.UUID, paidAt time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE orders SET journal_id = $2, status = 'paid', paid_at = $3 WHERE id = $1
	`, orderID, journalID, paidAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeNotFound, "order not found")
	}
	return nil
}

func (s *Store) loadLines(ctx context.Context, db DBTX, orders []Order) ([]Order, error) {
	if len(orders) == 0 {
		return orders, nil
	}
	ids := make([]uuid.UUID, len(orders))
	byID := map[uuid.UUID]int{}
	for i, o := range orders {
		ids[i] = o.ID
		byID[o.ID] = i
	}
	rows, err := db.Query(ctx, `
		SELECT id, order_id, product_id, sku, qty, unit_price_minor, line_total_minor
		FROM order_lines WHERE order_id = ANY($1) ORDER BY sku
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ln OrderLine
		if err := rows.Scan(&ln.ID, &ln.OrderID, &ln.ProductID, &ln.SKU, &ln.Qty, &ln.UnitPriceMinor, &ln.LineTotalMinor); err != nil {
			return nil, err
		}
		i := byID[ln.OrderID]
		orders[i].Lines = append(orders[i].Lines, ln)
	}
	return orders, rows.Err()
}

func (s *Store) GetOrder(ctx context.Context, db DBTX, merchantID, orderID uuid.UUID) (Order, error) {
	row := db.QueryRow(ctx, `SELECT `+orderSelect+` FROM orders WHERE id = $1 AND merchant_id = $2`, orderID, merchantID)
	o, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ledger.Err(ledger.CodeNotFound, "order not found")
	}
	if err != nil {
		return Order{}, err
	}
	loaded, err := s.loadLines(ctx, db, []Order{o})
	if err != nil {
		return Order{}, err
	}
	return loaded[0], nil
}

func (s *Store) LockOrder(ctx context.Context, tx pgx.Tx, merchantID, orderID uuid.UUID) (Order, error) {
	row := tx.QueryRow(ctx, `SELECT `+orderSelect+` FROM orders WHERE id = $1 AND merchant_id = $2 FOR UPDATE`, orderID, merchantID)
	o, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ledger.Err(ledger.CodeNotFound, "order not found")
	}
	if err != nil {
		return Order{}, err
	}
	loaded, err := s.loadLines(ctx, tx, []Order{o})
	if err != nil {
		return Order{}, err
	}
	return loaded[0], nil
}

func (s *Store) GetOrderByReference(ctx context.Context, db DBTX, reference string) (Order, error) {
	row := db.QueryRow(ctx, `SELECT `+orderSelect+` FROM orders WHERE reference = $1`, reference)
	o, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ledger.Err(ledger.CodeNotFound, "order not found")
	}
	if err != nil {
		return Order{}, err
	}
	loaded, err := s.loadLines(ctx, db, []Order{o})
	if err != nil {
		return Order{}, err
	}
	return loaded[0], nil
}

func (s *Store) ListOrders(ctx context.Context, db DBTX, merchantID uuid.UUID, outletID *uuid.UUID, limit int) ([]Order, error) {
	rows, err := db.Query(ctx, `
		SELECT `+orderSelect+`
		FROM orders
		WHERE merchant_id = $1 AND ($2::uuid IS NULL OR outlet_id = $2)
		ORDER BY created_at DESC
		LIMIT $3
	`, merchantID, outletID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.loadLines(ctx, db, out)
}

func (s *Store) MarkRefunded(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'refunded' WHERE id = $1 AND status = 'paid'`, orderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInvalidOrder, "only a paid order can be refunded")
	}
	return nil
}

func (s *Store) MarkExpired(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'expired' WHERE id = $1 AND status = 'pending_payment'`, orderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ledger.Err(ledger.CodeInvalidOrder, "order is not pending")
	}
	return nil
}

func (s *Store) JournalUsedByOrder(ctx context.Context, db DBTX, merchantID, journalID uuid.UUID) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM orders WHERE merchant_id = $1 AND journal_id = $2)
	`, merchantID, journalID).Scan(&ok)
	return ok, err
}

func (s *Store) InsertReservation(ctx context.Context, tx pgx.Tx, id, merchantID, outletID, orderID, productID uuid.UUID, qty int, expires time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO stock_reservations (id, merchant_id, outlet_id, order_id, product_id, qty, status, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,'active',$7)
	`, id, merchantID, outletID, orderID, productID, qty, expires)
	return err
}

func (s *Store) ReservationsByOrder(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) ([]Reservation, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, product_id, qty, status FROM stock_reservations
		WHERE order_id = $1 ORDER BY product_id
	`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.ID, &r.ProductID, &r.Qty, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetReservationsStatus(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, from, to string) error {
	_, err := tx.Exec(ctx, `
		UPDATE stock_reservations SET status = $3 WHERE order_id = $1 AND status = $2
	`, orderID, from, to)
	return err
}

func (s *Store) InsertPayment(ctx context.Context, tx pgx.Tx, id, orderID uuid.UUID, method, status string, amount int64, provider string, eventID *string, journalID *uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payments (id, order_id, method, status, amount_minor, provider, provider_event_id, journal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, id, orderID, method, status, amount, provider, eventID, journalID)
	return err
}

func (s *Store) ConfirmPayment(ctx context.Context, tx pgx.Tx, orderID, journalID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE payments SET status = 'confirmed', journal_id = $2
		WHERE order_id = $1 AND status = 'pending'
	`, orderID, journalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("confirm payment: expected 1 row")
	}
	return nil
}

func (s *Store) InsertAudit(ctx context.Context, tx pgx.Tx, ev AuditEvent) error {
	if ev.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		ev.ID = id
	}
	if len(ev.Metadata) == 0 {
		ev.Metadata = []byte(`{}`)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (id, merchant_id, staff_id, action, entity_type, entity_id, outlet_id, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, ev.ID, ev.MerchantID, ev.StaffID, ev.Action, ev.EntityType, ev.EntityID, ev.OutletID, ev.Metadata)
	return err
}

func (s *Store) ListAudit(ctx context.Context, db DBTX, merchantID uuid.UUID, outletID *uuid.UUID, limit int) ([]AuditEvent, error) {
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, staff_id, action, entity_type, entity_id, outlet_id, metadata, created_at
		FROM audit_events
		WHERE merchant_id = $1 AND ($2::uuid IS NULL OR outlet_id = $2)
		ORDER BY created_at DESC
		LIMIT $3
	`, merchantID, outletID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		if err := rows.Scan(&ev.ID, &ev.MerchantID, &ev.StaffID, &ev.Action, &ev.EntityType, &ev.EntityID, &ev.OutletID, &ev.Metadata, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) LockExpiredOrders(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Order, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+orderSelect+`
		FROM orders
		WHERE status = 'pending_payment' AND expires_at < $1
		ORDER BY id
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) DailySales(ctx context.Context, db DBTX, outletID uuid.UUID, day string) (DailySales, error) {
	var ds DailySales
	err := db.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_minor),0), COALESCE(SUM(vat_minor),0), COALESCE(SUM(net_minor),0)
		FROM orders
		WHERE outlet_id = $1 AND status = 'paid'
		  AND (paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
	`, outletID, day).Scan(&ds.OrderCount, &ds.TotalMinor, &ds.VATMinor, &ds.NetMinor)
	if err != nil {
		return DailySales{}, err
	}
	err = db.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount_minor),0)
		FROM orders o
		JOIN postings p ON p.journal_id = o.journal_id AND p.direction = 'debit'
		JOIN accounts a ON a.id = p.account_id AND a.type IN ('asset', 'liability')
		WHERE o.outlet_id = $1 AND o.status = 'paid'
		  AND (o.paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
	`, outletID, day).Scan(&ds.BooksDebit)
	if err != nil {
		return DailySales{}, err
	}
	rows, err := db.Query(ctx, `
		SELECT payment_method, COUNT(*), COALESCE(SUM(total_minor),0), COALESCE(SUM(vat_minor),0), COALESCE(SUM(net_minor),0)
		FROM orders
		WHERE outlet_id = $1 AND status = 'paid'
		  AND (paid_at AT TIME ZONE 'Africa/Lagos')::date = $2::date
		GROUP BY payment_method
		ORDER BY payment_method
	`, outletID, day)
	if err != nil {
		return DailySales{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MethodTotal
		if err := rows.Scan(&m.Method, &m.OrderCount, &m.TotalMinor, &m.VATMinor, &m.NetMinor); err != nil {
			return DailySales{}, err
		}
		ds.ByMethod = append(ds.ByMethod, m)
	}
	return ds, rows.Err()
}

func (s *Store) StockOnHand(ctx context.Context, db DBTX, outletID uuid.UUID, lowOnly bool) ([]StockRow, error) {
	q := `
		SELECT p.id, p.sku, p.name, s.on_hand, s.reserved, s.on_hand - s.reserved, p.reorder_level, p.price_minor, p.avg_cost_minor
		FROM stock_levels s
		JOIN products p ON p.id = s.product_id
		WHERE s.outlet_id = $1 AND p.active`
	if lowOnly {
		q += ` AND (s.on_hand - s.reserved) <= p.reorder_level`
	}
	q += ` ORDER BY p.sku`
	rows, err := db.Query(ctx, q, outletID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StockRow
	for rows.Next() {
		var r StockRow
		if err := rows.Scan(&r.ProductID, &r.SKU, &r.Name, &r.OnHand, &r.Reserved, &r.Available, &r.ReorderLevel, &r.PriceMinor, &r.AvgCostMinor); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
