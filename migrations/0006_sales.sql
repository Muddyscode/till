-- Phase 3A sales features: split tender, partial refunds, discount controls,
-- void, tax classes. Independent of 0007 so the two can merge in either order.

-- Merchant settings.
ALTER TABLE merchants
    ADD COLUMN vat_registered BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN max_discount_bps_cashier INT NOT NULL DEFAULT 1000
        CHECK (max_discount_bps_cashier BETWEEN 0 AND 10000),
    ADD COLUMN max_discount_bps_manager INT NOT NULL DEFAULT 3000
        CHECK (max_discount_bps_manager BETWEEN 0 AND 10000);

-- Tax class per product.
ALTER TABLE products
    ADD COLUMN tax_class TEXT NOT NULL DEFAULT 'standard'
        CHECK (tax_class IN ('standard', 'exempt', 'zero'));

-- Orders: new statuses, split payment method, discount reason, running refund total.
ALTER TABLE orders
    ADD COLUMN discount_reason TEXT,
    ADD COLUMN refunded_minor BIGINT NOT NULL DEFAULT 0 CHECK (refunded_minor >= 0);

ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending_payment', 'paid', 'expired', 'refunded', 'partially_refunded', 'voided'));

ALTER TABLE orders DROP CONSTRAINT orders_payment_method_check;
ALTER TABLE orders ADD CONSTRAINT orders_payment_method_check
    CHECK (payment_method IN ('cash', 'transfer', 'wallet', 'split'));

-- A split order carries the wallet tender's customer, if it has one.
ALTER TABLE orders DROP CONSTRAINT orders_wallet_customer_check;
ALTER TABLE orders ADD CONSTRAINT orders_wallet_customer_check CHECK (
    (payment_method = 'wallet' AND customer_id IS NOT NULL)
    OR (payment_method IN ('cash', 'transfer') AND customer_id IS NULL)
    OR payment_method = 'split'
);

UPDATE orders SET refunded_minor = total_minor WHERE status = 'refunded';

-- Per-line money. Order discount, VAT and cost are apportioned to lines
-- (floor of the weighted share, remainder kobo to the last line).
ALTER TABLE order_lines
    ADD COLUMN tax_class TEXT NOT NULL DEFAULT 'standard'
        CHECK (tax_class IN ('standard', 'exempt', 'zero')),
    ADD COLUMN discount_minor BIGINT NOT NULL DEFAULT 0 CHECK (discount_minor >= 0),
    ADD COLUMN vat_minor BIGINT NOT NULL DEFAULT 0 CHECK (vat_minor >= 0),
    ADD COLUMN net_minor BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN cost_minor BIGINT NOT NULL DEFAULT 0 CHECK (cost_minor >= 0),
    ADD COLUMN refunded_qty INT NOT NULL DEFAULT 0;

ALTER TABLE order_lines ADD CONSTRAINT order_lines_refunded_qty_check
    CHECK (refunded_qty >= 0 AND refunded_qty <= qty);

-- Backfill 1: discount by line total.
WITH base AS (
    SELECT l.id, l.order_id, l.line_total_minor AS lt, o.discount_minor AS d,
           ROW_NUMBER() OVER (PARTITION BY l.order_id ORDER BY l.id) AS rn,
           COUNT(*) OVER (PARTITION BY l.order_id) AS cnt,
           SUM(l.line_total_minor) OVER (PARTITION BY l.order_id) AS shelf
    FROM order_lines l JOIN orders o ON o.id = l.order_id
), fl AS (
    SELECT *, FLOOR(d::numeric * lt / shelf)::bigint AS f FROM base
), share AS (
    SELECT id, CASE WHEN rn = cnt THEN d - (SUM(f) OVER (PARTITION BY order_id) - f) ELSE f END AS v FROM fl
)
UPDATE order_lines ol SET discount_minor = share.v FROM share WHERE ol.id = share.id;

-- Backfill 2: VAT by payable (line total less its discount).
WITH base AS (
    SELECT l.id, l.order_id, (l.line_total_minor - l.discount_minor) AS pay, o.vat_minor AS v,
           ROW_NUMBER() OVER (PARTITION BY l.order_id ORDER BY l.id) AS rn,
           COUNT(*) OVER (PARTITION BY l.order_id) AS cnt,
           SUM(l.line_total_minor - l.discount_minor) OVER (PARTITION BY l.order_id) AS tot
    FROM order_lines l JOIN orders o ON o.id = l.order_id
), fl AS (
    SELECT *, CASE WHEN tot > 0 THEN FLOOR(v::numeric * pay / tot)::bigint ELSE 0 END AS f FROM base
), share AS (
    SELECT id, CASE WHEN rn = cnt THEN v - (SUM(f) OVER (PARTITION BY order_id) - f) ELSE f END AS s FROM fl
)
UPDATE order_lines ol SET vat_minor = share.s FROM share WHERE ol.id = share.id;

