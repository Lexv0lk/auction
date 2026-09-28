-- Accepted bids are immutable history. The request key is unique per
-- participant and lot, so a retry of the same attempt returns the same bid.
CREATE TABLE bids (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lot_id      BIGINT      NOT NULL,
    user_id     BIGINT      NOT NULL,
    amount      BIGINT      NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL,
    request_key UUID        NOT NULL,
    CONSTRAINT bids_amount_positive CHECK (amount > 0),
    CONSTRAINT bids_request_key_unique UNIQUE (user_id, lot_id, request_key)
);

CREATE INDEX bids_lot_amount_idx ON bids (lot_id, amount DESC, id DESC);
CREATE INDEX bids_lot_history_idx ON bids (lot_id, accepted_at DESC);

-- The winning bid must belong to the same lot; a missing winner stays NULL.
ALTER TABLE bids
    ADD CONSTRAINT bids_lot_id_id_key UNIQUE (lot_id, id);

ALTER TABLE lots
    ADD CONSTRAINT lots_winning_bid_fk
    FOREIGN KEY (id, winning_bid_id) REFERENCES bids (lot_id, id) ON DELETE RESTRICT;

-- A lot with bids is never deleted together with its history.
ALTER TABLE bids
    ADD CONSTRAINT bids_lot_id_fk FOREIGN KEY (lot_id) REFERENCES lots (id) ON DELETE RESTRICT;

ALTER TABLE bids
    ADD CONSTRAINT bids_user_id_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT;
