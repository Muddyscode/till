package ledger

import (
	"time"

	"github.com/google/uuid"
)

type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Equity    AccountType = "equity"
	Income    AccountType = "income"
	Expense   AccountType = "expense"
)

func (t AccountType) Valid() bool {
	switch t {
	case Asset, Liability, Equity, Income, Expense:
		return true
	}
	return false
}

func (t AccountType) DebitNormal() bool {
	return t == Asset || t == Expense
}

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

func (d Direction) Valid() bool {
	return d == Debit || d == Credit
}

type AccountStatus string

const (
	AccountOpen   AccountStatus = "open"
	AccountClosed AccountStatus = "closed"
)

type Account struct {
	ID            uuid.UUID
	MerchantID    uuid.UUID
	Code          string
	Name          string
	Type          AccountType
	Currency      string
	AllowNegative bool
	Status        AccountStatus
	BalanceMinor  int64
	CreatedAt     time.Time
}

type Line struct {
	AccountID uuid.UUID
	Direction Direction
	Amount    int64
}

type Posting struct {
	ID        uuid.UUID
	JournalID uuid.UUID
	AccountID uuid.UUID
	Direction Direction
	Amount    int64
	Currency  string
}

type Journal struct {
	ID                uuid.UUID
	MerchantID        uuid.UUID
	Status            string
	Description       string
	IdempotencyKey    string
	ReversesJournalID *uuid.UUID
	CreatedAt         time.Time
	Postings          []Posting
}

type PostRequest struct {
	MerchantID           uuid.UUID
	Description          string
	Lines                []Line
	ReversesJournalID    *uuid.UUID
	EnforceTransferPairs bool
	IdempotencyKey       string
}

func NaturalDelta(t AccountType, d Direction, amount int64) int64 {
	if t.DebitNormal() {
		if d == Debit {
			return amount
		}
		return -amount
	}
	if d == Credit {
		return amount
	}
	return -amount
}
