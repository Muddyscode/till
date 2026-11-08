# Till — Product Review, Pre-mortem and Execution Plan

Date: 2026-11-07. Scope: the full working tree (uncommitted pivot from `ledgerd` to `tilld`).
Method: four parallel Sonnet audits (ledger core, HTTP/security, tests/CI/ops, market research), with the high-severity claims re-checked against the code by hand.

**Important caveat:** this machine has no Go toolchain and no Postgres. Nothing in the current uncommitted change set has been compiled or tested locally. Every finding below comes from reading code, not running it.

---

## 1. What Till is, and what I think of it

Till is a headless Go + Postgres API that gives a Nigerian merchant with several shops a single source of truth: a sale moves stock and posts a balanced double-entry journal **in the same transaction**. Transfers are held (stock reserved) until a signed Paystack webhook confirms the money. Money is integer kobo. Posted journals are immutable at the database level. Cashiers cannot send prices.

**What is genuinely strong**
- The engineering core is better than most early-stage POS products: integer kobo with overflow guards, strict JSON number parsing, deterministic lock ordering (accounts by id, stock by product id), stock guards in both `UPDATE … WHERE` and `CHECK` constraints, immutability triggers on journals/postings/audit.
- The transfer design ("the webhook is the only writer of a paid transfer sale, there is no 'mark paid' button") directly answers the #1 Nigerian merchant pain: **fake transfer alerts**.
- Authorization is consistent (merchant → outlet → role) and the auth middleware re-reads staff on every request, so disabling a cashier is instant.
- The README is unusually honest about what the system does not do.

**What is weak**
- It is an **engine without a car**. There is no till UI, no SDK, no machine API keys, no outbound webhooks, no sandbox. Its success depends on someone else building the product.
- Several money-path edge cases lose or misattribute money (see §3). For a product whose entire promise is "the books always match the money", any one of these is fatal.
- It models the books well but models the **shop** thinly: no shifts/cash-up, no split tender, no partial refunds, no stock transfers between outlets, one VAT rate for every product.
- It has never been deployed: no container, no logs from the HTTP layer, no metrics, no backups plan, migrations unlocked on boot.

---

## 2. What we did not think about (gaps in the product)

### Shop-floor reality
| Missing | Why it matters |
|---|---|
| Shifts / till sessions / cash-up / Z-report / drawer variance | Staff theft is the owner's #2 fear. Without opening float + expected vs counted cash per cashier, the books balance but the owner still can't see *who* is short. |
| Split tender (part cash, part transfer, part wallet) | Extremely common in Nigeria. Cashiers will work around it, making the data fiction. |
| Partial / per-line refunds, exchanges, refund method choice | Only whole-order refunds exist. Discount is order-level and not apportioned to lines. |
| Void / cancel a pending transfer | A customer who walks away holds stock for 15 min. |
| Inter-outlet stock transfer, stock counts, write-offs with reasons | Multi-outlet merchants move stock between shops weekly. |
| Product variants, barcodes, units of measure | Fashion (sizes), pharmacy (pack vs tablet), supermarkets (barcodes). |
| Per-product tax class (exempt / zero-rated) | Basic food, medical, baby products are VAT-exempt in Nigeria. One merchant-wide rate is wrong for pharmacies and supermarkets. |
| Discount limits and reasons by role | Any cashier can discount up to shelf − ₦0.01. This is a theft vector. |
| Suppliers, purchase orders, payables | Receipts exist, but there is no supplier ledger. |
| Offline mode | Power and network outages are routine. The idempotent API is a start, but there is no offline queue / sync story. |

### Payment reality
- **Most Nigerian SMEs receive transfers straight into their own Moniepoint / OPay / bank account, not through Paystack.** Till only confirms Paystack-style webhooks. This is the single biggest untested assumption in the build (§5).
- No settlement reconciliation (Paystack fees, payouts, bank match).
- Money that arrives *after* a hold expires, overpayments, underpayments and duplicate payments are acknowledged as "ignored" and never recorded anywhere. The customer has paid; the merchant's books have no trace.
- Refund of a transfer sale books money leaving the clearing account but never actually refunds via Paystack.

