-- Auction lots: draft conditions are complete from creation; the deadline
-- relative to current time is re-checked by the publication operation, not here.
CREATE TABLE lots (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title          TEXT        NOT NULL,
    description    TEXT        NOT NULL,
    category_id    BIGINT      NOT NULL,
    start_price    BIGINT      NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'draft',
    ends_at        TIMESTAMPTZ NOT NULL,
    finished_at    TIMESTAMPTZ,
    winning_bid_id BIGINT,
    CONSTRAINT lots_title_not_empty CHECK (title <> ''),
    CONSTRAINT lots_title_length CHECK (char_length(title) <= 200),
    CONSTRAINT lots_description_not_empty CHECK (description <> ''),
    CONSTRAINT lots_description_length CHECK (char_length(description) <= 5000),
    CONSTRAINT lots_start_price_positive CHECK (start_price > 0),
    CONSTRAINT lots_status_check CHECK (status IN ('draft', 'active', 'finished')),
    CONSTRAINT lots_result_state_check CHECK (
        (status IN ('draft', 'active') AND finished_at IS NULL AND winning_bid_id IS NULL)
        OR (status = 'finished' AND finished_at IS NOT NULL)
    )
);

-- Used categories are never deleted together with their lots.
ALTER TABLE lots
    ADD CONSTRAINT lots_category_id_fk
    FOREIGN KEY (category_id) REFERENCES categories (id) ON DELETE RESTRICT;

-- Worker and catalog scan due active lots by (ends_at, id).
CREATE INDEX lots_active_ends_at_idx ON lots (ends_at, id) WHERE status = 'active';
CREATE INDEX lots_category_id_idx ON lots (category_id);
