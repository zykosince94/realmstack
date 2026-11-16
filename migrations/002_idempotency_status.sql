-- Store the original HTTP status so an idempotent replay returns it unchanged
-- (for example 201 for score submissions instead of 200).
ALTER TABLE idempotency_records ADD COLUMN IF NOT EXISTS status_code integer NOT NULL DEFAULT 200;