### Integrator reality (the people who must build on Till)
- Only staff email/password JWTs. A WhatsApp bot or till must store a human password. No API keys, no scopes, no refresh tokens, no logout.
- No outbound webhooks or event feed — a till must poll to learn a transfer was paid.
- Pagination is `limit` only (no cursor), so old orders/journals are unreachable. No filters by status, reference, customer, or date range.
- No SDK, no sandbox/test mode, no CORS (a browser admin screen cannot call it).

### Compliance reality
- **Nigeria Tax Act (effective 1 Jan 2026):** small businesses (≈ turnover ≤ ₦100m) are exempt from charging VAT. Till's seed assumes everyone charges 7.5%. The per-merchant rate makes 0% possible, but there is no input-VAT tracking and no VAT return export.
- **NRS (ex-FIRS) MBS e-invoicing:** phased; small taxpayers expected ~2027–2028. Till has no invoice numbering scheme, TIN fields, credit notes, or export.
- **NDPA 2023 / GAID 2025:** customer names, phones and wallet balances are stored in plaintext, visible to every cashier at every outlet, with no export/erasure path or retention policy.
- **CBN stored value:** the customer wallet may count as stored value. Untested legally.

---

## 3. What could have been done better (verified engineering findings)

### Critical / High — money can be lost or misattributed
1. **Idempotency hash ignores the URL.** `canonicalHash(raw)` hashes only the body (`internal/httpserver/json.go`), and keys are scoped per merchant, not per endpoint. Refund order A with key K and body `{}`, then order B with the same key → B returns A's cached 201 and is **not refunded**. Same for wallet top-ups to different customers: cash is taken, the wallet is not credited.
2. **Webhook key namespace can be squatted.** The webhook uses `wh:{data.id}` (`webhooks.go:93`); `requireIdempotencyKey` (`json.go:89`) accepts any client key including `wh:…`. A cashier who pre-claims a predictable Paystack event id makes the real webhook return 409 → the paid order expires, the customer's money is taken.
3. **Paid-after-expiry money is lost.** `webhooks.go:99` ignores any order not `pending_payment`, returns 200 (so Paystack never retries) and stores the "ignored" result permanently. No suspense journal, no payment record, no refund queue. Same for amount mismatch.
4. **`payments.provider_event_id` is never populated**, so its unique index is dead and there is no Paystack transaction id to reconcile against.
5. **Stock receipts, adjustments and product cost patches have no Idempotency-Key.** A till retry after a timeout doubles stock and the inventory journal.
6. **Free-form admin journals and raw reversals can post to control accounts** (wallet 2200, inventory 1200, VAT 2100, clearing 1100) without moving customer balances or stock, silently breaking the "wallet sum = 2200" and "stock × cost = 1200" invariants. `/v1/invariants` does not check either.
7. **Refunds can hard-fail with `insufficient_funds`** because outlet cash, clearing and VAT accounts disallow negative balances; a refund after the drawer was banked fails at the counter.
8. **No brute-force protection on login**, email enumeration via timing, and argon2 at 64 MiB per attempt makes unauthenticated login a memory-exhaustion DoS.
9. **Managers have merchant-wide price/cost authority** even though the role is described as outlet-scoped, and product PATCH is a lost update (read outside the transaction, unlocked full-row UPDATE) that can silently revert a receipt's new average cost.

### Medium
- Sweeper: errors discarded (`expire.go:62`), no logging, one poison order starves the batch, locks stock per order rather than in global product order (can deadlock with a sale and the sweeper has no retry), throughput 50 per 30s.
- Average-cost rounding leaves kobo stranded in inventory at zero stock; a product with an average cost but no receipt makes sales fail at the till.
- Daily sales report rewrites past days when an order is refunded; refunds never appear as their own line.
- Customer PII and wallet balances visible to every cashier merchant-wide; phone not normalised (`0803…` vs `+234803…` duplicates).
- 1 MiB body silently truncated into "invalid JSON" instead of 413; admin ledger endpoints accept unknown fields.
- Idempotency table never purged; stores every order response forever.

### Schema
- Tenant isolation is app-only (no composite FKs on merchant_id). Journal balance is enforced by app code only — no deferred DB constraint. `INSERT` of extra postings into an old journal is not blocked.
- Missing: partial index for the expiry scan, `orders(journal_id)`, `payments(order_id)`, a sargable index for the Lagos-date daily report, `postings(account_id, created_at)`.
- No refund link on orders (`refund_journal_id`, `refunded_at`, `refunded_by`, reason).

