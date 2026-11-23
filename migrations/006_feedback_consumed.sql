-- Unread feedback stays available to the curator until a proposal consumes it.
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS consumed_at TIMESTAMPTZ;
