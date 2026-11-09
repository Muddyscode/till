package store

import (
	"context"
	"errors"
	"math"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WeightedAverage is (onHand * avg + qty * unit) / (onHand + qty), rounded down.
func WeightedAverage(onHand int, avg int64, qty int, unit int64) (int64, error) {
	if qty < 0 || onHand < 0 || avg < 0 || unit < 0 {
		return 0, ledger.Err(ledger.CodeInvalidAmount, "cost cannot be negative")
	}
	totalQty := onHand + qty
	if totalQty <= 0 {
		return 0, ledger.Err(ledger.CodeInvalidOrder, "average cost needs units")
	}
	if avg > 0 && int64(onHand) > math.MaxInt64/avg {
		return 0, ledger.Err(ledger.CodeInvalidAmount, "average cost overflows")
	}
	if unit > 0 && int64(qty) > math.MaxInt64/unit {
		return 0, ledger.Err(ledger.CodeInvalidAmount, "receipt cost overflows")
	}
	sum := int64(onHand)*avg + int64(qty)*unit
	return sum / int64(totalQty), nil
}

func (s *Store) LockProduct(ctx context.Context, tx pgx.Tx, merchantID, productID uuid.UUID) (Product, error) {
	row := tx.QueryRow(ctx, `SELECT `+productCols+` FROM products WHERE id = $1 AND merchant_id = $2 FOR UPDATE`, productID, merchantID)
	p, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ledger.Err(ledger.CodeNotFound, "product not found")
	}
	return p, err
}

func (s *Store) SumOnHand(ctx context.Context, tx pgx.Tx, productID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(on_hand), 0) FROM stock_levels WHERE product_id = $1`, productID).Scan(&n)
	return n, err
}

// LockSaleCost locks the products in id order and returns qty * average cost, and sets each line's CostMinor in place.
func (s *Store) LockSaleCost(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, lines []OrderLine) (int64, error) {
	ids := make([]uuid.UUID, 0, len(lines))
	seen := map[uuid.UUID]struct{}{}
	for _, ln := range lines {
		if _, ok := seen[ln.ProductID]; ok {
			continue
		}
		seen[ln.ProductID] = struct{}{}
		ids = append(ids, ln.ProductID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id, avg_cost_minor FROM products
		WHERE merchant_id = $1 AND id = ANY($2)
		ORDER BY id
		FOR UPDATE
	`, merchantID, ids)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	costByID := map[uuid.UUID]int64{}
	for rows.Next() {
		var id uuid.UUID
		var avg int64
		if err := rows.Scan(&id, &avg); err != nil {
			return 0, err
		}
		costByID[id] = avg
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var cost int64
	for i, ln := range lines {
		avg, ok := costByID[ln.ProductID]
		if !ok {
			return 0, ledger.Err(ledger.CodeNotFound, "product not found")
		}
		lines[i].CostMinor = 0
		if avg == 0 || ln.Qty == 0 {
			continue
		}
		if int64(ln.Qty) > math.MaxInt64/avg || cost > math.MaxInt64-int64(ln.Qty)*avg {
			return 0, ledger.Err(ledger.CodeInvalidAmount, "cost of goods overflows")
		}
		lines[i].CostMinor = int64(ln.Qty) * avg
		cost += lines[i].CostMinor
	}
	return cost, nil
}

func (s *Store) SetOrderCost(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, cost int64) error {
	_, err := tx.Exec(ctx, `UPDATE orders SET cost_minor = $2 WHERE id = $1`, orderID, cost)
	return err
}
