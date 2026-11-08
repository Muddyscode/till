package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StorePoster struct {
	LockAccounts  func(ctx context.Context, tx pgx.Tx, operatorID uuid.UUID, ids []uuid.UUID) ([]Account, error)
	InsertJournal func(ctx context.Context, tx pgx.Tx, j Journal) error
	InsertPosting func(ctx context.Context, tx pgx.Tx, p Posting) error
	UpdateBalance func(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, balance int64) error
}

func Post(ctx context.Context, tx pgx.Tx, st StorePoster, req PostRequest) (Journal, error) {
	if err := ValidateLines(req.Lines); err != nil {
		return Journal{}, err
	}
	ids := make([]uuid.UUID, len(req.Lines))
	for i, l := range req.Lines {
		ids[i] = l.AccountID
	}
	accts, err := st.LockAccounts(ctx, tx, req.MerchantID, ids)
	if err != nil {
		return Journal{}, err
	}
	byID := map[uuid.UUID]Account{}
	for _, a := range accts {
		byID[a.ID] = a
	}

	if req.EnforceTransferPairs {
		if len(req.Lines) != 2 {
			return Journal{}, Err(CodeInvalidJournal, "transfers must have exactly two postings")
		}
		var debitType, creditType AccountType
		for _, l := range req.Lines {
			if l.Direction == Debit {
				debitType = byID[l.AccountID].Type
			} else {
				creditType = byID[l.AccountID].Type
			}
		}
		if !TransferPairAllowed(debitType, creditType) {
			return Journal{}, Err(CodePairNotAllowed, fmt.Sprintf("transfer debit %s / credit %s is not allowed", debitType, creditType))
		}
	}

	newBal := map[uuid.UUID]int64{}
	for _, a := range accts {
		newBal[a.ID] = a.BalanceMinor
	}

	for _, l := range req.Lines {
		a, ok := byID[l.AccountID]
		if !ok {
			return Journal{}, Err(CodeNotFound, "account not found")
		}
		if a.Status != AccountOpen {
			return Journal{}, Err(CodeClosedAccount, "account "+a.Code+" is closed")
		}
		if a.Currency != "NGN" {
			return Journal{}, Err(CodeCurrencyMismatch, "only NGN is supported")
		}
		nb := a.BalanceMinor + NaturalDelta(a.Type, l.Direction, l.Amount)
		if nb < 0 && !a.AllowNegative {
			return Journal{}, Err(CodeInsufficientFunds, "account "+a.Code+" would go negative")
		}
		newBal[a.ID] = nb
		a.BalanceMinor = nb
		byID[a.ID] = a
	}

	jid, err := uuid.NewV7()
	if err != nil {
		return Journal{}, err
	}
	j := Journal{
		ID:                jid,
		MerchantID:        req.MerchantID,
		Status:            "posted",
		Description:       req.Description,
		IdempotencyKey:    req.IdempotencyKey,
		ReversesJournalID: req.ReversesJournalID,
	}
	if err := st.InsertJournal(ctx, tx, j); err != nil {
		return Journal{}, err
	}
	for _, l := range req.Lines {
		pid, err := uuid.NewV7()
		if err != nil {
			return Journal{}, err
		}
		p := Posting{
			ID:        pid,
			JournalID: jid,
			AccountID: l.AccountID,
			Direction: l.Direction,
			Amount:    l.Amount,
			Currency:  "NGN",
		}
		if err := st.InsertPosting(ctx, tx, p); err != nil {
			return Journal{}, err
		}
		j.Postings = append(j.Postings, p)
	}
	for id, bal := range newBal {
		if err := st.UpdateBalance(ctx, tx, id, bal); err != nil {
			return Journal{}, err
		}
	}
	return j, nil
}

func ReverseLines(original []Posting) []Line {
	out := make([]Line, len(original))
	for i, p := range original {
		d := Debit
		if p.Direction == Debit {
			d = Credit
		}
		out[i] = Line{AccountID: p.AccountID, Direction: d, Amount: p.Amount}
	}
	return out
}