### Operations
- `httpserver` never logs. Every 500 is undiagnosable.
- No request IDs, metrics, tracing, readiness/liveness split, or sweeper health.
- Migrations run on every boot with **no advisory lock** (`internal/store/migrate.go`); two instances booting together can race. Migration 0002 is destructive (renames/drops) with no down path.
- Default `.env.example` secrets and `TILL_SEED=true` are accepted in any environment.
- No Dockerfile/deploy artefact, no backup/PITR plan.
- CI: Postgres service still named `ledger/ledger/ledger_test`; Go version unpinned (`stable`); `govulncheck@latest`; no `-race`. `.gitignore` still lists `/ledgerd`. `Server.MountConsole` is dead code. `ledger_test.go` still tests the old P2P ledger API.
- Integration tests **skip and exit 0** when Postgres is down unless `TILL_REQUIRE_DB=1` — a local run can look green while testing nothing.

### Test gaps
No test for: cross-merchant IDOR on POS endpoints; concurrent multi-SKU sales; sale vs sweeper; webhook vs sweeper; duplicate concurrent webhook; currency mismatch; non-`charge.success`; refund of a transfer or wallet sale (only cash is tested); idempotency key reuse across endpoints; concurrent wallet spend; double refund; refund of expired order; migrations on populated data; two instances; JWT expiry/tamper over HTTP; cashier negative paths.

---

## 4. Pre-mortem: it's November 2027 and Till failed. Why?

Ranked by likelihood × impact.

1. **Nobody built the car.** Till launched as an API. Micro-merchants buy apps, not APIs. The few agencies who looked at it found no SDK, no API keys, no sandbox, no outbound events, and went with Bumpa or an open-source ledger (Blnk) instead. *Usage never got past the demo merchant.*
2. **Transfers didn't flow through Paystack.** Merchants already had Moniepoint/OPay/bank accounts printed on the counter. Customers kept transferring there. The webhook never fired, so cashiers recorded transfers as "cash" to get the customer out the door. The anti-fraud promise — Till's best feature — was bypassed on day one.
3. **One unexplained discrepancy killed trust.** A customer paid 16 minutes after the hold expired, or a till retried a refund with a reused key. Money was taken and the books showed nothing. The owner's one reason to use Till ("the books match the money") was broken, and a ledger product does not get a second chance.
4. **It didn't match how the shop runs.** No cash-up per cashier, no split payments, no partial refunds, no stock transfers between outlets, wrong VAT for exempt goods. Staff improvised around it; the data stopped meaning anything; the owner went back to the notebook.
5. **Outages.** NEPA took the power, the network dropped, the online-only till stopped selling. Cashiers went back to paper "just for today" and never came back.
6. **Production incident with no visibility.** Two instances raced a migration, or the sweeper silently died, or a 500 storm hit at month-end. No logs, no metrics, no backups. Hours of downtime during peak trading.
7. **Regulatory drag.** The wallet was flagged as unlicensed stored value, or MBS e-invoicing became mandatory for the target segment before Till supported it, or an NDPA complaint about cashiers seeing every customer's phone and balance.
8. **Incumbents bundled it free.** Moniepoint/OPay added inventory to the terminal they already gave away; Bumpa added lending, making its POS the funnel.

---

## 5. Assumptions the build makes that have never been tested

| Assumption baked into the code | Why it's risky | How to test cheaply |
|---|---|---|
| Customer transfers arrive via Paystack | Most SMEs receive into their own Moniepoint/OPay/bank accounts | Interview 15 multi-outlet merchants: "where do customers transfer to today?" |
| Customers complete a transfer within 15 minutes | NIP delays and bank downtimes routinely exceed this | Log real time-to-webhook in a pilot; handle late money regardless |
| Developers/agencies want a headless POS ledger and will pay | No evidence of the buyer segment | 10 agency conversations + a landing page with an SDK waitlist |
| All merchants charge 7.5% inclusive VAT on everything | 2026 small-business exemption; exempt goods categories | Check the pilot merchants' VAT status and product mix |
| A merchant-wide moving average cost is good enough | Outlets may buy at different prices; FIFO may be needed for expiring goods (pharmacy) | Ask accountants of 3 target merchants |
| The shop is always online | Outages are routine | Measure uptime at a pilot outlet for 2 weeks |
| A customer wallet is not regulated stored value | CBN rules on closed-loop stored value | One payments-lawyer opinion |
| One staff email = one merchant | Accountants/owners often manage several businesses | Ask pilot users |
| One Paystack secret serves everyone | The schema is multi-tenant; the webhook secret is global | Required before a second merchant goes live |
| A single instance is enough and safe | Nothing tests two instances; migrations race | Run two instances in CI |
| Cashiers won't bypass the system | Recording a transfer as cash defeats the control | Cash-up variance per cashier makes bypass visible |
| Owners understand double-entry reports | They want "how much did I make, who is short, what's running out" | Show the pilot owner the reports; watch what they ask |

