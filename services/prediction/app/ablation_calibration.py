"""The ablation ladder's calibration layer — Wave 5 Lane 5A.

WHY THIS FILE IS NOT `calibration.py`, AND THAT IS A MEASURED FINDING, NOT A PREFERENCE
---------------------------------------------------------------------------------------
`QWEN_IMPLEMENTATION_PLAN.md`'s Wave 5 table names Lane 5A's files as
`services/prediction/app/{ablation,calibration}.py`. **`calibration.py` already exists** and belongs
to Step 05's quant signal: it is the isotonic/PAV calibrator that maps LightGBM's raw probability to
an honest one, it is imported by the signal path, and its own tests cover it. Writing this module
there would have silently replaced the signal's calibrator with something that answers a different
question — the one failure mode invariant #2 cannot survive, since the signal is the single
directional output the whole product is allowed to have.

The two objects are genuinely different and both are needed:

  * `calibration.py`      raw MODEL PROBABILITY -> honest probability. Continuous in, continuous out.
  * this file             ordinal CONFIDENCE BUCKET -> empirical probability. `low`/`medium`/`high`
                          in, a counted frequency out.

`confidenceBucket` is ORDINAL and is not a probability (contract §7's binding rule, doc §9, §16.6).
`low` / `medium` / `high` say only that the model ranked one call above another. This module is the
one place that turns an ordinal bucket into an empirical probability, and it does it the only way
that is honest: by counting how often calls in that bucket were actually right, on data the
measurement was not fitted to.

THE ONE RULE THAT MAKES EVERY NUMBER HERE MEAN ANYTHING
-------------------------------------------------------
**A calibration is FITTED on one split and APPLIED to another, or it is not a calibration.** Fitting
on `dev` and scoring `dev` recovers the training frequencies and produces a beautiful reliability
diagram that predicts nothing. `apply_to` raises `LeakageRefused` when the two splits are the same;
there is no flag to override it, because there is no legitimate reason to want one. Same posture as
`committee_features.py` refusing to let an LLM stance influence the signal before it has a track
record: an unearned number is worse than no number.

WHY A BUCKET CAN COME BACK UNCALIBRATED
---------------------------------------
A bucket with three observations has an empirical frequency of 0, ⅓, ⅔ or 1, and none of those is a
probability — it is a rounding of a tiny sample. Below `MIN_BIN_SAMPLES` the bucket stays
**uncalibrated** and every consumer excludes it, reporting the coverage it achieved instead of
smoothing a guess into the gap.

WHAT THIS MODULE NEVER DOES
---------------------------
It never converts a score into a probability by arithmetic (`(score + 1) / 2` and its friends). That
mapping is invented, it is monotone in a quantity nobody validated, and a Brier score computed on it
would look exactly like a real one. If no calibration exists, the answer is `None`, and the caller
reports "not calibrated" rather than a number.
"""
from __future__ import annotations

import json
import math
import os
from dataclasses import dataclass, field
from datetime import datetime, timezone

from . import db as _db

__all__ = [
    "CALIBRATION_VERSION", "MIN_BIN_SAMPLES", "BUCKETS", "DIRECTIONS", "ABSTAIN_DIRECTIONS",
    "Calibration", "Bin", "LeakageRefused", "bin_key", "fit", "apply_to", "probability_for",
    "reliability_table", "expected_calibration_error", "brier_skill_score",
    "write_calibration", "load_calibration",
]

CALIBRATION_VERSION = "ablation-calibration@1"

#: Below this, a bucket is left UNCALIBRATED. 20 is a stated floor, not a derived one: it is the
#: point at which an empirical frequency stops being a rounding of a handful of observations. It is
#: named here so a later change is a visible decision rather than a tweak.
MIN_BIN_SAMPLES = 20

BUCKETS = ("low", "medium", "high")
DIRECTIONS = ("bullish", "bearish")
#: Abstentions are NOT calibrated. "How often is a refusal to take a side correct?" is not a
#: question with an answer — there is no side to be right about (`ablation.py` makes the same point
#: about directional accuracy).
ABSTAIN_DIRECTIONS = ("neutral", "unclear")


class LeakageRefused(Exception):
    """A calibration was asked to score the split it was fitted on. Refused, never warned about."""


