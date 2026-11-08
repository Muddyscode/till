-- Store credit, average cost, and order discounts.
-- Wallet balance is a liability of this merchant. It is not a licensed e-money wallet.

ALTER TABLE products
    ADD COLUMN avg_cost_minor BIGINT NOT NULL DEFAULT 0 CHECK (avg_cost_minor >= 0);

ALTER TABLE orders
    ADD COLUMN discount_minor BIGINT NOT NULL DEFAULT 0 CHECK (discount_minor >= 0),
    ADD COLUMN cost_minor BIGINT NOT NULL DEFAULT 0 CHECK (cost_minor >= 0),
    ADD COLUMN customer_id UUID;

ALTER TABLE orders DROP CONSTRAINT orders_payment_method_check;
ALTER TABLE orders ADD CONSTRAINT orders_payment_method_check
    CHECK (payment_method IN ('cash', 'transfer', 'wallet'));

ALTER TABLE payments DROP CONSTRAINT payments_method_check;
ALTER TABLE payments ADD CONSTRAINT payments_method_check
    CHECK (method IN ('cash', 'transfer', 'wallet'));

CREATE TABLE customers (
    id             UUID PRIMARY KEY,
    merchant_id    UUID NOT NULL REFERENCES merchants (id),
    name           TEXT NOT NULL,
    phone          TEXT NOT NULL,
    balance_minor  BIGINT NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, phone)
);

CREATE INDEX customers_merchant_idx ON customers (merchant_id);

ALTER TABLE orders
    ADD CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id) REFERENCES customers (id),
    ADD CONSTRAINT orders_wallet_customer_check CHECK (
        (payment_method = 'wallet' AND customer_id IS NOT NULL)
        OR (payment_method <> 'wallet' AND customer_id IS NULL)
    );

ALTER TABLE merchants
    ADD COLUMN inventory_account_id UUID REFERENCES accounts (id),
    ADD COLUMN cogs_account_id UUID REFERENCES accounts (id),
    ADD COLUMN wallet_account_id UUID REFERENCES accounts (id),
    ADD COLUMN goods_received_account_id UUID REFERENCES accounts (id),
    ADD COLUMN adjustment_account_id UUID REFERENCES accounts (id);
