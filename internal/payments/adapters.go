package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// --- till: the standard format ---------------------------------------------
//
// Any system that knows a payment happened (a fintech's core, a terminal app,
// a bank feed) can post this shape. Amounts are integer minor units.
//
//	Till-Signature: t=<unix>,v1=<hex HMAC-SHA256 of "t.body">
//	{"id":"...","reference":"IKJ-...","amount_minor":450000,"fee_minor":0,
//	 "currency":"NGN","status":"success","method":"tap","paid_at":"...",
//	 "payer":{"name":"","account":"","bank":""},"account_number":"","session_id":""}

// Standard is the JSON body of the till format. The staff endpoint that records
// a payment by hand takes the same body.
type Standard struct {
	ID          string          `json:"id"`
	Reference   string          `json:"reference"`
	AmountMinor json.RawMessage `json:"amount_minor"`
	FeeMinor    json.RawMessage `json:"fee_minor"`
	Currency    string          `json:"currency"`
	Status      string          `json:"status"`
	Method      string          `json:"method"`
	PaidAt      string          `json:"paid_at"`
	Payer       struct {
		Name    string `json:"name"`
		Account string `json:"account"`
		Bank    string `json:"bank"`
	} `json:"payer"`
	AccountNumber string `json:"account_number"`
	SessionID     string `json:"session_id"`
}

// Event converts the body. Currency defaults to NGN and status to success.
func (s Standard) Event() (Event, error) {
	amount, err := minorInt(s.AmountMinor)
	if err != nil {
		return Event{}, err
	}
	fee, err := minorInt(s.FeeMinor)
	if err != nil {
		return Event{}, err
	}
	e := Event{
		ID: strings.TrimSpace(s.ID), Reference: strings.TrimSpace(s.Reference),
		AmountMinor: amount, FeeMinor: fee,
		Currency:  strings.ToUpper(strings.TrimSpace(s.Currency)),
		Status:    strings.ToLower(strings.TrimSpace(s.Status)),
		Method:    strings.ToLower(strings.TrimSpace(s.Method)),
		PaidAt:    parseTime(s.PaidAt),
		PayerName: strings.TrimSpace(s.Payer.Name), PayerAccount: strings.TrimSpace(s.Payer.Account),
		PayerBank:     strings.TrimSpace(s.Payer.Bank),
		AccountNumber: strings.TrimSpace(s.AccountNumber), SessionID: strings.TrimSpace(s.SessionID),
	}
	if e.Currency == "" {
		e.Currency = "NGN"
	}
	if e.Status == "" {
		e.Status = StatusSuccess
	}
	return e, e.Validate()
}

type tillAdapter struct{}

func (tillAdapter) Verify(secret string, h http.Header, body []byte, now time.Time) bool {
	return timestamped(sha256.New, secret, h.Get("Till-Signature"), body, now)
}

func (tillAdapter) Parse(body []byte) (Event, error) {
	var s Standard
	if err := json.Unmarshal(body, &s); err != nil {
		return Event{}, err
	}
	return s.Event()
}

// --- paystack ----------------------------------------------------------------
//
// Checked against paystack.com/docs/payments/webhooks (November 2026).
// Two signatures are in use: Paystack-Signature (t=,v1=<key id>:<hex>, HMAC
// SHA512 of "t.body" keyed with the whsec_ signing secret) and the older
// x-paystack-signature (HMAC SHA512 of the body keyed with the secret key).
// The connection's secret is whichever of the two the merchant's account uses.
// Amounts are kobo.

type paystackAdapter struct{}

func (paystackAdapter) Verify(secret string, h http.Header, body []byte, now time.Time) bool {
	if v := h.Get("Paystack-Signature"); v != "" {
		return timestamped(sha512.New, secret, v, body, now)
	}
	return hexEqual(hmacHex(sha512.New, secret, body), h.Get("x-paystack-signature"))
}