---

## 6. Why would anyone use Till instead of what they do today?

**What they do today:** a Moniepoint/OPay terminal or bank account for payments, a notebook or Excel for stock, WhatsApp screenshots as "proof of payment", and the owner calling each shop at night to ask for totals. Some use Bumpa.

**The honest answer today:** they wouldn't, because there is nothing for them to touch. A merchant cannot use an API.

**The answer Till should be able to give:**
> "You can't be fooled by a fake transfer alert, you know exactly which cashier is short at every shop every night, and your stock and money reconcile to the kobo — on your phone, without being there."

That is a real, painful, owner-level outcome that notebooks, terminals and Bumpa don't fully deliver. The engine Till already has is the hard part of delivering it. What's missing is (a) confirming transfers from the accounts merchants *already* use, (b) cash-up per cashier, and (c) a thin finished product (till app + owner's WhatsApp daily report) on top.

For **developers/agencies**, the pitch is: "Don't build books, stock and transfer-confirmation yourself — get them correct and idempotent out of the box," which only works with API keys, outbound webhooks, an SDK and a sandbox.

---

## 7. What will make it much more successful

1. **Fix every money-loss path first** (§3 Critical/High). The brand is "the books always match". Earn it.
2. **Confirm transfers from where money actually lands.** Provider-agnostic payment confirmation: Paystack, plus Monnify/Flutterwave dedicated virtual accounts and bank-account feeds (Mono). Record *every* inbound payment, including late, over- and under-payments, into a suspense/unmatched queue a manager resolves.
3. **Cash-up per cashier.** Shifts with opening float, expected vs counted cash, variance posted to a variance account, Z-report. This is the staff-theft feature owners will pay for.
4. **Ship the car, thinly.** A reference till PWA (works offline, queues with idempotency keys) and an owner's daily WhatsApp/email summary: sales per outlet, variance per cashier, low stock, unmatched payments.
5. **Make it buildable.** API keys with scopes, outbound signed webhooks + an event feed with a cursor, cursor pagination and filters, generated TypeScript SDK, sandbox mode, CORS.
6. **Make it operable.** Structured request logs, request IDs, metrics, readiness/liveness, migration advisory lock, Dockerfile, backup/PITR runbook, production guard on secrets.
7. **Fit the shop.** Split tender, partial/line refunds, discount limits by role, per-product tax class, inter-outlet transfers, stock counts.
8. **Compliance as a feature.** VAT-exempt mode for small businesses, input VAT, VAT return export, MBS-ready invoice numbering, NDPA export/erasure and PII role restriction. Get a legal opinion on the wallet.
9. **Price for the segment.** Per-outlet monthly pricing benchmarked against Bumpa (~₦5k–25k/month), with a free single-outlet tier.

---

## 8. Execution plan

The plan is phased so that each phase leaves the product shippable and fully tested. Phases 0–2 are engineering I can execute in this repository now. Phases 3–4 are larger product work; I'd execute the engineering pieces of 3 next and treat 4 as product/business work.

### Phase 0 — Make the build real (prerequisite)
- Install Go 1.25 and Postgres 16 locally (winget), create `till` / `till_test` with the README DSN.
- Commit the current pivot on a branch (`feat/till-pos`) so every later change is reviewable.
- Run `go build`, `go vet`, unit tests, integration tests with `TILL_REQUIRE_DB=1`, `-race`. Fix anything that fails before changing behaviour.
- Fix CI: Postgres `till/till/till_test`, pin Go to 1.25.x, pin `govulncheck`, add `-race`, `TILL_REQUIRE_DB=1`, `gofmt`/`staticcheck`.
- Clean stale bits: `.gitignore` ledgerd lines, dead `MountConsole`, decide on legacy `ledger_test.go` (keep the generic ledger invariants, drop P2P-only tests).

### Phase 1 — Money safety (P0, each with a regression test)
1. Idempotency: scope keys to `(merchant, method, route template)`, hash `method + path + body`; store request fingerprint; reject client keys in reserved namespaces; move webhook dedupe to its own `provider_events` table keyed by `(provider, event_id)`.
2. Idempotency-Key required on receipts, adjustments, product cost changes; `Idempotent-Replayed: true` header on replays; TTL purge job for old keys.
3. Inbound payments: record **every** verified webhook in `payments` with `provider_event_id`. Late/over/under/duplicate/unknown payments post to a **payment suspense** liability and create an `unmatched_payments` row; endpoints for a manager to apply to an order or mark for refund.
4. Control-account protection: free-form journals and raw reversals cannot touch wallet, inventory, VAT, clearing or suspense accounts.
5. Invariants: extend `/v1/invariants` with wallet sum = 2200, payment suspense = unmatched sum, inventory GL vs Σ(on_hand × avg) with an explained rounding residue.
6. DB-level: deferred constraint trigger enforcing per-journal balance; block INSERT of postings into an already-committed journal; partial and missing indexes; uniqueness on `payments(order_id)` for paid, `stock_reservations(order_id, product_id)`.
7. Product PATCH and sale pricing read under lock inside the transaction; refund locks product rows in order.
8. Refund: clear error codes and a choice of refund method (cash, store credit, refund-payable for transfers) instead of a bare `insufficient_funds`.
9. Sweeper: one transaction per order, global product-id lock order, logging + metrics, retry on deadlock, poison-order skip with alert; partial index on pending holds.
10. Migrations under `pg_advisory_lock`; seed under the same lock.

### Phase 2 — Security & operability
1. Login: per-IP and per-account rate limit, temporary lockout, constant-time dummy hash for unknown emails, argon2 concurrency semaphore.
2. Tokens: `jti` + per-staff `token_version` for logout/revocation, shorter access tokens + refresh tokens, password change endpoint.
3. Machine access: hashed, scoped API keys per outlet/integration.
4. Authorization fixes: managers' price/cost changes restricted (admin, or outlet-level price overrides); customer PII masked for cashiers; customer search instead of full list; phone normalisation to E.164.
5. Production guard: refuse default secrets and `TILL_SEED` when `TILL_ENV=production`; per-merchant Paystack secret.
6. HTTP: request ID middleware, structured access + error logs, JSON 404/405/413/503, `MaxBytesReader` everywhere, `DisallowUnknownFields` everywhere, CORS allowlist, `Cache-Control: no-store`.
7. Ops: `/livez` and `/readyz` (DB + sweeper heartbeat), Prometheus metrics, graceful shutdown waits for the sweeper, Dockerfile built in CI (even though this laptop can't run Docker), backup/PITR runbook.
8. Pagination: cursor-based on orders, journals, audit, statements; filters for status, reference, customer, date range.

### Phase 3 — Fit the shop (product features)
1. Shifts / till sessions with opening float, cash-up, variance account, Z-report per cashier.
2. Split tender and partial/line refunds (discount apportioned to lines).
3. Outbound signed webhooks + `/v1/events` cursor feed (`order.paid`, `order.expired`, `payment.unmatched`, `stock.low`, `shift.closed`).
4. Discount limits and reasons by role; void/cancel pending transfer.
5. Per-product tax class (standard, exempt, zero-rated); merchant VAT-exempt mode; input VAT on receipts; VAT summary export.
6. Inter-outlet stock transfers and stock counts with write-off reasons.
7. Daily report that shows refunds on the refund day and per-cashier variance.

### Phase 4 — Go to market (product/business, outside this repo's core)
- Provider-agnostic confirmation (Monnify / Flutterwave virtual accounts, Mono bank feeds).
- Reference till PWA with offline queue; owner's daily WhatsApp report.
- TypeScript SDK generated from OpenAPI; sandbox mode; docs site.
- MBS e-invoicing export; NDPA export/erasure; legal opinion on the wallet.
- Pilot with 3 multi-outlet merchants; measure time-to-webhook, uptime, variance found, and whether transfers actually route through the confirmed account.

### Definition of done for every phase
- `go vet`, `gofmt`, `staticcheck` clean; unit + integration tests pass with `TILL_REQUIRE_DB=1` and `-race`.
- A regression test for every fixed finding in §3.
- OpenAPI updated and README updated in the same change.
- CI green on the branch.
