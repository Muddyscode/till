# Till

Till is a point-of-sale ledger and inventory API for one merchant with more than one shop. A cash sale takes stock off the shelf and writes the money into the books in the same Postgres transaction. A transfer holds the stock until a signed Paystack-style webhook says the money arrived. If that sale fails, the stock and the journal roll back together.

Money is integer kobo. Posted journals cannot be edited. Stock cannot go negative. A cashier cannot send a price. The shelf price is snapshotted onto the order.

This repository is a Go service (`cmd/tilld`). A Node.js till, WhatsApp bot, or admin screen would call this HTTP API. The books stay here because the tested double-entry engine is what keeps the sales total equal to the money.

## Run it

This laptop cannot run Docker. Virtualization (VT-x) is off, so there is no Dockerfile and no Compose file. GitHub Actions starts Postgres 16 as a service. It does not build an image of Till.

You need Go 1.25 and Postgres 16 listening on `127.0.0.1:5432`.

```text
postgres://till:till@127.0.0.1:5432/till?sslmode=disable
```

```bash
cp .env.example .env
go run ./cmd/tilld
```

`TILL_SEED=true` loads **Adaeze Studio** once. The seed checks for `admin@till.local` and does nothing on the next boot. Opening cash is posted only in that first transaction.

| Who | Email | Outlet | Password |
|---|---|---|---|
| Admin | `admin@till.local` | every outlet | `change-me-now-12` |
| Ikeja manager | `manager.ikeja@till.local` | Ikeja `IKJ` | `change-me-now-12` |
| Ikeja cashier | `cashier.ikeja@till.local` | Ikeja `IKJ` | `change-me-now-12` |
| Surulere cashier | `cashier.surulere@till.local` | Surulere `SUL` | `change-me-now-12` |

That password and the secrets in `.env.example` are for a local demo. Change them before any real deployment. There is no public register.

OpenAPI: [`api/openapi.yaml`](api/openapi.yaml). A short walkthrough is in [`docs/till-walkthrough.pdf`](docs/till-walkthrough.pdf).

## What a sale does

**Cash.** One database transaction locks the product rows (for cost), then the stock rows, both in id order. It copies the server price, subtracts `on_hand`, and posts one journal:

- Debit outlet cash `1000-{CODE}` for the amount the customer pays
- Credit outlet sales `4000-{CODE}` for the net
- Credit VAT payable `2100` for the VAT, omitted when VAT is 0
- Debit cost of goods `5000` and credit inventory `1200` for quantity times average cost, omitted when that cost is 0

The order is `paid`. The response is `201`. Audit action: `order.paid`.

**Wallet.** Same stock movement and the same cost lines. The debit is customer wallet `2200` instead of cash, and `customers.balance_minor` drops by the same amount. The balance cannot go below zero (`409 insufficient_wallet`). `customer_id` is required. Cash and transfer reject it. A refund of a wallet sale puts that amount back on the balance in the same transaction as the reversal.

**Transfer.** The same transaction does not touch `on_hand` and does not post a sale. It increases `reserved`, stores the order as `pending_payment`, and holds the units for `TILL_HOLD_MINUTES` (default 15). Unconfirmed money is not revenue. There is no manager "mark paid" button. The webhook is the only writer of that sale journal. Cost of goods is snapshotted when the webhook pays, not when the hold starts, because a receipt can change the average during the hold.

**Webhook.** `POST /v1/webhooks/paystack` with header `x-paystack-signature` set to the hex HMAC-SHA512 of the raw body, keyed with `TILL_PAYSTACK_SECRET`. The amount is kobo. Only `charge.success` can post. A bad signature is `401` and changes nothing. Every signature-valid `charge.success` is recorded once in `provider_events` (primary key `(provider, event_id)`, the Paystack `data.id`); sending the same id again replays the stored response with `Idempotent-Replayed: true` and posts nothing. A matching event on a pending order for the right amount in NGN posts one journal (debit Paystack clearing `1100`, credit outlet sales, credit VAT, plus cost of goods when the average is above zero), commits the reservation, marks the order paid and stores the event id on `payments.provider_event_id`.

**Late or mismatched money is never dropped.** If the reference matches an order but the payment cannot be applied (order expired, already paid, refunded, wrong amount, wrong currency), the webhook posts Dr Paystack clearing `1100` / Cr Payment suspense `2300` and opens an `unmatched_payments` row (`open`). A reference that matches no order has no merchant to post to, so only the `provider_events` row and an audit row are written. Managers and admins resolve open rows (all need `Idempotency-Key`):

