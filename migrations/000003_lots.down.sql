DROP INDEX lots_category_id_idx;
DROP INDEX lots_active_ends_at_idx;
ALTER TABLE lots DROP CONSTRAINT lots_category_id_fk;
DROP TABLE lots;
