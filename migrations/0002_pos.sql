-- POS: merchants, staff, outlets, stock, orders. Ledger tables stay append-only.

DROP VIEW account_balances_from_journal;

ALTER TABLE operators RENAME TO merchants;
ALTER TABLE merchants DROP COLUMN email;
ALTER TABLE merchants DROP COLUMN password_hash;
ALTER TABLE merchants ADD COLUMN name TEXT NOT NULL DEFAULT 'Merchant';
ALTER TABLE merchants ALTER COLUMN name DROP DEFAULT;
ALTER TABLE merchants ADD COLUMN vat_inclusive_bps INT NOT NULL DEFAULT 0
    CHECK (vat_inclusive_bps BETWEEN 0 AND 5000);
ALTER TABLE merchants ADD COLUMN clearing_account_id UUID;
ALTER TABLE merchants ADD COLUMN vat_account_id UUID;
ALTER TABLE merchants ADD COLUMN equity_account_id UUID;

ALTER TABLE accounts RENAME COLUMN operator_id TO merchant_id;
ALTER TABLE journals RENAME COLUMN operator_id TO merchant_id;
ALTER TABLE idempotency_keys RENAME COLUMN operator_id TO merchant_id;
ALTER TABLE accounts ADD COLUMN outlet_id UUID;

ALTER TABLE merchants
    ADD CONSTRAINT merchants_clearing_fk FOREIGN KEY (clearing_account_id) REFERENCES accounts (id),
    ADD CONSTRAINT merchants_vat_fk FOREIGN KEY (vat_account_id) REFERENCES accounts (id),
    ADD CONSTRAINT merchants_equity_fk FOREIGN KEY (equity_account_id) REFERENCES accounts (id);

CREATE VIEW account_balances_from_journal AS
SELECT
    a.id,
    a.merchant_id,
    COALESCE(SUM(
        CASE
            WHEN a.type IN ('asset', 'expense') THEN
                CASE WHEN p.direction = 'debit' THEN p.amount_minor ELSE -p.amount_minor END
            ELSE
                CASE WHEN p.direction = 'credit' THEN p.amount_minor ELSE -p.amount_minor END
        END
    ), 0) AS balance_minor
FROM accounts a
LEFT JOIN postings p ON p.account_id = a.id
GROUP BY a.id, a.merchant_id, a.type;

CREATE TABLE staff (
    id              UUID PRIMARY KEY,
    merchant_id     UUID NOT NULL REFERENCES merchants (id),
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('admin', 'manager', 'cashier')),
    disabled_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX staff_merchant_idx ON staff (merchant_id);

CREATE TABLE outlets (
    id                UUID PRIMARY KEY,
    merchant_id       UUID NOT NULL REFERENCES merchants (id),
    name              TEXT NOT NULL,
    code              TEXT NOT NULL,
    cash_account_id   UUID NOT NULL REFERENCES accounts (id),
    sales_account_id  UUID NOT NULL REFERENCES accounts (id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, code)
);

CREATE INDEX outlets_merchant_idx ON outlets (merchant_id);

ALTER TABLE accounts
    ADD CONSTRAINT accounts_outlet_fk FOREIGN KEY (outlet_id) REFERENCES outlets (id);

CREATE TABLE staff_outlets (
    staff_id   UUID NOT NULL REFERENCES staff (id),
    outlet_id  UUID NOT NULL REFERENCES outlets (id),
    PRIMARY KEY (staff_id, outlet_id)
);

CREATE TABLE products (
    id              UUID PRIMARY KEY,
    merchant_id     UUID NOT NULL REFERENCES merchants (id),
    sku             TEXT NOT NULL,
    name            TEXT NOT NULL,
    price_minor     BIGINT NOT NULL CHECK (price_minor > 0),
    reorder_level   INT NOT NULL DEFAULT 0 CHECK (reorder_level >= 0),
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, sku)
);

CREATE TABLE stock_levels (
    outlet_id   UUID NOT NULL REFERENCES outlets (id),
    product_id  UUID NOT NULL REFERENCES products (id),
    on_hand     INT NOT NULL DEFAULT 0,
    reserved    INT NOT NULL DEFAULT 0,
    PRIMARY KEY (outlet_id, product_id),
    CHECK (on_hand >= 0),
    CHECK (reserved >= 0),
    CHECK (reserved <= on_hand)
);

