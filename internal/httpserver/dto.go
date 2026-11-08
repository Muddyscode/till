package httpserver

import (
	"time"

	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
)

type staffJSON struct {
	ID         uuid.UUID  `json:"id"`
	MerchantID uuid.UUID  `json:"merchant_id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func asStaff(st store.Staff) staffJSON {
	return staffJSON{
		ID: st.ID, MerchantID: st.MerchantID, Email: st.Email, Role: st.Role,
		DisabledAt: st.DisabledAt, CreatedAt: st.CreatedAt,
	}
}

type accountJSON struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	Currency      string    `json:"currency"`
	AllowNegative bool      `json:"allow_negative"`
	Status        string    `json:"status"`
	BalanceMinor  int64     `json:"balance_minor"`
	CreatedAt     time.Time `json:"created_at"`
}

func acctJSON(a ledger.Account) accountJSON {
	return accountJSON{
		ID: a.ID, Code: a.Code, Name: a.Name, Type: string(a.Type), Currency: a.Currency,
		AllowNegative: a.AllowNegative, Status: string(a.Status), BalanceMinor: a.BalanceMinor, CreatedAt: a.CreatedAt,
	}
}

type postingJSON struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Direction string    `json:"direction"`
	Amount    int64     `json:"amount_minor"`
	Currency  string    `json:"currency"`
}

type journalJSON struct {
	ID                uuid.UUID     `json:"id"`
	Description       string        `json:"description"`
	CreatedAt         time.Time     `json:"created_at"`
	ReversesJournalID *uuid.UUID    `json:"reverses_journal_id"`
	Postings          []postingJSON `json:"postings"`
}

func jJSON(j ledger.Journal) journalJSON {
	ps := make([]postingJSON, 0, len(j.Postings))
	for _, p := range j.Postings {
		ps = append(ps, postingJSON{
			ID: p.ID, AccountID: p.AccountID, Direction: string(p.Direction), Amount: p.Amount, Currency: p.Currency,
		})
	}
	return journalJSON{
		ID: j.ID, Description: j.Description, CreatedAt: j.CreatedAt,
		ReversesJournalID: j.ReversesJournalID, Postings: ps,
	}
}