@dataclass(frozen=True)
class Bin:
    """One (direction, bucket, horizon) cell of the calibration."""

    key: str
    direction: str
    bucket: str
    horizon: str
    n: int
    correct: int

    @property
    def calibrated(self) -> bool:
        return self.n >= MIN_BIN_SAMPLES

    @property
    def probability(self) -> float | None:
        """Empirical P(this call was directionally right). `None` below the floor — NEVER a
        smoothed or shrunk estimate, because a shrunk estimate is indistinguishable at the call
        site from a measured one."""
        return (self.correct / self.n) if self.calibrated else None

    def as_dict(self) -> dict:
        return {"key": self.key, "direction": self.direction, "bucket": self.bucket,
                "horizon": self.horizon, "n": self.n, "correct": self.correct,
                "calibrated": self.calibrated, "probability": self.probability}


@dataclass(frozen=True)
class Calibration:
    """A fitted map. Carries the split it was fitted on so `apply_to` can refuse the leak."""

    version: str
    experiment: str
    fitted_on_split: str
    fitted_at: str
    bins: dict[str, Bin] = field(default_factory=dict)

    def bin_for(self, direction: str, bucket: str, horizon: str) -> Bin | None:
        return self.bins.get(bin_key(direction, bucket, horizon))

    @property
    def coverage(self) -> dict:
        total = sum(b.n for b in self.bins.values())
        calibrated = sum(b.n for b in self.bins.values() if b.calibrated)
        return {
            "bins": len(self.bins),
            "calibratedBins": sum(1 for b in self.bins.values() if b.calibrated),
            "observations": total,
            "calibratedObservations": calibrated,
            "fraction": (calibrated / total) if total else None,
            "minBinSamples": MIN_BIN_SAMPLES,
        }

    def as_dict(self) -> dict:
        return {
            "version": self.version,
            "experiment": self.experiment,
            "fittedOnSplit": self.fitted_on_split,
            "fittedAt": self.fitted_at,
            "coverage": self.coverage,
            "bins": {k: b.as_dict() for k, b in sorted(self.bins.items())},
        }


def bin_key(direction: str, bucket: str, horizon: str) -> str:
    return f"{direction}|{bucket}|{horizon}"


# =================================================================================================
# Fitting
# =================================================================================================


def _entries(record: dict):
    """`(horizon, entry, outcome)` for every horizon on one record. One place that knows the shape,
    so `ablation.py` and this file cannot drift into two readings of it."""
    horizons = ((record.get("forecast") or {}).get("horizons") or {})
    outcomes = record.get("outcomes") or {}
    for horizon, entry in horizons.items():
        if isinstance(entry, dict):
            yield horizon, entry, (outcomes.get(horizon) or {})


def _bucket_of(record: dict, entry: dict) -> str:
    """The bucket that qualified THIS call. Per-horizon when the record carries one, else the
    record-level bucket §5's `forecast` object holds."""
    bucket = str(entry.get("confidenceBucket") or "").strip().lower()
    if bucket in BUCKETS:
        return bucket
    return str(((record.get("forecast") or {}).get("confidenceBucket") or "")).strip().lower()


def _was_correct(entry: dict, outcome: dict) -> bool | None:
    """`True` / `False` for a scored directional call, `None` for anything that cannot be scored.

    `None` covers three genuinely different situations and they all mean the same thing here: the
    call contributes to no frequency. An abstention had no side; an unresolved outcome has no
    answer yet; a flat realised return matches neither `bullish` nor `bearish`, and counting it as a
    miss would penalise a call for the market not moving.
    """
    direction = str(entry.get("direction") or "").lower()
    if entry.get("abstain") or direction in ABSTAIN_DIRECTIONS or direction not in DIRECTIONS:
        return None
    if not outcome.get("resolved"):
        return None
    realized = outcome.get("realizedReturn")
    if realized is None:
        return None
    actual = "bullish" if realized > 0 else "bearish" if realized < 0 else "flat"
    if actual == "flat":
        return None
    return direction == actual


