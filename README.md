# Till

Till is a point-of-sale ledger and inventory API for a Nigerian merchant with more than one shop. It takes payment from any provider and by any method: cash, store credit, bank transfer, card, tap, USSD, QR and mobile money. A sale takes stock off the shelf and writes the money into the books in the same Postgres transaction. If that sale fails, the stock and the journal roll back together.

A transfer, card or tap sale is not revenue until the provider says the money arrived. A signed provider event is the only thing that pays it. A screenshot, an SMS or a customer's word never does. Money that arrives late, twice, for the wrong amount or with no order reference is still booked, into suspense, and a manager resolves it. Each payment connection (a Paystack account, a Moniepoint account, a card terminal) has its own clearing account, so every provider reconciles on its own.

Money is integer kobo. Posted journals cannot be edited. Stock cannot go negative. A cashier cannot send a price. The shelf price is snapshotted onto the order.

This repository is a Go service (`cmd/tilld`). A till app, WhatsApp bot, admin screen or a fintech's own system would call this HTTP API.

## Run it

You need Go 1.26 and Postgres 16 listening on `127.0.0.1:5432`.

```text
postgres://till:till@127.0.0.1:5432/till?sslmode=disable
```

```bash
cp .env.example .env
go run ./cmd/tilld
```

The `Dockerfile` builds a non-root image and CI builds it. There is no Compose file. The image defaults to `TILL_ENV=production`, so give it real secrets.

`TILL_SEED=true` loads **Adaeze Studio** once. The seed checks for `admin@till.local` and does nothing on the next boot. Opening cash is posted only in that first transaction.

| Who | Email | Outlet | Password |
|---|---|---|---|
| Admin | `admin@till.local` | every outlet | `change-me-now-12` |
| Ikeja manager | `manager.ikeja@till.local` | Ikeja `IKJ` | `change-me-now-12` |
| Ikeja cashier | `cashier.ikeja@till.local` | Ikeja `IKJ` | `change-me-now-12` |
| Surulere cashier | `cashier.surulere@till.local` | Surulere `SUL` | `change-me-now-12` |

That password, the secrets in `.env.example` and the two demo connection secrets below are for a local demo. Change them before any real deployment. There is no public register.

OpenAPI: [`api/openapi.yaml`](api/openapi.yaml). Operations: [`docs/RUNBOOK.md`](docs/RUNBOOK.md).

## Payments

**Payment connections.** A connection is one place the merchant receives non-cash money: a Paystack account, a Moniepoint or Monnify account, a card or tap terminal, a bank account watched by a feed. Each has a code, a provider, a secret, an optional outlet and its own webhook URL.

- `POST /v1/payment-connections` (admin) with `code` (2 to 8 letters or digits), `name`, `provider` (`till`, `paystack`, `flutterwave` or `monnify`), `secret`, and optional `outlet_id` and `account_number`. The secret is the provider's webhook signing secret and is required, except for provider `till`, where Till generates one and returns it once. Creating a connection creates its clearing account `1100-CODE`.
- `GET /v1/payment-connections` (manager, admin) lists them with `balance_minor` (the clearing account: what the provider still holds for the merchant) and `webhook_path`. The secret is never returned.
- `DELETE /v1/payment-connections/{id}` (admin) disables one. History and balance stay.
- `POST /v1/payment-connections/{id}/settlements` (admin, `Idempotency-Key`) with `amount_minor` and `reference` records a provider payout to the bank: Dr `1050` Bank, Cr `1100-CODE`.

**Methods.** `payment_method` and each split tender accept `cash`, `wallet` and the awaited methods `transfer`, `card`, `tap`, `ussd`, `qr` and `mobile_money`. `tap` is a contactless card or phone on a terminal or SoftPOS. Every awaited method behaves the way a transfer always did: stock is reserved, the order is `pending_payment`, and only a payment event pays it. An order, or a split, has at most one awaited tender.

**Inbound.** `POST /v1/webhooks/{connection_id}` takes no bearer token. The connection picks the merchant, the adapter and the secret before the body is read. An unknown, disabled or badly signed request all answer `401`, so the URL does not reveal which connections exist. A signed event that is not a payment (a payout notice, a failed charge) answers `200` with status `ignored`.

**Recorded by staff.** `POST /v1/payment-connections/{id}/payments` takes the `till` format below with no signature. A manager, an admin or a manager-role API key calls it, for money Till cannot hear about by webhook: a transfer checked in the bank app, a terminal that is not integrated. The staff member goes on the audit trail. A cashier gets `403`. This is the deliberate replacement for a "mark paid" button: it needs a manager and a provider event id.

**One path for every payment.** Webhook or staff, the payment takes the same steps:

