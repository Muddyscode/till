package seed

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	adminEmail = "admin@till.local"
	ikejaCash  = int64(5_000_000)
	sulCash    = int64(2_000_000)
)

type Spec struct {
	Password string
}

// Run seeds the demo merchant under the same advisory lock the migrations use,
// so two instances booting together cannot both create it.
func Run(ctx context.Context, st *store.Store, spec Spec, log *slog.Logger) error {
	return store.WithAdvisoryLock(ctx, st.Pool, func(ctx context.Context) error {
		return run(ctx, st, spec, log)
	})
}

// Signing secrets of the seeded demo connections (development only).
const (
	DemoTillSecret     = "dev-till-secret-change-me"
	DemoPaystackSecret = "dev-paystack-secret-change-me"
)

func run(ctx context.Context, st *store.Store, spec Spec, log *slog.Logger) error {
	if _, err := st.GetStaffByEmail(ctx, st.Pool, adminEmail); err == nil {
		log.Info("seed already present", "email", adminEmail)
		return nil
	}
	hash, err := auth.HashPassword(spec.Password)
	if err != nil {
		return err
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		mid, err := uuid.NewV7()
		if err != nil {
			return err
		}
		merchant, err := st.CreateMerchant(ctx, tx, mid, "Adaeze Studio", 750)
		if err != nil {
			return err
		}
		merchant, err = st.EnsureBooks(ctx, tx, merchant.ID)
		if err != nil {
			return err
		}
		ikeja, err := makeOutlet(ctx, tx, st, merchant.ID, "Ikeja", "IKJ")
		if err != nil {
			return err
		}
		sul, err := makeOutlet(ctx, tx, st, merchant.ID, "Surulere", "SUL")
		if err != nil {
			return err
		}
		// Two demo payment connections. Their secrets are development values:
		// production refuses to seed at all.
		for _, c := range []store.Connection{
			{Code: "POS", Name: "Ikeja card and tap terminal", Provider: "till", Secret: DemoTillSecret, OutletID: &ikeja.ID},
			{Code: "PSTK", Name: "Paystack", Provider: "paystack", Secret: DemoPaystackSecret},
		} {
			if c.ID, err = uuid.NewV7(); err != nil {
				return err
			}
			c.MerchantID = merchant.ID
			if _, err := st.CreateConnection(ctx, tx, c, nil); err != nil {
				return err
			}
		}
		if err := addStaff(ctx, tx, st, merchant.ID, adminEmail, hash, auth.RoleAdmin, nil); err != nil {
			return err
		}
		if err := addStaff(ctx, tx, st, merchant.ID, "manager.ikeja@till.local", hash, auth.RoleManager, []uuid.UUID{ikeja.ID}); err != nil {
			return err
		}
		if err := addStaff(ctx, tx, st, merchant.ID, "cashier.ikeja@till.local", hash, auth.RoleCashier, []uuid.UUID{ikeja.ID}); err != nil {
			return err
		}
		if err := addStaff(ctx, tx, st, merchant.ID, "cashier.surulere@till.local", hash, auth.RoleCashier, []uuid.UUID{sul.ID}); err != nil {
			return err
		}
		type item struct {
			sku, name   string
			price, cost int64
			reorder     int
			ikeja, sul  int
		}
		catalog := []item{
			{"SERUM-C", "Vitamin C serum", 850_000, 320_000, 2, 20, 20},
			{"SHEA-500", "Shea moisturizer 500ml", 450_000, 180_000, 2, 20, 12},
			{"TOWEL", "Face towel", 120_000, 40_000, 2, 30, 15},
			{"SOAP", "Castile soap", 200_000, 70_000, 5, 3, 20},
		}
		var inventory int64
		for _, it := range catalog {
			pid, err := uuid.NewV7()
			if err != nil {
				return err
			}
			p, err := st.CreateProduct(ctx, tx, store.Product{
				ID: pid, MerchantID: merchant.ID, SKU: it.sku, Name: it.name,
				PriceMinor: it.price, AvgCostMinor: it.cost, ReorderLevel: it.reorder, Active: true,
			})
			if err != nil {
				return err
			}
			// Quantity only. A receipt journal would book this stock a second time.
			// The opening journal below is the one debit to inventory.
			if _, err := st.ReceiveStock(ctx, tx, ikeja.ID, p.ID, it.ikeja); err != nil {
				return err
			}
			if _, err := st.ReceiveStock(ctx, tx, sul.ID, p.ID, it.sul); err != nil {
				return err
			}
			inventory += it.cost * int64(it.ikeja+it.sul)
		}
		if merchant.EquityAccountID == nil || merchant.InventoryAccountID == nil {
			return fmt.Errorf("seed: books are missing")
		}
		_, err = ledger.Post(ctx, tx, st.Poster(), ledger.PostRequest{
			MerchantID:  merchant.ID,
			Description: "opening float and inventory",
			Lines: []ledger.Line{
				{AccountID: ikeja.CashAccountID, Direction: ledger.Debit, Amount: ikejaCash},
				{AccountID: sul.CashAccountID, Direction: ledger.Debit, Amount: sulCash},
				{AccountID: *merchant.InventoryAccountID, Direction: ledger.Debit, Amount: inventory},
				{AccountID: *merchant.EquityAccountID, Direction: ledger.Credit, Amount: ikejaCash + sulCash + inventory},
			},
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	log.Info("seeded Adaeze Studio", "email", adminEmail)
	return nil
}

func makeOutlet(ctx context.Context, tx pgx.Tx, st *store.Store, merchantID uuid.UUID, name, code string) (store.Outlet, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.Outlet{}, err
	}
	return st.CreateOutlet(ctx, tx, id, merchantID, name, code)
}

func addStaff(ctx context.Context, tx pgx.Tx, st *store.Store, merchantID uuid.UUID, email, hash, role string, outlets []uuid.UUID) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	created, err := st.CreateStaff(ctx, tx, store.Staff{
		ID: id, MerchantID: merchantID, Email: email, PasswordHash: hash, Role: role,
	})
	if err != nil {
		return err
	}
	for _, oid := range outlets {
		if err := st.AssignStaffOutlet(ctx, tx, created.ID, oid); err != nil {
			return err
		}
	}
	return nil
}