func (paystackAdapter) Parse(body []byte) (Event, error) {
	var in struct {
		Event string `json:"event"`
		Data  struct {
			ID            json.RawMessage `json:"id"`
			Reference     string          `json:"reference"`
			Amount        json.RawMessage `json:"amount"`
			Fees          json.RawMessage `json:"fees"`
			Currency      string          `json:"currency"`
			Status        string          `json:"status"`
			Channel       string          `json:"channel"`
			PaidAt        string          `json:"paid_at"`
			Authorization struct {
				SenderName     string `json:"sender_name"`
				SenderBank     string `json:"sender_bank"`
				SenderAccount  string `json:"sender_bank_account_number"`
				ReceiverNumber string `json:"receiver_bank_account_number"`
			} `json:"authorization"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Event{}, err
	}
	if in.Event != "charge.success" {
		return Event{}, ErrIgnored
	}
	amount, err := minorInt(in.Data.Amount)
	if err != nil {
		return Event{}, err
	}
	fee, _ := minorInt(in.Data.Fees)
	e := Event{
		ID: rawID(in.Data.ID), Reference: strings.TrimSpace(in.Data.Reference),
		AmountMinor: amount, FeeMinor: fee,
		Currency: strings.ToUpper(strings.TrimSpace(in.Data.Currency)),
		Status:   StatusFailed, PaidAt: parseTime(in.Data.PaidAt),
		PayerName: in.Data.Authorization.SenderName, PayerBank: in.Data.Authorization.SenderBank,
		PayerAccount: in.Data.Authorization.SenderAccount, AccountNumber: in.Data.Authorization.ReceiverNumber,
	}
	if strings.EqualFold(strings.TrimSpace(in.Data.Status), "success") {
		e.Status = StatusSuccess
	}
	switch strings.ToLower(in.Data.Channel) {
	case "card", "apple_pay":
		e.Method = MethodCard
	case "bank_transfer", "dedicated_nuban", "bank", "eft":
		e.Method = MethodTransfer
	case "ussd":
		e.Method = MethodUSSD
	case "qr":
		e.Method = MethodQR
	case "mobile_money":
		e.Method = MethodMobileMoney
	}
	if e.FeeMinor >= e.AmountMinor {
		e.FeeMinor = 0
	}
	return e, e.Validate()
}

// --- flutterwave (v3) ----------------------------------------------------------
//
// NOT checked against live documentation: the docs could not be read when this
// was written. Built from the published v3 behaviour: the verif-hash header
// carries the secret hash set on the dashboard (a shared secret, not an HMAC),
// and amounts are decimal naira. Confirm with a sandbox webhook before go-live.

type flutterwaveAdapter struct{}

func (flutterwaveAdapter) Verify(secret string, h http.Header, _ []byte, _ time.Time) bool {
	got := h.Get("verif-hash")
	return got != "" && hmac.Equal([]byte(got), []byte(secret))
}

func (flutterwaveAdapter) Parse(body []byte) (Event, error) {
	var in struct {
		Event string `json:"event"`
		Data  struct {
			ID          json.RawMessage `json:"id"`
			TxRef       string          `json:"tx_ref"`
			Amount      json.RawMessage `json:"amount"`
			AppFee      json.RawMessage `json:"app_fee"`
			Currency    string          `json:"currency"`
			Status      string          `json:"status"`
			PaymentType string          `json:"payment_type"`
			CreatedAt   string          `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Event{}, err
	}
	if in.Event != "charge.completed" {
		return Event{}, ErrIgnored
	}
	amount, err := majorToMinor(in.Data.Amount)
	if err != nil {
		return Event{}, err
	}
	fee, _ := majorToMinor(in.Data.AppFee)
	e := Event{
		ID: rawID(in.Data.ID), Reference: strings.TrimSpace(in.Data.TxRef),
		AmountMinor: amount, FeeMinor: fee,
		Currency: strings.ToUpper(strings.TrimSpace(in.Data.Currency)),
		Status:   StatusFailed, PaidAt: parseTime(in.Data.CreatedAt),
	}
	if strings.EqualFold(strings.TrimSpace(in.Data.Status), "successful") {
		e.Status = StatusSuccess
	}
	switch strings.ToLower(in.Data.PaymentType) {
	case "card":
		e.Method = MethodCard
	case "bank_transfer", "banktransfer", "account":
		e.Method = MethodTransfer
	case "ussd":
		e.Method = MethodUSSD
	case "qr", "nqr":
		e.Method = MethodQR
	}
	if strings.HasPrefix(strings.ToLower(in.Data.PaymentType), "mobilemoney") {
		e.Method = MethodMobileMoney
	}
	if e.FeeMinor >= e.AmountMinor {
		e.FeeMinor = 0
	}
	return e, e.Validate()
}

// --- monnify (Moniepoint's gateway) ---------------------------------------------
//
// NOT checked against live documentation: the docs could not be read when this
// was written. Built from the published behaviour: monnify-signature is the hex
// HMAC SHA512 of the body keyed with the client secret, and amounts are decimal
// naira. Confirm with a sandbox webhook before go-live.

type monnifyAdapter struct{}

func (monnifyAdapter) Verify(secret string, h http.Header, body []byte, _ time.Time) bool {
	return hexEqual(hmacHex(sha512.New, secret, body), h.Get("monnify-signature"))
}

func (monnifyAdapter) Parse(body []byte) (Event, error) {
	var in struct {
		EventType string `json:"eventType"`
		EventData struct {
			TransactionReference string          `json:"transactionReference"`
			PaymentReference     string          `json:"paymentReference"`
			AmountPaid           json.RawMessage `json:"amountPaid"`
			Currency             string          `json:"currency"`
			PaymentStatus        string          `json:"paymentStatus"`
			PaymentMethod        string          `json:"paymentMethod"`
			PaidOn               string          `json:"paidOn"`
			Destination          struct {
				AccountNumber string `json:"accountNumber"`
			} `json:"destinationAccountInformation"`
			Sources []struct {
				AccountName   string `json:"accountName"`
				AccountNumber string `json:"accountNumber"`
				BankCode      string `json:"bankCode"`
				SessionID     string `json:"sessionId"`
			} `json:"paymentSourceInformation"`
		} `json:"eventData"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Event{}, err
	}
	if in.EventType != "SUCCESSFUL_TRANSACTION" {
		return Event{}, ErrIgnored
	}
	d := in.EventData
	amount, err := majorToMinor(d.AmountPaid)
	if err != nil {
		return Event{}, err
	}
	e := Event{
		ID: strings.TrimSpace(d.TransactionReference), Reference: strings.TrimSpace(d.PaymentReference),
		AmountMinor: amount, Currency: strings.ToUpper(strings.TrimSpace(d.Currency)),
		Status: StatusFailed, AccountNumber: d.Destination.AccountNumber,
		PaidAt: parseTime(d.PaidOn, "2006-01-02 15:04:05.000", "2006-01-02 15:04:05", "02/01/2006 3:04:05 PM"),
	}
	if e.Currency == "" {
		e.Currency = "NGN"
	}
	if strings.EqualFold(strings.TrimSpace(d.PaymentStatus), "PAID") {
		e.Status = StatusSuccess
	}
	if len(d.Sources) > 0 {
		e.PayerName, e.PayerAccount = d.Sources[0].AccountName, d.Sources[0].AccountNumber
		e.PayerBank, e.SessionID = d.Sources[0].BankCode, d.Sources[0].SessionID
	}
	switch strings.ToUpper(d.PaymentMethod) {
	case "CARD":
		e.Method = MethodCard
	case "ACCOUNT_TRANSFER":
		e.Method = MethodTransfer
	case "USSD":
		e.Method = MethodUSSD
	}
	return e, e.Validate()
}