- `GET /v1/unmatched-payments?outlet_id=&status=`
- `POST /v1/unmatched-payments/{id}/apply` turns the money into the paid sale. It works only for an expired order, when the amount equals the order total and the stock is available again; otherwise `409`. The sale journal debits `2300` instead of clearing.
- `POST /v1/unmatched-payments/{id}/refund` moves it to `refund_pending`: Dr `2300` / Cr Refunds payable `2310`.
- `POST /v1/unmatched-payments/{id}/refunded` records the payout: Dr `2310` / Cr `1100`.

**Refund.** A manager or admin restocks and reverses the sale journal in one transaction; an optional `reason` is stored with `refund_journal_id`, `refunded_at` and `refunded_by` on the order. Products are locked in id order before stock. That reversal puts the snapshotted cost back into inventory. It does not recompute the average, so a receipt that landed during the sale can leave quantity times average a few kobo away from the inventory account until the next receipt or a manual average change. If an account would go negative (for example the drawer was already banked) the answer is `409 refund_insufficient_funds` naming the account. A refunded **transfer** sale credits Refunds payable `2310` instead of clearing, because the money still has to leave through Paystack; the response carries `refund_record_id` and the same unmatched-payments record (`reason: order_refund`, `refund_pending`) is closed with `POST /v1/unmatched-payments/{id}/refunded`. `POST /v1/journals/{id}/reversal` refuses a journal that belongs to an order (`409 refund_via_order`). Refund the order so the shelf and the books move together.

**Control accounts.** Free-form `POST /v1/journals`, `POST /v1/transfers` and raw reversals refuse any line on the merchant's system accounts (`1100`, `1200`, `2100`, `2150`, `2200`, `2300`, `2310`, `5000`) with `409 control_account`. They move only through sales, receipts, top-ups and the flows above. (Paying a supplier from cash against `2150` therefore needs its own flow; it is no longer a free-form transfer.)

**Expiry.** Every 30 seconds a sweeper finds due holds and releases each one in its own transaction (stock rows locked in product id order, deadlocks retried). Each release sets the order to `expired` and writes `reservation.released`; it posts no journal. A failing order is logged with `slog` and skipped for that tick so it cannot starve the others. The same loop deletes `idempotency_keys` older than 30 days (at most hourly).

## Staff and outlets

| Role | Can do |
|---|---|
| Cashier | Log in, see assigned outlets, sell, read stock, create a customer, take a cash top-up |
| Manager | Cashier, plus non-price product edits, receipts, adjustments, refunds, daily sales, audit for their outlet |
| Admin | Manager, plus creating products and price or cost changes, staff, API keys, outlets, the chart of accounts, and free-form journals |

An outlet id from another merchant is `404`. The same merchant, but a staff member who is not assigned to that outlet, is `403 outlet_forbidden`. Admins are not assigned outlet-by-outlet. They see the whole merchant. The last active admin cannot be disabled (`409 last_admin`).

`GET /v1/reports/daily-sales` always requires `outlet_id`, including for an admin. The day is an Africa/Lagos calendar date. Omit `date` and the report uses today in Lagos. Admin may omit `outlet_id` on `GET /v1/orders` and `GET /v1/audit`. A manager or cashier must pass one.

## Security and operations

**Login.** Per-IP and per-email token buckets (`429 rate_limited` with `Retry-After`; they live in process memory, so with several instances each enforces its own limit). Ten failed logins in 15 minutes lock the account for 15 minutes (`429 account_locked`), stored in Postgres so it holds across instances. Unknown emails are verified against a dummy argon2 hash so they look like a wrong password, argon2 runs behind a 4-way semaphore, and the login body is capped at 4 KiB.

**Tokens.** Access tokens carry `jti` and the staff row's `token_version`; `TILL_TOKEN_TTL` sets the lifetime (default `8h`). `POST /v1/auth/logout` and `POST /v1/auth/password` (current password required, 12 to 128 characters) bump the version and kill every older token. An admin can `POST /v1/staff/{id}/password-reset`.

