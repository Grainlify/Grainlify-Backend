CREATE TABLE processed_deliveries (
    delivery_id VARCHAR(100) PRIMARY KEY,
    processed_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_processed_deliveries_processed_at ON processed_deliveries (processed_at);
