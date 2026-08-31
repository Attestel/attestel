CREATE TABLE bars (
  ticker      TEXT NOT NULL,
  timeframe   TEXT NOT NULL,
  ts          TIMESTAMPTZ NOT NULL,
  open        DOUBLE PRECISION NOT NULL,
  high        DOUBLE PRECISION NOT NULL,
  low         DOUBLE PRECISION NOT NULL,
  close       DOUBLE PRECISION NOT NULL,
  volume      DOUBLE PRECISION NOT NULL,
  source      TEXT NOT NULL,
  ingested_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (ticker, timeframe, ts),
  CHECK (timeframe <> '1W'),
  CHECK (source IN ('alpaca','twelvedata','yfinance','tiingo'))
);

CREATE INDEX idx_bars_lookup ON bars (ticker, timeframe, ts DESC);
