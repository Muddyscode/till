-- Phase 2: token revocation, login lockout, machine API keys.
-- Independent of 0004 so the two phases can merge in either order.

ALTER TABLE staff
    ADD COLUMN token_version        INT NOT NULL DEFAULT 0,
    ADD COLUMN failed_login_count   INT NOT NULL DEFAULT 0,
    ADD COLUMN last_failed_login_at TIMESTAMPTZ,
    ADD COLUMN locked_until         TIMESTAMPTZ,
    ADD COLUMN is_service           BOOLEAN NOT NULL DEFAULT false;

-- Each API key is backed by a non-login service staff row (is_service) so orders,
-- stock moves and audit rows keep their staff_id foreign keys. The staff row carries
-- the role and staff_outlets of the key, so authorizeOutlet works unchanged.
CREATE TABLE api_keys (
    id            UUID PRIMARY KEY,
    merchant_id   UUID NOT NULL REFERENCES merchants (id),
    staff_id      UUID NOT NULL UNIQUE REFERENCES staff (id),
    name          TEXT NOT NULL,
    prefix        TEXT NOT NULL UNIQUE,
    hash          BYTEA NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('cashier', 'manager')),
    outlet_ids    UUID[] NOT NULL,
    created_by    UUID NOT NULL REFERENCES staff (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX api_keys_merchant_idx ON api_keys (merchant_id, created_at DESC);
