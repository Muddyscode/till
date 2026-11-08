package store

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SearchCustomers lists customers by name (substring) or phone (digit substring).
// An empty q lists everyone alphabetically.
func (s *Store) SearchCustomers(ctx context.Context, db DBTX, merchantID uuid.UUID, q string, limit int) ([]Customer, error) {
	q = strings.TrimSpace(q)
	namePat, phonePat := "%", ""
	if q != "" {
		namePat = "%" + escapeLike(q) + "%"
		if d := PhoneSearchDigits(q); len(d) >= 3 {
			phonePat = "%" + d + "%"
		}
	}
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, name, phone, balance_minor, created_at
		FROM customers
		WHERE merchant_id = $1
		  AND (name ILIKE $2 OR ($3 <> '' AND phone LIKE $3))
		ORDER BY name, id
		LIMIT $4
	`, merchantID, namePat, phonePat, limit)
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
