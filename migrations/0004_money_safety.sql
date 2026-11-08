-- Phase 1 money safety: provider events, payment suspense, refund links,
-- database-level journal guards and the missing indexes.

-- Webhook dedupe moves out of idempotency_keys so a client can never squat a
-- provider event id. One row per (provider, event id), kept forever.
CREATE TABLE provider_events (
    provider         TEXT NOT NULL,
    event_id         TEXT NOT NULL,
    merchant_id      UUID REFERENCES merchants (id),
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    outcome          TEXT NOT NULL,
    order_id         UUID REFERENCES orders (id),
    response_status  INT,
    response_body    JSONB,
    PRIMARY KEY (provider, event_id)
);

-- Events already handled under the old "wh:<id>" idempotency keys stay deduped.
INSERT INTO provider_events (provider, event_id, merchant_id, outcome, response_status, response_body)
SELECT 'paystack', substr(key, 4), merchant_id, 'migrated', http_status, response_body
FROM idempotency_keys
WHERE key LIKE 'wh:%' AND status = 'completed'
ON CONFLICT DO NOTHING;

-- Money that arrived but could not be applied, and refunds waiting for the
-- provider to pay out.
CREATE TABLE unmatched_payments (
    id                     UUID PRIMARY KEY,
    merchant_id            UUID NOT NULL REFERENCES merchants (id),
    order_id               UUID REFERENCES orders (id),
    outlet_id              UUID REFERENCES outlets (id),
    provider               TEXT NOT NULL,
    event_id               TEXT NOT NULL,
    amount_minor           BIGINT NOT NULL CHECK (amount_minor > 0),
    currency               TEXT NOT NULL,
    reason                 TEXT NOT NULL,
    status                 TEXT NOT NULL CHECK (status IN ('open', 'applied', 'refund_pending', 'refunded')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at            TIMESTAMPTZ,
    resolved_by            UUID REFERENCES staff (id),
    journal_id             UUID REFERENCES journals (id),
    resolution_journal_id  UUID REFERENCES journals (id),
    settlement_journal_id  UUID REFERENCES journals (id),
    UNIQUE (provider, event_id)
);

CREATE INDEX unmatched_payments_merchant_status_idx ON unmatched_payments (merchant_id, status, created_at DESC);
CREATE INDEX unmatched_payments_order_idx ON unmatched_payments (order_id) WHERE order_id IS NOT NULL;

ALTER TABLE merchants
    ADD COLUMN suspense_account_id UUID REFERENCES accounts (id),
    ADD COLUMN refunds_payable_account_id UUID REFERENCES accounts (id);

ALTER TABLE orders
    ADD COLUMN refund_journal_id UUID REFERENCES journals (id),
    ADD COLUMN refunded_at TIMESTAMPTZ,
    ADD COLUMN refunded_by UUID REFERENCES staff (id),
    ADD COLUMN refund_reason TEXT;

-- Every journal must balance when its transaction commits.
CREATE FUNCTION check_journal_balanced() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    d BIGINT;
    c BIGINT;
BEGIN
    SELECT COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'), 0),
           COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0)
      INTO d, c
      FROM postings WHERE journal_id = NEW.journal_id;
    IF d <> c THEN
        RAISE EXCEPTION 'journal % is unbalanced: debits % credits %', NEW.journal_id, d, c
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER postings_balanced
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION check_journal_balanced();

-- Postings may only be added in the transaction that created their journal.
ALTER TABLE journals ADD COLUMN created_txid xid8 NOT NULL DEFAULT pg_current_xact_id();

CREATE FUNCTION postings_same_tx_as_journal() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM journals WHERE id = NEW.journal_id AND created_txid = pg_current_xact_id()
    ) THEN
        RAISE EXCEPTION 'postings can only be added in the transaction that created journal %', NEW.journal_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER postings_same_tx
    BEFORE INSERT ON postings
    FOR EACH ROW
    EXECUTE FUNCTION postings_same_tx_as_journal();

-- Indexes.
CREATE INDEX orders_pending_expiry_idx ON orders (expires_at) WHERE status = 'pending_payment';
CREATE INDEX orders_journal_idx ON orders (journal_id) WHERE journal_id IS NOT NULL;
CREATE INDEX orders_customer_idx ON orders (customer_id) WHERE customer_id IS NOT NULL;
CREATE INDEX order_lines_product_idx ON order_lines (product_id);
CREATE INDEX payments_order_idx ON payments (order_id);
CREATE UNIQUE INDEX payments_one_confirmed_idx ON payments (order_id) WHERE status = 'confirmed';
CREATE INDEX postings_account_journal_idx ON postings (account_id, journal_id);
DROP INDEX postings_account_idx;
CREATE UNIQUE INDEX stock_reservations_order_product_idx ON stock_reservations (order_id, product_id);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);