1. The event is recorded once per connection and event id. A repeat replays the first answer with `Idempotent-Replayed: true` and posts nothing.
2. The order is found by `reference` inside that merchant. With no reference, or one that matches no order, the single pending order waiting for exactly that amount is paid (at the connection's outlet when it has one). Two candidates are not guessed (`ambiguous_amount`).
3. A payment that fits pays the order: Dr `1100-CODE`, Cr outlet sales, Cr VAT, plus cost of goods when the average is above zero. The reservation becomes a sale.
4. Anything that cannot be applied is booked Dr `1100-CODE` / Cr Payment suspense `2300` and opened as an unmatched payment, with reason `no_matching_order`, `ambiguous_amount`, `order_expired`, `order_voided`, `order_already_paid`, `order_refunded`, `currency_mismatch` or `amount_mismatch`. Money is never dropped.
5. `fee_minor` is posted Dr `5100` Payment fees / Cr `1100-CODE`, so the clearing balance is what the provider really holds.
6. Status `failed` is ignored. Status `reversed` is only written to the audit log (`payment.reversed`) and posts nothing. Chargeback handling is not built.

**Unmatched payments.** Each row carries `connection_id` and `details` (payer name, bank, account, session id, method, reference). Managers and admins resolve open rows (all need `Idempotency-Key`):

- `GET /v1/unmatched-payments?outlet_id=&status=`
- `POST /v1/unmatched-payments/{id}/apply` with optional `order_id` turns the money into the paid sale. It works for a pending order (the amount must equal what the order is waiting for) and for an expired one (the amount must equal the order total and the stock must be available again). Otherwise `409`. A non-admin must be assigned to that order's outlet. The sale journal debits `2300`.
- `POST /v1/unmatched-payments/{id}/refund` moves it to `refund_pending`: Dr `2300` / Cr Refunds payable `2310`.
- `POST /v1/unmatched-payments/{id}/refunded` records the payout: Dr `2310` / Cr the clearing account of the connection the money came in on.

## Integrating a payment provider

1. An admin creates a connection for the provider account. Note its `id`.
2. In the provider's dashboard, set the webhook URL to `https://<your host>/v1/webhooks/<id>` and the signing secret to the one you gave Till.
3. Send a test payment against a pending order and check `balance_minor` on the connection.

| Provider | Signature header | Payment event | Amounts | Status |
|---|---|---|---|---|
| `paystack` | `Paystack-Signature: t=..,v1=<key id>:<hex>`, HMAC-SHA512 of `t.body`, 5 minute tolerance (`whsec_` secret); or the older `x-paystack-signature`, HMAC-SHA512 of the body | `charge.success` | kobo | Checked against Paystack's documentation |
| `till` | `Till-Signature: t=<unix>,v1=<hex>`, HMAC-SHA256 of `t.body`, 5 minute tolerance | the format below | integer kobo | Till's own format |
| `flutterwave` | `verif-hash`, a shared secret | `charge.completed` | naira, converted exactly | **Not verified** |
| `monnify` | `monnify-signature`, HMAC-SHA512 of the body | `SUCCESSFUL_TRANSACTION` | naira, converted exactly | **Not verified** |

The Flutterwave and Monnify adapters are written from published behaviour and were not checked against live documentation. Confirm each with a sandbox webhook before go-live.

**The `till` format** is the standard shape any system can post (a fintech's core, a terminal app, a bank feed). Body:

```json
{"id":"tap-1001","reference":"IKJ-A1B2C3D4-000002","amount_minor":120000,"method":"tap"}
```

Only `id` (the dedupe key, unique per connection) and `amount_minor` are required. Optional: `reference`, `fee_minor`, `currency` (default `NGN`), `status` (`success`, `failed`, `reversed`; default `success`), `method` (an awaited method), `paid_at`, `payer` (`name`, `account`, `bank`), `account_number`, `session_id`.

```bash
SECRET=dev-till-secret-change-me     # the connection's secret
BODY='{"id":"tap-1001","reference":"IKJ-A1B2C3D4-000002","amount_minor":120000,"method":"tap"}'
T=$(date +%s)
SIG=$(printf '%s.%s' "$T" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')

curl -s -X POST "$BASE/v1/webhooks/$CONNECTION_ID" \
  -H 'Content-Type: application/json' \
  -H "Till-Signature: t=$T,v1=$SIG" \
  -d "$BODY"
```

## What a sale does

**Cash.** One database transaction locks the product rows (for cost), then the stock rows, both in id order. It copies the server price, subtracts `on_hand`, and posts one journal:

- Debit outlet cash `1000-{CODE}` for the amount the customer pays
- Credit outlet sales `4000-{CODE}` for the net
- Credit VAT payable `2100` for the VAT, omitted when VAT is 0
- Debit cost of goods `5000` and credit inventory `1200` for quantity times average cost, omitted when that cost is 0

The order is `paid`. The response is `201`. Audit action: `order.paid`.

**Wallet.** Same stock movement and the same cost lines. The debit is customer wallet `2200` instead of cash, and `customers.balance_minor` drops by the same amount. The balance cannot go below zero (`409 insufficient_wallet`). `customer_id` is required. Every other method rejects it. A refund of a wallet sale puts that amount back on the balance in the same transaction as the reversal.

**Transfer, card, tap, USSD, QR, mobile money.** The same transaction does not touch `on_hand` and does not post a sale. It increases `reserved`, stores the order as `pending_payment`, and holds the units for `TILL_HOLD_MINUTES` (default 15). Unconfirmed money is not revenue. A payment event (see Payments) is the only writer of that sale journal. Cost of goods is snapshotted when the payment lands, not when the hold starts, because a receipt can change the average during the hold.

**Split tender.** `POST /v1/orders` takes either `payment_method` or `payments: [{method, amount_minor, customer_id?}]` (at most one cash, one wallet and one awaited tender, `customer_id` only on the wallet tender, amounts must add up to the payable or `400 tender_mismatch`). Two or more tenders make the order's `payment_method` `split`; `payments` on the order shows each one. Cash and wallet only: the sale posts at once. With an awaited tender the order is held `pending_payment`; the cash and wallet tenders are taken now and parked in **Deposits held `2400`** (Dr cash / Dr `2200`, Cr `2400`). The provider payment must be for the awaited portion only; it posts the sale as Dr `2400` + Dr `1100-CODE`, Cr sales, Cr VAT (+ Dr COGS, Cr inventory). On expiry or void the deposits go back. A late payment for an order whose deposits were already returned lands in unmatched payments, so the customer is refunded rather than silently charged.

**Void.** `POST /v1/orders/{id}/void` (Idempotency-Key; cashier at the outlet, manager, admin; optional `reason`) cancels a `pending_payment` order: reservation released, deposits returned, status `voided`, audit `order.voided`. A payment that arrives afterwards is parked in unmatched payments with reason `order_voided`.

**Refund.** A manager or admin restocks and reverses the sale journal in one transaction; an optional `reason` is stored with `refund_journal_id`, `refunded_at` and `refunded_by` on the order. Products are locked in id order before stock. That reversal puts the snapshotted cost back into inventory. It does not recompute the average, so quantity times average can sit a few kobo away from the inventory account until the next receipt. If an account would go negative (for example the drawer was already banked) the answer is `409 refund_insufficient_funds` naming the account. The refund of an awaited tender credits Refunds payable `2310` instead of clearing, because the money still has to leave through the provider; the response carries `refund_record_id`, and that `refund_pending` record (`reason: order_refund`) is closed with `POST /v1/unmatched-payments/{id}/refunded`. `POST /v1/journals/{id}/reversal` refuses a journal that belongs to an order (`409 refund_via_order`).

**Partial and per-line refunds.** `POST /v1/orders/{id}/refund` also takes `lines: [{product_id | sku, quantity}]`, `reason` and `refund_to` (any of the order's methods). No `lines` means everything still unrefunded. Each line stores its own `discount_minor`, `vat_minor`, `net_minor` and `cost_minor` at sale time: the order discount is split by line total (remainder kobo to the last line), then VAT is worked out per line. A line's amounts are spread over its units as cumulative floors, so refunds in pieces add up to exactly what was sold. A refund can be repeated until nothing is left; status goes `paid`, `partially_refunded`, `refunded`, and `refunded_minor` keeps the running total. Each refund is its own journal (Dr sales, Dr VAT payable, Cr the tender accounts, plus Dr inventory / Cr COGS), restocks the units and writes an `order_refunds` row; the response carries `refund`. More units than are left is `409 refund_exceeds_order`. **Which tender gets the money:** with `refund_to`, all of it, and that tender must still have that much left (`400` otherwise). Without it, the refund is spread over the order's tenders in proportion to what each has left (rounding kobo go to cash, then wallet, then the awaited tender). The awaited share is credited to Refunds payable `2310` with its own `refund_pending` record; the wallet share goes back to the customer's balance.

**Discount controls.** `discount_reason` is required when `discount_minor > 0` (`400 discount_reason_required`) and is stored on the order. A cashier may discount up to `max_discount_bps_cashier` (default 1000, 10%) of the shelf total, a manager up to `max_discount_bps_manager` (default 3000); above that is `403 discount_limit`. An admin has no limit, but the discount must still be below the shelf total. `GET` and `PATCH /v1/merchant/settings` (admin) read and change `max_discount_bps_cashier`, `max_discount_bps_manager`, `vat_registered` and `vat_inclusive_bps`; changes are audited as `merchant.settings_updated` and apply to new orders only.

**Tax classes.** Each product has `tax_class`: `standard` (default), `exempt` or `zero` (admin sets it on create or `PATCH`; a manager gets `403`). Only `standard` lines carry VAT, at the merchant's `vat_inclusive_bps`. `vat_registered=false` (the small-business exemption) puts 0 VAT on every line. The class is copied onto the order line. Multi-line orders can differ by a kobo or two from the order-level formula below, because VAT is rounded per line.

**Control accounts.** Free-form `POST /v1/journals` and raw reversals refuse any line on the merchant's system accounts (`1100` and every `1100-CODE`, `1200`, `2100`, `2150`, `2200`, `2300`, `2310`, `2400`, `5000`) with `409 control_account`. They move only through sales, receipts, top-ups, payments, settlements, supplier payments and the flows above. `1050` Bank is not a control account.

**Expiry.** Every 30 seconds a sweeper finds due holds and releases each one in its own transaction (stock rows locked in product id order, deadlocks retried). Each release sets the order to `expired` and writes `reservation.released`; it posts no journal. A failing order is logged with `slog` and skipped for that tick so it cannot starve the others. The same loop deletes `idempotency_keys` older than 30 days (at most hourly).

## Staff and outlets

| Role | Can do |
|---|---|
| Cashier | Log in, see assigned outlets, sell (discounts up to the cashier limit), void a pending order, read stock, create a customer, take a cash top-up |
| Manager | Cashier, plus non-price product edits, receipts, adjustments, refunds, daily sales, audit for their outlet, a higher discount limit, unmatched payments, listing payment connections and recording a payment by hand |
| Admin | Manager, plus creating products and price, cost or tax class changes, staff, API keys, outlets, payment connections and settlements, merchant settings, unlimited discounts, the chart of accounts, and free-form journals |

An outlet id from another merchant is `404`. The same merchant, but a staff member who is not assigned to that outlet, is `403 outlet_forbidden`. Admins are not assigned outlet-by-outlet. They see the whole merchant. The last active admin cannot be disabled (`409 last_admin`).

`GET /v1/reports/daily-sales` always requires `outlet_id`, including for an admin. The day is an Africa/Lagos calendar date. Omit `date` and the report uses today in Lagos. Admin may omit `outlet_id` on `GET /v1/orders` and `GET /v1/audit`. A manager or cashier must pass one.

## Security and operations

**Login.** Per-IP and per-email token buckets (`429 rate_limited` with `Retry-After`; the burst equals the per-minute limit, and they live in process memory, so each instance enforces its own). Ten failed logins in 15 minutes lock the account for 15 minutes (`429 account_locked`), stored in Postgres so it holds across instances. Unknown emails are verified against a dummy argon2 hash, argon2 runs behind a 4-way semaphore, and the login body is capped at 4 KiB.

**Tokens.** Access tokens carry `jti` and the staff row's `token_version`; `TILL_TOKEN_TTL` sets the lifetime (default `8h`). `POST /v1/auth/logout` and `POST /v1/auth/password` (current password required, 12 to 128 characters) bump the version and kill every older token. An admin can `POST /v1/staff/{id}/password-reset`.

**API keys.** Admin `POST /v1/api-keys` with `name`, `role` (`cashier` or `manager`) and `outlet_ids` returns `till_live_<prefix>_<secret>` once. Send it as `Authorization: Bearer till_live_...`. Only a SHA-256 digest is stored. `GET /v1/api-keys` lists, `DELETE /v1/api-keys/{id}` revokes at once. Each key is backed by a non-login staff row, so outlet scope, roles, orders and audit work unchanged.

**Authorization.** Only admins create products or change `price_minor` / `avg_cost_minor` (managers get `403`). Cashiers list customers only with `q` (3+ characters) and see phones masked as `0803****567`; managers and admins see full numbers. Phones are stored as E.164 (`+2348031234567`).

**Pagination.** `GET /v1/orders`, `/v1/journals` and `/v1/audit` return `next_cursor` (null on the last page); pass it back as `cursor`. Orders also filter by `status` (`pending_payment`, `paid`, `partially_refunded`, `refunded`, `expired`, `voided`), `reference`, `customer_id`, `from`, `to`.

**Production guard.** `TILL_ENV=production` refuses the `.env.example` JWT secret, a JWT secret under 48 characters, `TILL_SEED=true`, and `TILL_CORS_ORIGINS=*`.

**HTTP.** `X-Request-ID` in and out, JSON access log per request (route pattern, status, duration, merchant, staff, api key), JSON `404`/`405`/`413`, `Cache-Control: no-store` on `/v1`, CORS only for origins in `TILL_CORS_ORIGINS`. Staff write requests reject unknown JSON fields, including `POST /v1/journals`.

**Ops.** `/livez`, `/readyz` (database, sweeper heartbeat, draining), `/metrics` (Prometheus text), graceful shutdown waits for the sweeper. Backups, restore drill, rotation, the daily payment routine and incident steps: [docs/RUNBOOK.md](docs/RUNBOOK.md).

## Money, VAT, and stock

- Currency is NGN. Amounts are `BIGINT` kobo. `100` kobo is ₦1. JSON floats and scientific notation are `400`.
- VAT is included in the price the customer pays. The merchant rate is `vat_inclusive_bps`. The seed uses `750`, which is 7.5%.
- `vat = payable * bps / (10000 + bps)`, integer division, per line by tax class (see Tax classes). The leftover kobo stays in sales, so net plus VAT equals the payable. A ₦4,500.00 line (`450000` kobo) at 750 bps is VAT `31395` and net `418605`.
- `discount_minor` is an optional order-level integer. Omit it, or send `0`, and nothing is taken off. It must be less than the shelf total. A ₦500.00 discount (`50000` kobo) on that ₦4,500.00 line makes the payable `400000`, VAT `27906`, and net `372094`. The cashier still cannot send a unit price. A discount does not reduce cost of goods.
- Average cost is `avg_cost_minor` on the product. A receipt may send `unit_cost_minor`. The new average is `(units on hand * old average + quantity * unit cost) / (units on hand + quantity)`, rounded down. Omit the unit cost and the current average is kept.
- Available stock is `on_hand - reserved`. Low stock means available is less than or equal to `reorder_level`.
- Postgres checks: `on_hand >= 0`, `reserved >= 0`, `reserved <= on_hand`.
- Stock rows are locked `ORDER BY product_id`. Account rows are locked `ORDER BY id`. That order is what keeps two cashiers from deadlocking each other.

Order references look like `IKJ-A1B2C3D4-000001`. The outlet code repeats across merchants, so the middle token is the random tail of that outlet's id. A provider sends the reference back with no merchant id, and the reference is unique in the database. Till still looks it up inside the merchant that owns the connection.

## Idempotency

`Idempotency-Key` is required on orders, voids, refunds, wallet top-ups, journals, reversals, stock receipts, stock adjustments, stock transfers, stock counts, supplier payments, connection settlements, product `PATCH` and the unmatched-payment actions. The key is scoped to the merchant and bound to a fingerprint of the HTTP method, the resolved path (ids included) and the canonical body (numbers are compared exactly, never as floats).

- Same key and the same request: the stored response is returned with `Idempotent-Replayed: true`. Stock is not deducted twice and the journal is not posted twice.
- Same key on a different order, customer, endpoint or body: `409 idempotency_conflict`.
- A key that is still inside its transaction: `409 idempotency_in_progress`.
- Keys starting with `wh:` or `sys:` (any case) are reserved for the system: `400 idempotency_key_reserved`.
- Keys are kept for 30 days, then purged.

Payments do not use this table. They are deduplicated per connection and event id; see One path for every payment.

## Reports

`GET /v1/reports/daily-sales` (outlet and Lagos day) separates sales from refunds:

- **Sales** (`order_count`, `total_minor`, `vat_minor`, `net_minor`, `discount_total_minor`) are orders paid on that Lagos day, whatever happened to them later. **A paid order that is refunded afterwards still counts as gross sales on its sale day**, so a past day's report does not change after the fact. Pending, expired and voided orders are not sales.
- **Refunds** (`refund_count`, `refunds_minor`) are counted on the refund day. `net_sales_minor = total_minor - refunds_minor` (both VAT inclusive; it can be negative).
- `tender_totals` has one `<method>_minor` key for each method used that day, and `cash_minor`, `transfer_minor` and `wallet_minor` are always present. They are the confirmed tenders of that day's sales, so a split order contributes to each of its tenders. `by_method` groups by the order's `payment_method` (including `split`). `by_cashier` gives each staff member's sales, discounts, and the refunds they made.
- `books_debit_minor` is the asset and liability debits (outlet cash, connection clearing, wallet `2200`, suspense `2300`, Deposits held `2400`) on the sale journals of those orders; `books_refund_credit_minor` is the asset and liability credits (cash, wallet, Refunds payable `2310`) on that day's refund journals. **`matches_books` is true when `total_minor = books_debit_minor` and `refunds_minor = books_refund_credit_minor`.** Cost of goods, inventory, sales and VAT lines are not part of either figure, so a sale with a cost layer still matches. Receipts and revaluations are not sale journals.

`GET /v1/reports/low-stock` and `GET /v1/reports/stock-on-hand` take `outlet_id`. Ikeja soap is seeded at 3 on hand with reorder level 5, so it is low. Surulere soap is 20 and is not.

`GET /v1/invariants` is admin-only and returns `checks`, each with `name`, `holds`, `expected`, `actual`. `holds` is true when every check holds: the identity below, cached balances matching the journal, wallet `2200` equal to the sum of customer balances, suspense `2300` equal to the open unmatched payments, Refunds payable `2310` equal to the refunds waiting for payout, Deposits held `2400` equal to the held tenders of pending split orders, and inventory `1200` within one kobo per unit of `on_hand` times average cost (`residue_minor` and `tolerance_minor` are reported). The identity:

```text
SUM(asset, expense) - SUM(liability, equity, income) = 0
```

## Shop operations

- **Shifts and cash-up.** `POST /v1/shifts/open`, `GET /v1/shifts`, `GET /v1/shifts/{id}`, `GET /v1/shifts/{id}/z-report`, `POST /v1/shifts/{id}/close`. One open shift per staff member per outlet (`409 shift_already_open`). Expected cash is the opening float plus debits minus credits to `1000-CODE` between open and close, for journals that staff member posted (matched through the audit event written in the same transaction). A count that differs posts the variance against `5300`. The Z-report is returned by close and frozen. A manager or admin may close anyone's shift. Not built: a `require_open_shift` merchant setting.
- **Event feed and outbound webhooks.** `GET /v1/events?cursor=&types=` (admin) lists public events derived from `audit_events`. `POST/GET/DELETE /v1/webhook-endpoints` and `GET /v1/webhook-endpoints/{id}/deliveries` manage endpoints; the signing secret is shown once. The worker in `cmd/tilld` POSTs each event with `Till-Signature` (the same scheme as the `till` payment format) and `Till-Event-Id`, retrying with exponential backoff up to 8 attempts, 10 s timeout, redirects never followed, private and link-local targets blocked in production. Several instances are safe. `stock.low` is webhook-only. Delivery is at-least-once; dedupe on `Till-Event-Id`.
- **Stock transfers.** `POST /v1/stock/transfers` deducts from the source (never below reserved) and sets `in_transit`; `.../receive` adds to the destination; `.../cancel` returns to the source. No journal: inventory `1200` is merchant-wide and cost is per product. `/v1/invariants` counts units in transit at average cost.
- **Stock counts.** `POST /v1/stock/counts` sets `on_hand` to the counted figure, posting any variance through the normal adjustment path (`5200`, reason `stock_count`). Counting below reserved is `409`.
- **Suppliers.** CRUD on `/v1/suppliers`; a stock receipt may carry `supplier_id`. `POST /v1/supplier-payments` posts Dr `2150` / Cr outlet cash (`cash`) or Cr `1050` Bank (`bank`). `GET /v1/suppliers/{id}/statement` shows receipts, payments and the balance owed.
- **VAT summary.** `GET /v1/reports/vat?from=&to=` (admin): output VAT (credits to `2100`) and reversals by Lagos month. Input VAT is a placeholder `0`.

## Seeded shop

Adaeze Studio sells skincare. Outlets are Ikeja `IKJ` and Surulere `SUL`. Two payment connections are seeded: `POS` (provider `till`, Ikeja, a card and tap terminal, secret `dev-till-secret-change-me`) and `PSTK` (provider `paystack`, secret `dev-paystack-secret-change-me`).

| SKU | Price (kobo) | Average cost (kobo) | Reorder | Ikeja on hand | Surulere on hand |
|---|---:|---:|---:|---:|---:|
| `SERUM-C` | 850000 (₦8,500) | 320000 | 2 | 20 | 20 |
| `SHEA-500` | 450000 (₦4,500) | 180000 | 2 | 20 | 12 |
| `TOWEL` | 120000 (₦1,200) | 40000 | 2 | 30 | 15 |
| `SOAP` | 200000 (₦2,000) | 70000 | 5 | 3, low | 20 |

Opening journal: debit Cash Ikeja ₦50,000 (`5000000`), debit Cash Surulere ₦20,000 (`2000000`), debit Inventory `21970000` (the stock the shop already owns, at those averages), credit Opening equity `3000` for `28970000`. No sale is seeded. The seed writes that inventory debit itself. It does not also post a receipt, or the stock would be counted twice.

The seed runs once. If `admin@till.local` is already in the database, a restart adds nothing, including the demo connections. To reload the sample, stop Till, drop the database `till` (not `till_test`), and start again with `TILL_SEED=true`.

Chart of accounts:

| Code | Account |
|---|---|
| `1000-IKJ` / `1000-SUL` | Cash at that outlet |
| `1050` | Bank. Created on first use. Not a control account, so free-form journals may post to it (an opening balance, say) |
| `1100-POS` / `1100-PSTK` | Clearing for one payment connection: what the provider still holds. Control account. May go negative |
| `1100` | Legacy clearing from before connections. New flows do not use it |
| `1200` | Inventory, at average cost |
| `2100` | VAT payable |
| `2150` | Goods received. A receipt credits it. `POST /v1/supplier-payments` debits it |
| `2200` | Customer wallet. Equals the sum of `customers.balance_minor` |
| `2300` | Payment suspense. Confirmed money not yet applied to an order |
| `2310` | Refunds payable. Refunds waiting to leave the provider |
| `2400` | Deposits held. Cash and wallet tenders of a split order awaiting its other tender. Created on first use |
| `3000` | Opening equity |
| `4000-IKJ` / `4000-SUL` | Sales at that outlet |
| `5000` | Cost of goods sold |
| `5100` | Payment fees. Created on first use |
| `5200` | Inventory adjustment. May go negative, because raising an average cost credits it |
| `5300` | Cash over/short. Created on first use |

Clearing is per connection. Sales and cash are split by outlet. The outlet code is the account suffix because a UUID v7's first digits stay the same for about a minute, and two branches opened together would otherwise collide.

## Curl examples

The server is on port 8080 with the seed loaded. These commands are bash. On PowerShell, `curl` is an alias for `Invoke-WebRequest`. Call `curl.exe`, or use the same URLs from any client.

```bash
BASE=http://127.0.0.1:8080

curl -s $BASE/readyz

TOKEN=$(curl -s -X POST $BASE/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@till.local","password":"change-me-now-12"}' | jq -r .token)

curl -s $BASE/v1/auth/me -H "Authorization: Bearer $TOKEN"

IKEJA=$(curl -s $BASE/v1/outlets -H "Authorization: Bearer $TOKEN" \
  | jq -r '.outlets[] | select(.code=="IKJ") | .id')

# Cash sale. The body has no price. Retrying this same key returns the same order.
curl -s -D - -X POST $BASE/v1/orders \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-cash-serum-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"payment_method\":\"cash\",\"lines\":[{\"sku\":\"SERUM-C\",\"qty\":1}]}"

# Today's Lagos sales. matches_books should be true.
curl -s "$BASE/v1/reports/daily-sales?outlet_id=$IKEJA" -H "Authorization: Bearer $TOKEN"

# Store credit: create a customer, then top up with cash at Ikeja.
CUST=$(curl -s -X POST $BASE/v1/customers \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Ada Okonkwo","phone":"08030001111"}' | jq -r .id)

curl -s -X POST $BASE/v1/customers/$CUST/topups \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-wallet-topup-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"amount_minor\":2000000}"

# The two seeded payment connections. PSTK is Paystack, POS is the till-format terminal.
PSTK=$(curl -s $BASE/v1/payment-connections -H "Authorization: Bearer $TOKEN" \
  | jq -r '.payment_connections[] | select(.code=="PSTK") | .id')
POS=$(curl -s $BASE/v1/payment-connections -H "Authorization: Bearer $TOKEN" \
  | jq -r '.payment_connections[] | select(.code=="POS") | .id')

# Transfer holds 1 towel for 15 minutes. No journal yet.
HOLD=$(curl -s -X POST $BASE/v1/orders \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-transfer-towel-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"payment_method\":\"transfer\",\"lines\":[{\"sku\":\"TOWEL\",\"qty\":1}]}")
echo "$HOLD" | jq '{status,reference,total_minor,journal_id}'

# Paystack confirms it. The connection id in the URL picks the secret.
REF=$(echo "$HOLD" | jq -r .reference)
AMT=$(echo "$HOLD" | jq -r .total_minor)
BODY=$(printf '{"event":"charge.success","data":{"id":9001,"reference":"%s","amount":%s,"currency":"NGN","status":"success","channel":"bank_transfer"}}' "$REF" "$AMT")
SIG=$(printf '%s' "$BODY" | openssl dgst -sha512 -hmac dev-paystack-secret-change-me | awk '{print $2}')

curl -s -X POST $BASE/v1/webhooks/$PSTK \
  -H 'Content-Type: application/json' \
  -H "x-paystack-signature: $SIG" \
  -d "$BODY"

# Tap. A customer taps a card on the Ikeja terminal. The terminal posts the till format.
HOLD=$(curl -s -X POST $BASE/v1/orders \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-tap-towel-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"payment_method\":\"tap\",\"lines\":[{\"sku\":\"TOWEL\",\"qty\":1}]}")
REF=$(echo "$HOLD" | jq -r .reference)
AMT=$(echo "$HOLD" | jq -r .total_minor)
BODY=$(printf '{"id":"tap-1001","reference":"%s","amount_minor":%s,"method":"tap"}' "$REF" "$AMT")
T=$(date +%s)
SIG=$(printf '%s.%s' "$T" "$BODY" | openssl dgst -sha256 -hmac dev-till-secret-change-me | awk '{print $2}')

curl -s -X POST $BASE/v1/webhooks/$POS \
  -H 'Content-Type: application/json' \
  -H "Till-Signature: t=$T,v1=$SIG" \
  -d "$BODY"

# What each provider still holds for the shop.
curl -s $BASE/v1/payment-connections -H "Authorization: Bearer $TOKEN" | jq '.payment_connections[] | {code,balance_minor}'
```

A wrong signature returns `401` and leaves the order pending. Sending the same event id again returns the first answer with `Idempotent-Replayed: true`.

A cashier token against Surulere, while that cashier is only assigned to Ikeja, returns `403` with code `outlet_forbidden`.

## Tests

```bash
export TILL_REQUIRE_DB=1
export TILL_TEST_DATABASE_URL=postgres://till:till@127.0.0.1:5432/till_test?sslmode=disable
go test ./... -count=1 -timeout 900s
```

`make test` does the same once `TILL_TEST_DATABASE_URL` is set. The suite uses a real Postgres and the full integration run takes several minutes. CI runs it with `-race`. This laptop was not load-tested. Do not quote a requests-per-second number from it.

What the tests protect:

- A sale deducts stock and the books still balance. Selling more than is available is `409 insufficient_stock` and writes no journal. Thirty concurrent buyers of the last unit: one wins
- VAT on 450000 kobo at 750 bps is 31395 / 418605
- The same idempotency key does not deduct twice. A different body with that key is `409`
- An awaited-method order reserves stock and posts nothing until the payment event. A bad signature and a wrong amount do not post. An expired hold gives the stock back
- Roles and outlet scope: cross-merchant `404`, wrong outlet `403`
- Daily sales match the books. Wallet balance and account `2200` move together
- Averages, cost of goods, refunds, split tender, voids, discounts and tax classes (`tests/integration/sales_test.go`). Every scenario ends with the invariants holding
- Money safety (`tests/integration/money_safety_test.go`): late or mismatched payments land in suspense, control accounts refuse free-form postings, the database rejects unbalanced postings, and the sweeper skips a poison order
- Payments (`tests/integration/payments_test.go`): a tap sale confirmed by a till-format event, a transfer with no reference matched by amount, a manager recording a payment by hand, and connections isolated from each other
- Ledger tests reject unbalanced journals, JSON floats, and lost updates under concurrent postings

## How it is put together

```text
cmd/tilld                 process: migrate, seed, sweeper, event worker, HTTP
internal/httpserver       chi routes, orders, inbound payments, connections, reports
internal/payments         provider adapters (till, paystack, flutterwave, monnify) and the common Event
internal/checkout         sales, confirm, refund, receipts, unmatched payments, expiry
internal/ledger           double-entry post, reversals, immutability
internal/store            pgx and SQL
internal/events           public event types for the feed and outbound webhooks
internal/auth             argon2id, JWT, roles
internal/config           environment and the production guard
internal/ratelimit        login token buckets
internal/money            kobo parsing, no floats
internal/seed             Adaeze Studio
migrations                0001 ledger through 0008 payment connections
```

One stateless API process. One Postgres. Journals and postings are append-only: a trigger rejects updates and deletes. Reversals are new journals. Cached `balance_minor` moves in the same transaction as the postings. The journal is the source of truth.

Sorted row locks and `SKIP LOCKED` let more than one cashier sell at once without corrupting stock. That is the whole scale story. It is not a benchmark and it is not Kubernetes.

## What this wallet is

A customer can hold store credit at Adaeze Studio. The cashier takes cash, the drawer and the customer's balance go up, and the next sale can spend that balance in the same transaction that takes the stock. The shop owes the customer goods, which is why account `2200` is a liability. It is not a wallet you can withdraw to a bank, send to another person, or spend at another merchant. Money that can leave the shop is stored value, and in Nigeria that needs a Central Bank licence. Till does not do that, ask for KYC, pay interest, or move money between customers.

## Not built yet

- **Credit sales and a debtors book.** Every sale is paid at the till or by a confirmed payment.
- **Chargebacks and reversals.** A `reversed` event is audited and posts nothing.
- **Provider API calls.** Till never initiates a charge, a refund or a virtual account. It records and confirms what the provider reports.
- **Encrypted connection secrets.** They are stored unencrypted in Postgres.
- **An offline till.**

## Left out on purpose

- No licensed e-money wallet and no frontend.
- No microservices and no Kubernetes. The sale is one Postgres transaction. Splitting stock and the journal into separate services would let one succeed while the other failed. More than one copy of this same process, still talking to one database, is the scale path.
- WhatsApp intake and a manager phone page are a later product with its own name. They are not this API.

## Layout

Environment variables use the prefix `TILL_`. See `.env.example`. The binary is `tilld`. JWT issuer is `till`. The subject is the staff id. Claims carry `merchant_id`, `role`, `jti` and `tv` (token version). Passwords are argon2id, minimum 12 characters. Tokens last `TILL_TOKEN_TTL` (default 8 hours), and every request reloads the staff row so a disabled account or a changed role stops working immediately.
