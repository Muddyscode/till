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

**Webhook.** `POST /v1/webhooks/paystack` with header `x-paystack-signature` set to the hex HMAC-SHA512 of the raw body, keyed with `TILL_PAYSTACK_SECRET`. The amount is kobo, and it must match the amount the customer pays (the shelf total minus any discount). Only `charge.success` can post. A bad signature is `401` and changes nothing. An unknown reference, or an amount or currency that does not match the order, returns `200` with `{"status":"ignored"}` and an audit row. It does not post and it does not touch stock. A matching event posts one journal (debit Paystack clearing `1100`, credit outlet sales, credit VAT, plus cost of goods when the average is above zero), commits the reservation, and marks the order paid. Sending the same `data.id` again does not add a second journal. An expired or already-paid order is ignored (`webhook.order_not_pending`).

**Refund.** A manager or admin restocks and reverses the sale journal in one transaction. That reversal puts the snapshotted cost back into inventory. It does not recompute the average, so a receipt that landed during the sale can leave quantity times average a few kobo away from the inventory account until the next receipt or a manual average change. `POST /v1/journals/{id}/reversal` refuses a journal that belongs to an order (`409 refund_via_order`). Refund the order so the shelf and the books move together.

**Expiry.** Every 30 seconds a sweeper locks due holds with `FOR UPDATE SKIP LOCKED`, releases `reserved`, sets the order to `expired`, and writes `reservation.released`. It does not post a journal.

## Staff and outlets

| Role | Can do |
|---|---|
| Cashier | Log in, see assigned outlets, sell, read stock, create a customer, take a cash top-up |
| Manager | Cashier, plus products, receipts, adjustments, refunds, daily sales, audit for their outlet |
| Admin | Manager, plus staff, outlets, the chart of accounts, and free-form journals |

An outlet id from another merchant is `404`. The same merchant, but a staff member who is not assigned to that outlet, is `403 outlet_forbidden`. Admins are not assigned outlet-by-outlet. They see the whole merchant. The last active admin cannot be disabled (`409 last_admin`).

`GET /v1/reports/daily-sales` always requires `outlet_id`, including for an admin. The day is an Africa/Lagos calendar date. Omit `date` and the report uses today in Lagos. Admin may omit `outlet_id` on `GET /v1/orders` and `GET /v1/audit`. A manager or cashier must pass one.

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

`Idempotency-Key` is required on orders, refunds, wallet top-ups, transfers, journals, and reversals. The key is scoped to the merchant.

- Same key and the same body: the stored response is returned. Stock is not deducted twice and the journal is not posted twice.
- Same key and a different body: `409 idempotency_conflict`.
- A key that is still inside its transaction: `409 idempotency_in_progress`.

The webhook's key is `wh:{data.id}` on the merchant that owns the order.

## Reports

`matches_books` is true when the day's paid-order total equals the asset and liability debits on those sale journals. Cash and Paystack are asset debits. A wallet payment is a liability debit, so it still counts. Cost of goods is an expense debit and is left out, which is why a sale with a cost layer still matches. A refunded order is not in the total. Pending and expired orders are not in the total. Receipts and revaluations are not sale journals, so they are not in the figure either.

`GET /v1/reports/low-stock` and `GET /v1/reports/stock-on-hand` take `outlet_id`. Ikeja soap is seeded at 3 on hand with reorder level 5, so it is low. Surulere soap is 20 and is not.

`GET /v1/invariants` is admin-only. `holds` is true when this identity is zero and the cached balances match the journal:

```text
SUM(asset, expense) - SUM(liability, equity, income) = 0
```

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
migrations                0001 ledger, 0002 POS, 0003 wallet and cost
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

Environment variables use the prefix `TILL_`. See `.env.example`. The binary is `tilld`. JWT issuer is `till`. The subject is the staff id. Claims carry `merchant_id` and `role`. Passwords are argon2id, minimum 12 characters. Tokens last 12 hours, and every request reloads the staff row so a disabled account or a changed role stops working immediately.