def fit(records: list[dict], *, split: str, experiment: str,
        now: str | None = None) -> Calibration:
    """Count, per (direction, bucket, horizon), how often the call was right.

    Deliberately not a logistic regression, a spline or isotonic regression — `calibration.py` is
    where isotonic lives, and it fits over a CONTINUOUS probability where that is the right tool.
    Here there are four horizons × three buckets × two directions = 24 cells; a parametric fit over
    a few hundred observations would smooth exactly the sparsity `MIN_BIN_SAMPLES` exists to expose,
    and the smoothing would be invisible in the output.
    """
    if not split:
        raise ValueError("a calibration must record the split it was fitted on, or `apply_to` "
                         "cannot refuse the leak it exists to refuse")
    tally: dict[str, list[int]] = {}
    for record in records:
        if record.get("split") and record["split"] != split:
            raise ValueError(
                f"record split {record['split']!r} does not match the requested fit split "
                f"{split!r}; a calibration fitted across two splits belongs to neither")
        for horizon, entry, outcome in _entries(record):
            correct = _was_correct(entry, outcome)
            if correct is None:
                continue
            key = bin_key(str(entry.get("direction")).lower(), _bucket_of(record, entry), horizon)
            slot = tally.setdefault(key, [0, 0])
            slot[0] += 1
            slot[1] += int(correct)

    bins = {}
    for key, (n, correct) in tally.items():
        direction, bucket, horizon = key.split("|")
        bins[key] = Bin(key=key, direction=direction, bucket=bucket, horizon=horizon,
                        n=n, correct=correct)
    return Calibration(
        version=CALIBRATION_VERSION,
        experiment=experiment,
        fitted_on_split=split,
        fitted_at=now or datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        bins=bins,
    )


# =================================================================================================
# Applying — out-of-sample, and it refuses to be anything else
# =================================================================================================


def probability_for(calibration: Calibration | None, record: dict, entry: dict,
                    horizon: str) -> float | None:
    """The calibrated P(directionally right) for one call, or `None`.

    `None` when there is no calibration, when the bucket never reached the floor, or when the call
    is an abstention. Every caller must handle `None` by EXCLUDING the call and reporting coverage —
    never by substituting a default probability, which would be the invented mapping this module's
    header refuses.
    """
    if calibration is None:
        return None
    direction = str(entry.get("direction") or "").lower()
    if entry.get("abstain") or direction not in DIRECTIONS:
        return None
    found = calibration.bin_for(direction, _bucket_of(record, entry), horizon)
    return found.probability if found else None


def apply_to(calibration: Calibration, records: list[dict], *, split: str) -> dict:
    """Score `records` (all from `split`) against a calibration fitted on a DIFFERENT split.

    Returns the reliability evidence: per-bin predicted-vs-observed, the expected calibration error,
    the Brier score over the calibrated subset, and the coverage achieved. Raises `LeakageRefused`
    when the splits match.
    """
    if split == calibration.fitted_on_split:
        raise LeakageRefused(
            f"this calibration was fitted on {calibration.fitted_on_split!r} and was asked to "
            f"score {split!r}. Scoring the fit split recovers the training frequencies and "
            "measures nothing. Fit on `dev`, score on `validation`; keep `test` for the end."
        )
    rows: list[tuple[float, bool]] = []
    excluded = {"abstained": 0, "unscored": 0, "uncalibratedBin": 0}
    for record in records:
        if record.get("split") and record["split"] != split:
            raise ValueError(f"record split {record['split']!r} is not the scored split {split!r}")
        for horizon, entry, outcome in _entries(record):
            correct = _was_correct(entry, outcome)
            if correct is None:
                direction = str(entry.get("direction") or "").lower()
                key = "abstained" if (entry.get("abstain") or direction in ABSTAIN_DIRECTIONS) \
                    else "unscored"
                excluded[key] += 1
                continue
            probability = probability_for(calibration, record, entry, horizon)
            if probability is None:
                excluded["uncalibratedBin"] += 1
                continue
            rows.append((probability, bool(correct)))

    scored = len(rows)
    considered = scored + sum(excluded.values())
    base_rate = (sum(1 for _, hit in rows if hit) / scored) if scored else None
    brier = _brier(rows)
    return {
        "version": CALIBRATION_VERSION,
        "experiment": calibration.experiment,
        "fittedOnSplit": calibration.fitted_on_split,
        "scoredSplit": split,
        "scored": scored,
        "considered": considered,
        # THE HONESTY NUMBER. A brilliant ECE over 4% of the calls is not a calibrated pipeline.
        "coverage": (scored / considered) if considered else None,
        "excluded": excluded,
        "baseRate": base_rate,
        "reliability": reliability_table(rows),
        "expectedCalibrationError": expected_calibration_error(rows),
        "brier": brier,
        "brierSkillScore": brier_skill_score(brier, base_rate),
    }


