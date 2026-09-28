ALTER TABLE bids DROP CONSTRAINT bids_user_id_fk;
ALTER TABLE bids DROP CONSTRAINT bids_lot_id_fk;
ALTER TABLE lots DROP CONSTRAINT lots_winning_bid_fk;
ALTER TABLE bids DROP CONSTRAINT bids_lot_id_id_key;
DROP INDEX bids_lot_history_idx;
DROP INDEX bids_lot_amount_idx;
DROP TABLE bids;
