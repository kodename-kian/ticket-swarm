CREATE TABLE tiers (
    id          integer PRIMARY KEY,
    total_stock integer NOT NULL CHECK (total_stock >= 0),
    available   integer NOT NULL,
    name        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE holds (
    id              uuid PRIMARY KEY,
    tier_id         integer NOT NULL REFERENCES tiers(id),
    state           text NOT NULL CHECK (state IN ('active', 'converted', 'expired')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    idempotency_id  uuid NOT NULL,
    UNIQUE (tier_id, idempotency_id)
);

CREATE TABLE orders (
    id          uuid PRIMARY KEY,
    hold_id     uuid NOT NULL UNIQUE REFERENCES holds(id),
    tier_id     integer NOT NULL REFERENCES tiers(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX holds_committed_idx  ON holds(tier_id)      WHERE state IN ('active', 'converted');
CREATE INDEX holds_expiry_idx     ON holds(expires_at)   WHERE state = 'active';