-- Backfill 3: cost of goods by line total (an approximation for history only).
WITH base AS (
    SELECT l.id, l.order_id, l.line_total_minor AS lt, o.cost_minor AS c,
           ROW_NUMBER() OVER (PARTITION BY l.order_id ORDER BY l.id) AS rn,
           COUNT(*) OVER (PARTITION BY l.order_id) AS cnt,
           SUM(l.line_total_minor) OVER (PARTITION BY l.order_id) AS shelf
    FROM order_lines l JOIN orders o ON o.id = l.order_id
), fl AS (
    SELECT *, FLOOR(c::numeric * lt / shelf)::bigint AS f FROM base
), share AS (
    SELECT id, CASE WHEN rn = cnt THEN c - (SUM(f) OVER (PARTITION BY order_id) - f) ELSE f END AS s FROM fl
)
UPDATE order_lines ol SET cost_minor = share.s FROM share WHERE ol.id = share.id;

UPDATE order_lines SET net_minor = line_total_minor - discount_minor - vat_minor;

ALTER TABLE order_lines ADD CONSTRAINT order_lines_split_check
    CHECK (net_minor + vat_minor + discount_minor = line_total_minor);

UPDATE order_lines ol SET refunded_qty = ol.qty
FROM orders o WHERE o.id = ol.order_id AND o.status = 'refunded';

-- Refunds. One row per refund, whole or partial.
CREATE TABLE order_refunds (
    id            UUID PRIMARY KEY,
    merchant_id   UUID NOT NULL REFERENCES merchants (id),
    order_id      UUID NOT NULL REFERENCES orders (id),
    outlet_id     UUID NOT NULL REFERENCES outlets (id),
    journal_id    UUID REFERENCES journals (id),
    amount_minor  BIGINT NOT NULL CHECK (amount_minor >= 0),
    net_minor     BIGINT NOT NULL CHECK (net_minor >= 0),
    vat_minor     BIGINT NOT NULL CHECK (vat_minor >= 0),
    cost_minor    BIGINT NOT NULL CHECK (cost_minor >= 0),
    lines         JSONB NOT NULL DEFAULT '[]',
    tenders       JSONB NOT NULL DEFAULT '{}',
    reason        TEXT,
    staff_id      UUID NOT NULL REFERENCES staff (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (net_minor + vat_minor = amount_minor)
);

CREATE INDEX order_refunds_order_idx ON order_refunds (order_id);
CREATE INDEX order_refunds_outlet_created_idx ON order_refunds (outlet_id, created_at);

-- Refunds made before this migration, so the refund-day report sees them.
INSERT INTO order_refunds (id, merchant_id, order_id, outlet_id, journal_id, amount_minor, net_minor, vat_minor,
                           cost_minor, lines, tenders, reason, staff_id, created_at)
SELECT gen_random_uuid(), o.merchant_id, o.id, o.outlet_id, o.refund_journal_id, o.total_minor, o.net_minor,
       o.vat_minor, o.cost_minor,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('product_id', l.product_id, 'sku', l.sku, 'quantity', l.qty))
                 FROM order_lines l WHERE l.order_id = o.id), '[]'::jsonb),
       jsonb_build_object(o.payment_method, o.total_minor),
       o.refund_reason, o.refunded_by, COALESCE(o.refunded_at, now())
FROM orders o
WHERE o.status = 'refunded' AND o.refund_journal_id IS NOT NULL AND o.refunded_by IS NOT NULL;

-- Payments: tenders held against a pending split order, and what was refunded per tender.
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check
    CHECK (status IN ('pending', 'confirmed', 'failed', 'held', 'returned'));

ALTER TABLE payments
    ADD COLUMN deposit_journal_id UUID REFERENCES journals (id),
    ADD COLUMN refunded_minor BIGINT NOT NULL DEFAULT 0 CHECK (refunded_minor >= 0);

DROP INDEX payments_one_confirmed_idx;
CREATE UNIQUE INDEX payments_one_confirmed_idx ON payments (order_id, method) WHERE status = 'confirmed';
