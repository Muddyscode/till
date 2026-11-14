-- Provider-neutral payments.
--
-- A payment connection is one place a merchant receives non-cash money: a
-- Paystack account, a Moniepoint terminal, a bank account watched by a feed.
-- Each has its own secret, its own webhook URL and its own clearing account,
-- so two providers reconcile separately and one merchant's secret never
-- verifies another merchant's money.

CREATE TABLE payment_connections (
    id                   UUID PRIMARY KEY,
    merchant_id          UUID NOT NULL REFERENCES merchants (id),
    -- Money with no order reference is looked for at this outlet first.
    outlet_id            UUID REFERENCES outlets (id),
    code                 TEXT NOT NULL CHECK (code ~ '^[A-Z0-9]{2,8}$'),
    name                 TEXT NOT NULL,
    provider             TEXT NOT NULL,
    secret               TEXT NOT NULL,
    -- The account customers pay into, when it is a fixed one. Shown to staff.
    account_number       TEXT NOT NULL DEFAULT '',
    clearing_account_id  UUID NOT NULL REFERENCES accounts (id),
    active               BOOLEAN NOT NULL DEFAULT true,
    created_by           UUID REFERENCES staff (id),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, code)
);

-- Money a provider has paid out to the merchant's bank.
CREATE TABLE payment_settlements (
    id             UUID PRIMARY KEY,
    merchant_id    UUID NOT NULL REFERENCES merchants (id),
    connection_id  UUID NOT NULL REFERENCES payment_connections (id),
    amount_minor   BIGINT NOT NULL CHECK (amount_minor > 0),
    reference      TEXT NOT NULL DEFAULT '',
    journal_id     UUID NOT NULL REFERENCES journals (id),
    staff_id       UUID NOT NULL REFERENCES staff (id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX payment_settlements_connection_idx ON payment_settlements (connection_id, created_at DESC);

-- Event ids are only unique inside one provider account, so dedupe is per
-- connection. Rows written before connections existed keep their provider name.
ALTER TABLE provider_events
    ADD COLUMN connection_id UUID REFERENCES payment_connections (id),
    ADD COLUMN details JSONB;
ALTER TABLE provider_events DROP CONSTRAINT provider_events_pkey;
CREATE UNIQUE INDEX provider_events_source_idx
    ON provider_events ((COALESCE(connection_id::text, provider)), event_id);

ALTER TABLE unmatched_payments
    ADD COLUMN connection_id UUID REFERENCES payment_connections (id),
    -- Who paid and how, as the provider reported it: what a manager needs to
    -- recognise the payment.
    ADD COLUMN details JSONB;
ALTER TABLE unmatched_payments DROP CONSTRAINT unmatched_payments_provider_event_id_key;
CREATE UNIQUE INDEX unmatched_payments_source_idx
    ON unmatched_payments ((COALESCE(connection_id::text, provider)), event_id);

ALTER TABLE payments ADD COLUMN connection_id UUID REFERENCES payment_connections (id);
DROP INDEX payments_provider_event_idx;
CREATE UNIQUE INDEX payments_provider_event_idx
    ON payments ((COALESCE(connection_id::text, provider)), provider_event_id)
    WHERE provider_event_id IS NOT NULL;
CREATE INDEX payments_pending_amount_idx ON payments (amount_minor) WHERE status = 'pending';

-- Any method that is confirmed by a payment event behaves like a transfer.
ALTER TABLE payments DROP CONSTRAINT payments_method_check;
ALTER TABLE payments ADD CONSTRAINT payments_method_check
    CHECK (method IN ('cash', 'wallet', 'transfer', 'card', 'tap', 'ussd', 'qr', 'mobile_money'));

ALTER TABLE orders DROP CONSTRAINT orders_payment_method_check;
ALTER TABLE orders ADD CONSTRAINT orders_payment_method_check
    CHECK (payment_method IN ('cash', 'wallet', 'transfer', 'card', 'tap', 'ussd', 'qr', 'mobile_money', 'split'));

ALTER TABLE orders DROP CONSTRAINT orders_wallet_customer_check;
ALTER TABLE orders ADD CONSTRAINT orders_wallet_customer_check CHECK (
    (payment_method = 'wallet' AND customer_id IS NOT NULL)
    OR payment_method = 'split'
    OR (payment_method NOT IN ('wallet', 'split') AND customer_id IS NULL)
);


CREATE INDEX stock_levels_product_idx ON stock_levels (product_id);