def _brier(rows: list[tuple[float, bool]]) -> float | None:
    """Mean squared error of the calibrated probability against the binary outcome.

    `None`, never 0.0, on an empty set: 0.0 is a perfect score and would read as the best possible
    result for a pipeline that made no scorable call at all.
    """
    if not rows:
        return None
    return sum((p - (1.0 if hit else 0.0)) ** 2 for p, hit in rows) / len(rows)


def reliability_table(rows: list[tuple[float, bool]], *, bins: int = 10) -> list[dict]:
    """Predicted vs observed, in equal-width probability bins. Empty bins are OMITTED, not emitted
    with `observed: 0` — an empty bin is an absence of evidence, and printing a zero there is the
    §9.44 mistake in chart form."""
    buckets: dict[int, list[tuple[float, bool]]] = {}
    for probability, hit in rows:
        index = min(bins - 1, max(0, int(probability * bins)))
        buckets.setdefault(index, []).append((probability, hit))
    table = []
    for index in sorted(buckets):
        cell = buckets[index]
        table.append({
            "bin": f"[{index / bins:.1f},{(index + 1) / bins:.1f})",
            "n": len(cell),
            "predicted": sum(p for p, _ in cell) / len(cell),
            "observed": sum(1 for _, hit in cell if hit) / len(cell),
        })
    return table


def expected_calibration_error(rows: list[tuple[float, bool]], *, bins: int = 10) -> float | None:
    """Sample-weighted mean |predicted − observed| across occupied bins. `None` on an empty set."""
    if not rows:
        return None
    table = reliability_table(rows, bins=bins)
    total = sum(cell["n"] for cell in table)
    if not total:
        return None
    return sum(cell["n"] * abs(cell["predicted"] - cell["observed"]) for cell in table) / total


def brier_skill_score(brier: float | None, base_rate: float | None) -> float | None:
    """Brier relative to always predicting the base rate. Positive ⇒ the calibrated probabilities
    beat a constant. `None` propagates — an unmeasurable skill score is not a skill of zero."""
    if brier is None or base_rate is None:
        return None
    reference = base_rate * (1.0 - base_rate)
    if not reference or math.isclose(reference, 0.0):
        return None
    return 1.0 - (brier / reference)


# =================================================================================================
# Persistence — a calibration is an artefact, and it names its own provenance
# =================================================================================================


def write_calibration(calibration: Calibration, out_dir: str) -> str:
    path = os.path.join(out_dir, f"calibration-{calibration.experiment}-"
                                 f"{calibration.fitted_on_split}.json")
    payload = json.dumps(calibration.as_dict(), indent=2, sort_keys=True).encode()
    if _db.enabled():
        _db.save_artifact(os.path.basename(path), "application/json", payload)
    else:
        os.makedirs(out_dir, exist_ok=True)
        with open(path, "wb") as fh:
            fh.write(payload)
    return path


def load_calibration(path: str) -> Calibration | None:
    """`None` for a missing file — an absent calibration is the ordinary state, not an error."""
    try:
        if _db.enabled():
            raw = _db.load_artifact(os.path.basename(path))
            if raw is None:
                return None
            payload = json.loads(raw)
        else:
            with open(path, encoding="utf-8") as fh:
                payload = json.load(fh)
    except (OSError, ValueError):
        return None
    bins = {}
    for key, row in (payload.get("bins") or {}).items():
        bins[key] = Bin(key=key, direction=row["direction"], bucket=row["bucket"],
                        horizon=row["horizon"], n=int(row["n"]), correct=int(row["correct"]))
    return Calibration(
        version=str(payload.get("version") or CALIBRATION_VERSION),
        experiment=str(payload.get("experiment") or ""),
        fitted_on_split=str(payload.get("fittedOnSplit") or ""),
        fitted_at=str(payload.get("fittedAt") or ""),
        bins=bins,
    )
