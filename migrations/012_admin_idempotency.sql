-- Dashboard users can manage players (grants, claims, quest changes...). Their
-- idempotency keys get their own namespace so they never collide with a player's or the
-- game server's keys for the same player.
ALTER TABLE idempotency_records DROP CONSTRAINT IF EXISTS idempotency_records_actor_kind_check;
ALTER TABLE idempotency_records ADD CONSTRAINT idempotency_records_actor_kind_check CHECK (actor_kind IN ('player','server','admin'));