CREATE TABLE outlet_counters (
    outlet_id  UUID PRIMARY KEY REFERENCES outlets (id),
    n          INT NOT NULL DEFAULT 0 CHECK (n >= 0)
);

CREATE TABLE orders (
    id                UUID PRIMARY KEY,
    merchant_id       UUID NOT NULL REFERENCES merchants (id),
    outlet_id         UUID NOT NULL REFERENCES outlets (id),
    staff_id          UUID NOT NULL REFERENCES staff (id),
    status            TEXT NOT NULL CHECK (status IN ('pending_payment', 'paid', 'expired', 'refunded')),
    payment_method    TEXT NOT NULL CHECK (payment_method IN ('cash', 'transfer')),
    currency          CHAR(3) NOT NULL DEFAULT 'NGN' CHECK (currency = 'NGN'),
    total_minor       BIGINT NOT NULL CHECK (total_minor > 0),
    vat_minor         BIGINT NOT NULL CHECK (vat_minor >= 0),
    net_minor         BIGINT NOT NULL CHECK (net_minor > 0),
    reference         TEXT NOT NULL UNIQUE,
    idempotency_key   TEXT,
    journal_id        UUID REFERENCES journals (id),
    expires_at        TIMESTAMPTZ,
    paid_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (vat_minor + net_minor = total_minor)
);

CREATE UNIQUE INDEX orders_idempotency_idx ON orders (merchant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX orders_outlet_paid_idx ON orders (outlet_id, paid_at DESC);
CREATE INDEX orders_merchant_created_idx ON orders (merchant_id, created_at DESC);

CREATE TABLE order_lines (
    id                 UUID PRIMARY KEY,
    order_id           UUID NOT NULL REFERENCES orders (id),
    product_id         UUID NOT NULL REFERENCES products (id),
    sku                TEXT NOT NULL,
    qty                INT NOT NULL CHECK (qty > 0),
    unit_price_minor   BIGINT NOT NULL CHECK (unit_price_minor > 0),
    line_total_minor   BIGINT NOT NULL CHECK (line_total_minor = qty::bigint * unit_price_minor)
);

CREATE INDEX order_lines_order_idx ON order_lines (order_id);

CREATE TABLE stock_reservations (
    id           UUID PRIMARY KEY,
    merchant_id  UUID NOT NULL REFERENCES merchants (id),
    outlet_id    UUID NOT NULL REFERENCES outlets (id),
    order_id     UUID NOT NULL REFERENCES orders (id),
    product_id   UUID NOT NULL REFERENCES products (id),
    qty          INT NOT NULL CHECK (qty > 0),
    status       TEXT NOT NULL CHECK (status IN ('active', 'committed', 'released')),
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX stock_reservations_order_idx ON stock_reservations (order_id);

CREATE TABLE payments (
    id                 UUID PRIMARY KEY,
    order_id           UUID NOT NULL REFERENCES orders (id),
    method             TEXT NOT NULL CHECK (method IN ('cash', 'transfer')),
    status             TEXT NOT NULL CHECK (status IN ('pending', 'confirmed', 'failed')),
    amount_minor       BIGINT NOT NULL CHECK (amount_minor > 0),
    provider           TEXT NOT NULL DEFAULT '',
    provider_event_id  TEXT,
    journal_id         UUID REFERENCES journals (id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX payments_provider_event_idx ON payments (provider, provider_event_id)
    WHERE provider_event_id IS NOT NULL;

CREATE TABLE audit_events (
    id            UUID PRIMARY KEY,
    merchant_id   UUID REFERENCES merchants (id),
    staff_id      UUID REFERENCES staff (id),
    action        TEXT NOT NULL,
    entity_type   TEXT NOT NULL,
    entity_id     UUID NOT NULL,
    outlet_id     UUID REFERENCES outlets (id),
    metadata      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_merchant_created_idx ON audit_events (merchant_id, created_at DESC);

CREATE FUNCTION forbid_audit_mutation() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'audit events are immutable';
END;
$$;

CREATE TRIGGER audit_events_immutable
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW
    EXECUTE FUNCTION forbid_audit_mutation();
