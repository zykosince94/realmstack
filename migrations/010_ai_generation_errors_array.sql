-- ai_generations.errors is always a JSON array (empty for a valid proposal). A Go nil
-- slice once stored JSON null for valid proposals, which the dashboard could not read.
-- Repair those rows, then let the schema refuse anything that is not an array.
UPDATE ai_generations SET errors = '[]'::jsonb WHERE jsonb_typeof(errors) <> 'array';
ALTER TABLE ai_generations DROP CONSTRAINT IF EXISTS ai_generations_errors_array;
ALTER TABLE ai_generations ADD CONSTRAINT ai_generations_errors_array CHECK (jsonb_typeof(errors) = 'array');
