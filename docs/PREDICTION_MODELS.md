# Prediction models & the buy/sell signal — research + recommendation

*Compiled 2026-07-07. For adding a directional prediction and a user-facing buy/sell suggestion
(execution stays 100% on the user; the app never places orders).*

## Read this first — honest expectations

No open-source model — and no model, period — reliably predicts short-term stock direction. The
current (2026) evidence is blunt about the ceiling:

- Real equity edges at days-to-weeks horizons **rarely exceed Sharpe 1.5–2**. A backtest showing
  **Sharpe > 3 almost always means a bug** — look-ahead leakage, survivorship bias, or over-tuning.
- Even a solid, peer-reviewed model degrades when the regime shifts: a 2026 regime-aware LightGBM
  study saw its signal's rank-correlation **turn negative during the 2024 AI-driven rally** — i.e.
  it stopped working exactly when the market changed character.
- A useful directional model is one that's right maybe **53–56%** of the time with positive
  expectancy after costs. That is "good," not "amazing," and it decays.
- The base rate is brutal: **most people who trade actively lose money.** The small edge that does
  exist is perishable, and it is what a beginner is fighting from day one.
- Getting consistent is a **multi-year arc — commonly ~2–3 years** of deliberate practice, journaling,
  and surviving your own mistakes. Anyone promising faster is selling something.

So the goal is not to find a magic predictor. It's to build a *modest, honest* signal, **prove it
with a walk-forward backtest including trading costs**, and always show it **with its real track
record and a confidence number** — never a bare "BUY". This is what makes a buy/sell option
responsible rather than dangerous. The design keeps the human (you) executing every trade.

## The options (all open-source, all run locally on your Mac)

**A. Time-series foundation models** — Chronos-2 / Chronos-Bolt (Amazon, Apache-2.0, 9M–710M
params), TimesFM 2.5 (Google), Moirai-2 (Salesforce), Lag-Llama, MOMENT.
- *What they do:* zero-shot forecast the price series (a path + uncertainty band), no training.
- *Reality for stocks:* they forecast smooth continuations and are weak at the **sign** of noisy
  short-horizon returns — good for an "expected path" visual and an uncertainty cone, poor as a
  standalone tradable signal. The original tech doc already earmarked Chronos and warned: "expect
  wide, often-wrong intervals — treat as one input among many." That still holds.
- *Fit:* optional **secondary** prior / chart overlay. Chronos-Bolt-small runs on CPU and fits the
  16 GB budget **if run sequentially with qwen** (never both loaded at once — original memory rule).

**B. Gradient-boosted classifier (LightGBM / XGBoost)** on the indicator features we already
compute → probability of an up move over a chosen horizon. **← recommended core.**
- *Why it fits this project best:* CPU-only and tiny (no GPU, trivial memory), **interpretable**
  (feature importances tell you *why*), directly **reuses our existing feature pipeline** (RSI,
  MACD, ADX, stochastic, OBV, VWAP, Bollinger, confluence), and — most important — it pairs
  naturally with a **walk-forward backtest** that measures whether it actually has an edge. The 2026
  literature uses exactly this (regime-aware LightGBM, walk-forward, Sharpe ~1.2) as a credible,
  honest baseline.

**C. Deep learning (LSTM / Transformer / TFT).** Heavier, far more overfit-prone on single-name
data, GPU-hungry, and not worth it for a solo project before a simpler model is proven. Skip for now.

**D. The LLM (qwen) as a predictor.** Language models are bad at numeric prediction. Keep qwen for
the narrative read only — it must stay non-prescriptive. The number comes from the quant model, not
the LLM.

## Recommendation

1. **Core signal: LightGBM directional classifier** on our features, per (ticker, timeframe,
   horizon), trained and **validated with walk-forward backtesting** (realistic costs, no
   look-ahead, out-of-sample final holdout).
2. **Optional overlay: Chronos-Bolt-small** as an "expected path + uncertainty cone" on the chart —
   clearly a prior, not a signal, and run sequentially with qwen.
3. **The buy/sell suggestion is gated on the backtest.** The signal panel shows: direction
   (Buy / Hold / Sell bias), probability, a confidence number, **and the model's walk-forward
   hit-rate / Sharpe / expectancy / max-drawdown**. If there's no passing backtest, it shows
   "insufficient validation — no signal," not a guess. A backtest Sharpe > 3 is auto-flagged as
   *suspect (probable leakage)*, not celebrated.
4. **Track it live.** Every prediction is logged (and tied into the journal) so live hit-rate can be
   compared to the backtest — the early-warning system for the inevitable decay.
5. **Execution stays with you.** The app surfaces the suggestion; you place the trade. No broker
   integration, ever.

## Validation is the actual product here
The model is 20% of the work; the **honest walk-forward backtest is 80%** and the thing that earns
the right to show a buy/sell label. Requirements: features strictly lagged (no future data), rolling
or expanding walk-forward folds, transaction cost + slippage modeled, a final untouched holdout, and
the Sharpe-ceiling sanity check. Without these, a buy/sell signal is just confident noise.

## Sources
- [The 2026 Time Series Toolkit — 5 foundation models](https://machinelearningmastery.com/the-2026-time-series-toolkit-5-foundation-models-for-autonomous-forecasting/)
- [Regime-Aware LightGBM, validated walk-forward framework (MDPI, 2026)](https://www.mdpi.com/2079-9292/15/6/1334)
- [ML for Trading tutorial — walk-forward & realistic edges (2026)](https://www.quantt.co.uk/resources/machine-learning-for-trading-tutorial)
- [Chronos / TimesFM / Moirai unified access — TSFM.ai](https://tsfm.ai/)
