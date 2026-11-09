-- Phase 3B shop operations: shifts, event feed + webhooks, stock transfers,
-- stock counts, suppliers. (0006 belongs to Phase 3A; files apply in sorted order.)

-- A monotonic sequence on the audit log is the cursor for the event feed and
-- the webhook worker. Existing rows are numbered when the column is added.
ALTER TABLE audit_events ADD COLUMN seq BIGINT GENERATED ALWAYS AS IDENTITY;
CREATE UNIQUE INDEX audit_events_seq_idx ON audit_events (seq);
CREATE INDEX audit_events_merchant_seq_idx ON audit_events (merchant_id, seq);

CREATE TABLE shifts (
    id                    UUID PRIMARY KEY,
    merchant_id           UUID NOT NULL REFERENCES merchants (id),
    outlet_id             UUID NOT NULL REFERENCES outlets (id),
    staff_id              UUID NOT NULL REFERENCES staff (id),
    status                TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    opening_float_minor   BIGINT NOT NULL CHECK (opening_float_minor >= 0),
    opened_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at             TIMESTAMPTZ,
    closed_by             UUID REFERENCES staff (id),
    expected_cash_minor   BIGINT,
    counted_cash_minor    BIGINT CHECK (counted_cash_minor IS NULL OR counted_cash_minor >= 0),
    variance_minor        BIGINT,
    variance_journal_id   UUID REFERENCES journals (id),
    note                  TEXT NOT NULL DEFAULT '',
    report                JSONB
);
CREATE UNIQUE INDEX shifts_one_open_idx ON shifts (staff_id, outlet_id) WHERE status = 'open';
CREATE INDEX shifts_merchant_opened_idx ON shifts (merchant_id, opened_at DESC);

CREATE TABLE webhook_endpoints (
    id           UUID PRIMARY KEY,
    merchant_id  UUID NOT NULL REFERENCES merchants (id),
    url          TEXT NOT NULL,
    event_types  TEXT[] NOT NULL DEFAULT '{}',
    secret       TEXT NOT NULL,
    created_by   UUID REFERENCES staff (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);
CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id) WHERE deleted_at IS NULL;

CREATE TABLE event_cursors (
    name  TEXT PRIMARY KEY,
    seq   BIGINT NOT NULL
);
-- Only events recorded from now on are delivered.
INSERT INTO event_cursors (name, seq) SELECT 'webhooks', COALESCE(MAX(seq), 0) FROM audit_events;

CREATE TABLE webhook_deliveries (
    id               UUID PRIMARY KEY,
    endpoint_id      UUID NOT NULL REFERENCES webhook_endpoints (id),
    event_id         UUID NOT NULL,
    event_type       TEXT NOT NULL,
    payload          JSONB NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts         INT NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status      INT,
    last_error       TEXT,
    delivered_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (endpoint_id, event_id)
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id, created_at DESC);

CREATE TABLE stock_transfers (
    id              UUID PRIMARY KEY,
    merchant_id     UUID NOT NULL REFERENCES merchants (id),
    from_outlet_id  UUID NOT NULL REFERENCES outlets (id),
    to_outlet_id    UUID NOT NULL REFERENCES outlets (id),
    status          TEXT NOT NULL CHECK (status IN ('in_transit', 'received', 'cancelled')),
    note            TEXT NOT NULL DEFAULT '',
    created_by      UUID NOT NULL REFERENCES staff (id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    received_by     UUID REFERENCES staff (id),
    received_at     TIMESTAMPTZ,
    cancelled_by    UUID REFERENCES staff (id),
    cancelled_at    TIMESTAMPTZ,
    CHECK (from_outlet_id <> to_outlet_id)
);
CREATE INDEX stock_transfers_merchant_idx ON stock_transfers (merchant_id, created_at DESC);

CREATE TABLE stock_transfer_lines (
    transfer_id  UUID NOT NULL REFERENCES stock_transfers (id),
    product_id   UUID NOT NULL REFERENCES products (id),
    sku          TEXT NOT NULL,
    quantity     INT NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (transfer_id, product_id)
);

CREATE TABLE stock_counts (
    id           UUID PRIMARY KEY,
    merchant_id  UUID NOT NULL REFERENCES merchants (id),
    outlet_id    UUID NOT NULL REFERENCES outlets (id),
    note         TEXT NOT NULL DEFAULT '',
    created_by   UUID NOT NULL REFERENCES staff (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE stock_count_lines (
    count_id            UUID NOT NULL REFERENCES stock_counts (id),
    product_id          UUID NOT NULL REFERENCES products (id),
    sku                 TEXT NOT NULL,
    expected_on_hand    INT NOT NULL,
    counted             INT NOT NULL CHECK (counted >= 0),
    variance            INT NOT NULL,
    unit_cost_minor     BIGINT NOT NULL,
    variance_value_minor BIGINT NOT NULL,
    PRIMARY KEY (count_id, product_id)
);

CREATE TABLE suppliers (
    id           UUID PRIMARY KEY,
    merchant_id  UUID NOT NULL REFERENCES merchants (id),
    name         TEXT NOT NULL,
    phone        TEXT NOT NULL DEFAULT '',
    note         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);
CREATE INDEX suppliers_merchant_idx ON suppliers (merchant_id) WHERE deleted_at IS NULL;

-- Stock receipts keep no row of their own (they are a journal plus an audit
-- event), so a receipt that names a supplier is recorded here.
CREATE TABLE supplier_receipts (
    id            UUID PRIMARY KEY,
    merchant_id   UUID NOT NULL REFERENCES merchants (id),
    supplier_id   UUID NOT NULL REFERENCES suppliers (id),
    outlet_id     UUID NOT NULL REFERENCES outlets (id),
    product_id    UUID NOT NULL REFERENCES products (id),
    sku           TEXT NOT NULL,
    qty           INT NOT NULL CHECK (qty > 0),
    amount_minor  BIGINT NOT NULL CHECK (amount_minor >= 0),
    staff_id      UUID NOT NULL REFERENCES staff (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX supplier_receipts_supplier_idx ON supplier_receipts (supplier_id, created_at);

CREATE TABLE supplier_payments (
    id            UUID PRIMARY KEY,
    merchant_id   UUID NOT NULL REFERENCES merchants (id),
    supplier_id   UUID NOT NULL REFERENCES suppliers (id),
    outlet_id     UUID NOT NULL REFERENCES outlets (id),
    amount_minor  BIGINT NOT NULL CHECK (amount_minor > 0),
    method        TEXT NOT NULL CHECK (method IN ('cash', 'bank')),
    note          TEXT NOT NULL DEFAULT '',
    journal_id    UUID NOT NULL REFERENCES journals (id),
    staff_id      UUID NOT NULL REFERENCES staff (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX supplier_payments_supplier_idx ON supplier_payments (supplier_id, created_at);
