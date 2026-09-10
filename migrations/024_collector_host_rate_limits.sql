-- Coordinate collection request starts across sla-core and collector processes.
-- The row count is bounded by configured/imported upstream hosts; no cleanup worker is needed in P1.

CREATE TABLE collector_host_rate_limits (
  host            TEXT PRIMARY KEY CHECK (host <> ''),
  next_allowed_at TIMESTAMPTZ NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