**API keys.** Admin `POST /v1/api-keys` with `name`, `role` (`cashier` or `manager`) and `outlet_ids` returns `till_live_<prefix>_<secret>` once. Send it as `Authorization: Bearer till_live_...`. Only a SHA-256 digest is stored. `GET /v1/api-keys` lists, `DELETE /v1/api-keys/{id}` revokes at once. Each key is backed by a non-login staff row, so outlet scope, roles, orders and audit work unchanged (the audit `staff_id` is the key's `staff_id`).

**Authorization.** Only admins create products or change `price_minor` / `avg_cost_minor` (managers get `403`). Cashiers list customers only with `q` (3+ characters) and see phones masked as `0803****567`; managers and admins see full numbers. Phones are stored as E.164 (`+2348031234567`).

**Pagination.** `GET /v1/orders`, `/v1/journals`, `/v1/transfers` and `/v1/audit` return `next_cursor` (null on the last page); pass it back as `cursor`. Orders also filter by `status`, `reference`, `customer_id`, `from`, `to`.

**Production guard.** `TILL_ENV=production` refuses the `.env.example` secrets, short secrets (JWT under 48, Paystack under 24 characters), `TILL_SEED=true`, and `TILL_CORS_ORIGINS=*`.

**HTTP.** `X-Request-ID` in and out, JSON access log per request (route pattern, status, duration, merchant, staff, api key), JSON `404`/`405`/`413`, `Cache-Control: no-store` on `/v1`, CORS only for origins in `TILL_CORS_ORIGINS`.

**Ops.** `/livez`, `/readyz` (database, sweeper heartbeat, draining), `/metrics` (Prometheus text), graceful shutdown waits for the sweeper. `Dockerfile` builds a non-root image; CI builds it. Backups, restore drill, rotation and incident steps: [docs/RUNBOOK.md](docs/RUNBOOK.md).

## Money, VAT, and stock

- Currency is NGN. Amounts are `BIGINT` kobo. `100` kobo is ₦1. JSON floats and scientific notation are `400`.
- VAT is included in the price the customer pays. The merchant rate is `vat_inclusive_bps`. The seed uses `750`, which is 7.5%.
- `vat = payable * bps / (10000 + bps)`, integer division. The leftover kobo stays in sales, so net plus VAT equals the payable. A ₦4,500.00 line (`450000` kobo) at 750 bps is VAT `31395` and net `418605`.
- `discount_minor` is an optional order-level integer. Omit it, or send `0`, and nothing is taken off. It must be less than the shelf total. A ₦500.00 discount (`50000` kobo) on that ₦4,500.00 line makes the payable `400000`, VAT `27906`, and net `372094`. The cashier still cannot send a unit price. A discount does not reduce cost of goods.
- Average cost is `avg_cost_minor` on the product. A receipt may send `unit_cost_minor`. The new average is `(units on hand * old average + quantity * unit cost) / (units on hand + quantity)`, rounded down. Omit the unit cost and the current average is kept. The first receipt that names a cost, when nothing is on hand, sets the average to that cost.
- Available stock is `on_hand - reserved`. Low stock means available is less than or equal to `reorder_level`.
- Postgres checks: `on_hand >= 0`, `reserved >= 0`, `reserved <= on_hand`.
- Stock rows are locked `ORDER BY product_id`. Account rows are locked `ORDER BY id`. That order is what keeps two cashiers from deadlocking each other.

Order references look like `IKJ-A1B2C3D4-000001`. The outlet code repeats across merchants, so the middle token is the random tail of that outlet's id. Paystack sends the reference back with no merchant id, and the reference is unique in the database.

## Idempotency

`Idempotency-Key` is required on orders, refunds, wallet top-ups, transfers, journals, reversals, stock receipts, stock adjustments, product `PATCH` and the unmatched-payment actions. The key is scoped to the merchant and bound to a fingerprint of the HTTP method, the resolved path (ids included) and the canonical body (numbers are compared exactly, never as floats).

- Same key and the same request: the stored response is returned with `Idempotent-Replayed: true`. Stock is not deducted twice and the journal is not posted twice.
- Same key on a different order, customer, endpoint or body: `409 idempotency_conflict`.
- A key that is still inside its transaction: `409 idempotency_in_progress`.
- Keys starting with `wh:` or `sys:` (any case) are reserved for the system: `400 idempotency_key_reserved`.
- Keys are kept for 30 days, then purged.

Webhook dedupe does not use this table; see `provider_events` under Webhook.

## Reports

`matches_books` is true when the day's paid-order total equals the asset and liability debits on those sale journals. Cash and Paystack are asset debits. A wallet payment is a liability debit, so it still counts. Cost of goods is an expense debit and is left out, which is why a sale with a cost layer still matches. A refunded order is not in the total. Pending and expired orders are not in the total. Receipts and revaluations are not sale journals, so they are not in the figure either.

`GET /v1/reports/low-stock` and `GET /v1/reports/stock-on-hand` take `outlet_id`. Ikeja soap is seeded at 3 on hand with reorder level 5, so it is low. Surulere soap is 20 and is not.

`GET /v1/invariants` is admin-only and returns `checks`, each with `name`, `holds`, `expected`, `actual`. `holds` is true when every check holds. Besides the identity below and the cached balances matching the journal, it checks that wallet `2200` equals the sum of customer balances, suspense `2300` equals the open unmatched payments, Refunds payable `2310` equals the refunds waiting for payout, and inventory `1200` is within one kobo per unit of the sum of `on_hand` times average cost (the check reports `residue_minor` and `tolerance_minor`, because average-cost rounding leaves a few kobo). The identity:

```text
SUM(asset, expense) - SUM(liability, equity, income) = 0
```

## Shop operations (Phase 3B)

Migration `0007_operations.sql`. All of it is derived from, or sits beside, the existing sale code.

- **Shifts and cash-up.** `POST /v1/shifts/open`, `GET /v1/shifts`, `GET /v1/shifts/{id}`, `GET /v1/shifts/{id}/z-report`, `POST /v1/shifts/{id}/close`. One open shift per staff member per outlet (`409 shift_already_open`). Expected cash is the opening float plus debits minus credits to the outlet cash account `1000-CODE` between open and close, for journals that staff member posted. Journals carry no actor, so each is matched to its staff member through the audit event written in the same transaction (same `created_at`). A count that differs posts the variance against account `5300` "Cash over/short" (created on first use). The Z-report is returned by close and frozen. A manager or admin may close anyone's shift. Not built: a `require_open_shift` merchant setting; enforcing it needs a hook in the sale path.
- **Event feed and webhooks.** `GET /v1/events?cursor=&types=` (admin) lists public events derived from `audit_events`. `POST/GET/DELETE /v1/webhook-endpoints` and `GET /v1/webhook-endpoints/{id}/deliveries` manage endpoints; the signing secret is shown once. The worker in `cmd/tilld` fans new events out and POSTs them with `Till-Signature: t=<unix>,v1=<hex HMAC-SHA256 of "t.body">` and `Till-Event-Id`, retrying with exponential backoff up to 8 attempts, 10 s timeout, redirects never followed, private and link-local targets blocked in production. Several instances are safe (cursor row lock, `SKIP LOCKED`). `stock.low` is computed by the worker, so it is webhook-only. Delivery is at-least-once; dedupe on `Till-Event-Id`. The worker holds back events younger than 1 s so a slow commit is not skipped by the cursor.
- **Stock transfers.** `POST /v1/stock/transfers` deducts from the source (never below reserved) and sets `in_transit`; `.../receive` adds to the destination; `.../cancel` returns to the source. No journal: inventory `1200` is merchant-wide and cost is per product. `/v1/invariants` counts units in transit at average cost.
- **Stock counts.** `POST /v1/stock/counts` sets `on_hand` to the counted figure, posting any variance through the normal adjustment path (`5200`, reason `stock_count`). Counting below reserved is `409`.
- **Suppliers.** CRUD on `/v1/suppliers`; a stock receipt may carry `supplier_id`. `POST /v1/supplier-payments` posts Dr `2150` / Cr outlet cash (`cash`) or Cr `1100` (`bank`). `GET /v1/suppliers/{id}/statement` shows receipts, payments and the balance owed.
- **VAT summary.** `GET /v1/reports/vat?from=&to=` (admin): output VAT (credits to `2100`) and reversals by Lagos month. Input VAT is a placeholder `0`.

## Seeded shop

Adaeze Studio sells skincare. Outlets are Ikeja `IKJ` and Surulere `SUL`.

| SKU | Price (kobo) | Average cost (kobo) | Reorder | Ikeja on hand | Surulere on hand |
|---|---:|---:|---:|---:|---:|
| `SERUM-C` | 850000 (₦8,500) | 320000 | 2 | 20 | 20 |
| `SHEA-500` | 450000 (₦4,500) | 180000 | 2 | 20 | 12 |
| `TOWEL` | 120000 (₦1,200) | 40000 | 2 | 30 | 15 |
| `SOAP` | 200000 (₦2,000) | 70000 | 5 | 3, low | 20 |

Opening journal: debit Cash Ikeja ₦50,000 (`5000000`), debit Cash Surulere ₦20,000 (`2000000`), debit Inventory `21970000` (the stock the shop already owns, at those averages), credit Opening equity `3000` for `28970000`. No sale is seeded. The seed writes that inventory debit itself. It does not also post a receipt, or the stock would be counted twice.

The seed runs once. If `admin@till.local` is already in the database, a restart does not add average costs or the inventory debit. To load this version of the sample, stop Till and drop the database `till` (not `till_test`), then start again with `TILL_SEED=true`. The test database is rebuilt by the test suite.

Chart of accounts:

| Code | Account |
|---|---|
| `1000-IKJ` / `1000-SUL` | Cash at that outlet |
| `4000-IKJ` / `4000-SUL` | Sales at that outlet |
| `1100` | Paystack clearing, one for the merchant |
| `1200` | Inventory, at average cost |
| `2100` | VAT payable |
| `2150` | Goods received. A receipt credits this. Paying the supplier later is an admin transfer: debit `2150`, credit cash |
| `2200` | Customer wallet. Store credit. The sum of `customers.balance_minor` equals this account |
| `3000` | Opening equity |
| `5000` | Cost of goods sold |
| `5200` | Inventory adjustment. Allowed to go negative, because raising an average cost credits this account |

Clearing is merchant-wide. Sales and cash are split by outlet. The outlet code is the account suffix because a UUID v7's first digits stay the same for about a minute, and two branches opened together would otherwise collide.

## Curl examples

The server is on port 8080 with the seed loaded. These commands are bash. On PowerShell, `curl` is an alias for `Invoke-WebRequest`. Call `curl.exe`, or use the same URLs from any client.

```bash
BASE=http://127.0.0.1:8080

curl -s $BASE/v1/health

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

# Ikeja soap is low (3 on hand, reorder 5).
curl -s "$BASE/v1/reports/low-stock?outlet_id=$IKEJA" -H "Authorization: Bearer $TOKEN"

# Today's Lagos sales. matches_books should be true.
curl -s "$BASE/v1/reports/daily-sales?outlet_id=$IKEJA" -H "Authorization: Bearer $TOKEN"

# Store credit. The phone is the key. Top-up takes cash at Ikeja.
CUST=$(curl -s -X POST $BASE/v1/customers \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Ada Okonkwo","phone":"08030001111"}' | jq -r .id)

curl -s -X POST $BASE/v1/customers/$CUST/topups \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-wallet-topup-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"amount_minor\":2000000}"

# Spend ₦2,000 of that credit on soap. No price in the body.
curl -s -X POST $BASE/v1/orders \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-wallet-soap-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"payment_method\":\"wallet\",\"customer_id\":\"$CUST\",\"lines\":[{\"sku\":\"SOAP\",\"qty\":1}]}"

# Transfer holds 1 towel for 15 minutes. No journal yet.
HOLD=$(curl -s -X POST $BASE/v1/orders \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-transfer-towel-1' \
  -d "{\"outlet_id\":\"$IKEJA\",\"payment_method\":\"transfer\",\"lines\":[{\"sku\":\"TOWEL\",\"qty\":1}]}")
echo "$HOLD" | jq '{status,reference,total_minor,journal_id}'

export TILL_PAYSTACK_SECRET=dev-paystack-secret
REF=$(echo "$HOLD" | jq -r .reference)
AMT=$(echo "$HOLD" | jq -r .total_minor)
BODY=$(printf '{"event":"charge.success","data":{"id":9001,"reference":"%s","amount":%s,"currency":"NGN","status":"success"}}' "$REF" "$AMT")
SIG=$(printf '%s' "$BODY" | openssl dgst -sha512 -hmac "$TILL_PAYSTACK_SECRET" | awk '{print $2}')

curl -s -X POST $BASE/v1/webhooks/paystack \
  -H 'Content-Type: application/json' \
  -H "x-paystack-signature: $SIG" \
  -d "$BODY"
```

`TILL_PAYSTACK_SECRET` in `.env.example` is `dev-paystack-secret`. A wrong signature returns `401` and leaves the order pending.

A cashier token against Surulere, while that cashier is only assigned to Ikeja, returns `403` with code `outlet_forbidden`.

## Tests

```bash
export TILL_REQUIRE_DB=1
export TILL_TEST_DATABASE_URL=postgres://till:till@127.0.0.1:5432/till_test?sslmode=disable
go test ./... -count=1
```

The suite uses a real Postgres. It is not a race-detector run, and this laptop was not load-tested. Do not quote a requests-per-second number from it.

The tests that protect the shop:

- A cash sale deducts stock and the books still balance
- VAT on 450000 kobo at 750 bps is 31395 / 418605
- Selling more than available returns `409 insufficient_stock` and writes no journal
- The same idempotency key does not deduct twice. A different body with that key is `409`
- Thirty concurrent buyers of the last unit: one sale wins
- A transfer reserves stock and posts nothing until the webhook
- A bad signature and a wrong amount do not post
- An expired hold gives the stock back
- Roles and outlet scope: cross-merchant `404`, wrong outlet `403`
- Daily sales match the books, including a discounted cash sale and a wallet sale that both carry cost of goods
- A wallet top-up, a wallet sale, and a refund move the customer balance and account `2200` together. The balance cannot be overdrawn
- A receipt updates the weighted average, a sale posts cost of goods, and a manual average change revalues the units on hand
- Audit records the actor. A refund restocks. A raw reversal of a sale journal is refused
- Money safety (`tests/integration/money_safety_test.go`): idempotency keys are bound to method, path and body, webhook dedupe lives in `provider_events`, late or mismatched payments land in suspense and can be applied or refunded, control accounts refuse free-form postings, the database rejects unbalanced or late-added postings, and the sweeper skips a poison order
- A stock adjustment cannot pass below reserved or below zero

Ledger tests still reject unbalanced journals, JSON floats, and lost updates under concurrent transfers.

## How it is put together

```text
cmd/tilld                 process: migrate, seed, sweeper, HTTP
internal/httpserver       chi routes, auth, orders, webhook, reports
internal/checkout         cash sale, wallet, reserve, confirm, refund, receipts, expiry
internal/ledger           double-entry post, reversals, immutability
internal/store            pgx and SQL
internal/seed             Adaeze Studio
migrations                0001 ledger, 0002 POS, 0003 wallet and cost, 0005 security
```

One stateless API process. One Postgres. Journals and postings are append-only: a trigger rejects updates and deletes. Reversals are new journals. Cached `balance_minor` moves in the same transaction as the postings. The journal is the source of truth.

That is the scale story, and it is the whole scale story. Sorted row locks and `SKIP LOCKED` let more than one cashier sell at once without corrupting stock. It is not a claim that this machine was benchmarked, and it is not Kubernetes.

## What this wallet is

A customer can hold store credit at Adaeze Studio. The cashier takes cash, the drawer goes up, and the customer's balance goes up. The next sale can spend that balance in the same transaction that takes the stock. The shop still owes the customer goods, which is why account `2200` is a liability.

That is not a wallet you can withdraw to a bank, send to another person, or spend at another merchant. Holding money that can leave the shop is stored value. In Nigeria that needs a Central Bank licence. Till does not do that, and it does not ask for KYC, pay interest, or move money between customers.

## Left out on purpose

- No licensed e-money wallet, no live Paystack API, and no frontend.
- No microservices and no Kubernetes. The sale is one Postgres transaction. Splitting stock and the journal into separate services would let one succeed while the other failed. This laptop also cannot run Docker, so it cannot run a cluster. More than one copy of this same process, still talking to one database, is the scale path.
- Shift close, an offline till, WhatsApp intake, and a manager phone page are a later product with its own name. They are not this API.

## Layout

Environment variables use the prefix `TILL_`. See `.env.example`. The binary is `tilld`. JWT issuer is `till`. The subject is the staff id. Claims carry `merchant_id`, `role`, `jti` and `tv` (token version). Passwords are argon2id, minimum 12 characters. Tokens last `TILL_TOKEN_TTL` (default 8 hours), and every request reloads the staff row so a disabled account or a changed role stops working immediately.
