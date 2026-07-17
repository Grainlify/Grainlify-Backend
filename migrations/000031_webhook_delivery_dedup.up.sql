CREATE TABLE IF NOT EXISTS webhook_delivery_dedup (
    delivery_id TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_webhook_dedup_created_at ON webhook_delivery_dedup (created_at);
