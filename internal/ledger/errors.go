package ledger

import "fmt"

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func Err(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

const (
	CodeUnbalanced            = "unbalanced_journal"
	CodeInsufficientFunds     = "insufficient_funds"
	CodeIdempotencyConflict   = "idempotency_conflict"
	CodeIdempotencyInProgress = "idempotency_in_progress"
	CodeClosedAccount         = "closed_account"
	CodeInvalidAmount         = "invalid_amount"
	CodeCurrencyMismatch      = "currency_mismatch"
	CodeUnauthorized          = "unauthorized"
	CodeNotFound              = "not_found"
	CodeAlreadyReversed       = "already_reversed"
	CodeInvalidJournal        = "invalid_journal"
	CodeDuplicateAccount      = "duplicate_account"
	CodeInsufficientStock     = "insufficient_stock"
	CodeInsufficientWallet    = "insufficient_wallet"
	CodeForbidden             = "forbidden"
	CodeOutletForbidden       = "outlet_forbidden"
	CodeRefundViaOrder        = "refund_via_order"
	CodeInvalidOrder          = "invalid_order"
	CodeControlAccount        = "control_account"
	CodeRefundInsufficient    = "refund_insufficient_funds"
)

func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return nil
}

func Unbalanced(debits, credits int64) *Error {
	return Err(CodeUnbalanced, fmt.Sprintf("debits %d != credits %d", debits, credits))
}
