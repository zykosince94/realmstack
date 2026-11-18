-- OpenAI-compatible providers: any service that implements the Chat Completions API
-- (OpenRouter, Together, Groq, vLLM, Ollama, ...) at a base URL the user chooses.
-- base_url is empty for OpenAI, which has no configurable endpoint. A stored key is only
-- ever sent to the provider and base_url it was saved with (enforced in the API).
ALTER TABLE ai_provider_settings ADD COLUMN IF NOT EXISTS base_url text NOT NULL DEFAULT '';
ALTER TABLE ai_provider_settings DROP CONSTRAINT IF EXISTS ai_provider_settings_provider_check;
ALTER TABLE ai_provider_settings DROP CONSTRAINT IF EXISTS ai_provider_settings_provider_base_url_check;
ALTER TABLE ai_provider_settings ADD CONSTRAINT ai_provider_settings_provider_base_url_check CHECK (
  (provider = 'openai' AND base_url = '') OR (provider = 'openai_compatible' AND base_url <> ''));
