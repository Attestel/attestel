# NVIDIA Research Reference — for the NVDA Platform

*Compiled 2026-07-07. Hard financial figures are from NVIDIA SEC filings / newsroom. Consensus
estimates and stock reactions are from financial media and are approximate. 2026 roadmap specs
that are press/secondary are flagged. Do not treat flagged items as verified.*

---

## 1. Fiscal calendar (read first — avoids the single biggest confusion)

NVIDIA's fiscal year ends in **late January**, ~one month ahead of the calendar. **FY2026 = the
~12 months ending Jan 25, 2026** (mostly covers calendar 2025).

| Fiscal quarter | Ended | Reported | ≈ Calendar |
|---|---|---|---|
| Q3 FY2026 | Oct 26 2025 | Nov 19 2025 | ~CY Q3 2025 |
| Q4 FY2026 | Jan 25 2026 | Feb 25 2026 | ~CY Q4 2025 |
| Q1 FY2027 | Apr 26 2026 | May 20 2026 | ~CY Q1 2026 |

As of today the **two most recent reported quarters are Q4 FY2026 and Q1 FY2027**. **Q2 FY2027 is
not out** — it ends ~late July 2026, reported ~late August 2026.

### Two accounting traps at the FY26 → FY27 boundary
1. **Non-GAAP now includes stock-based comp** starting Q1 FY2027 (previously excluded). Non-GAAP
   EPS across the boundary is not comparable unless restated. NVIDIA restated priors.
2. **New segment structure** from Q1 FY2027: Data Center split into Hyperscale + ACIE; Gaming/Auto
   folded into a new **"Edge Computing"** platform. **Standalone "Gaming" is no longer disclosed.**

---

## 2. Quarterly results

### Q1 FY2027 — ended Apr 26 2026, reported May 20 2026 (LATEST)
| Metric | Actual | Consensus | Result |
|---|---|---|---|
| Total revenue | **$81.615B** (+85% YoY, +20% QoQ) | ~$78.8B | Beat ~+3.5% |
| Data Center | **$75.2B** (+92% YoY) | — | Record |
| Edge Computing (incl. Gaming) | **$6.4B** (+29% YoY) | — | Gaming not broken out |
| EPS GAAP | **$2.39** | — | — |
| EPS non-GAAP (new basis, incl. SBC) | **$1.87** | ~$1.76 | Beat |
| Gross margin | GAAP 74.9% / non-GAAP 75.0% | — | In line |
| Q2 FY2027 guide | **$91.0B ±2%** | some ~$86B buy-side | Above sell-side; stock still fell |

- DC compute $60.4B (+77% YoY); DC networking record $14.8B (+199% YoY).
- China: Q2 outlook assumes **zero** China DC-compute revenue. No H20 charge this quarter (vs. the
  $4.5B H20 inventory charge a year earlier in Q1 FY2026).
- Capital returns: new **$80B** buyback; dividend raised $0.01 → **$0.25**.
- Reaction: **stock fell despite the beat** — buy-side had whispered a higher (~$86B+) Q2 number.
  (Exact next-day % not pinned to a primary source.)

### Q4 FY2026 — ended Jan 25 2026, reported Feb 25 2026
| Metric | Actual | Consensus | Result |
|---|---|---|---|
| Total revenue | **$68.127B** (+73% YoY) | ~$65.9B | Beat ~+3.4% |
| Data Center | **$62.3B** (+75% YoY) | — | Record |
| Gaming | **$3.7B** (+47% YoY, −13% QoQ) | — | Holiday channel normalization |
| EPS non-GAAP (old basis) | **$1.62** | ~$1.53 | Beat |
| EPS non-GAAP (restated, new basis) | **$1.59** | — | Use for apples-to-apples vs Q1 FY27 |
| Gross margin | GAAP 75.0% / non-GAAP 75.2% | — | Strong |
| Q1 FY2027 guide | **$78.0B ±2%** | ~$72.6B | Above |

- Full-year FY2026: revenue **$215.9B** (+65%), GAAP EPS $4.90, non-GAAP EPS $4.77.
- Reaction: initial after-hours gain that mostly faded.

### Q3 FY2026 — ended Oct 26 2025, reported Nov 19 2025 (context)
| Metric | Actual | Consensus | Result |
|---|---|---|---|
| Total revenue | **$57.006B** (+62% YoY) | ~$55.2B | Beat |
| Data Center | **$51.2B** (+66% YoY) | — | Record |
| Gaming | **$4.3B** (+30% YoY) | — | — |
| EPS (GAAP = non-GAAP) | **$1.30** | ~$1.25 | Beat |
| Q4 FY2026 guide | ~$65B midpoint | — | Raised |

- Jensen Huang: *"Blackwell sales are off the charts, and cloud GPUs are sold out."* Shares rose
  ~4% after-hours.

**Uncertain / not hard facts:** exact consensus numbers (vary by provider ±few hundred M); exact
post-earnings % moves (directions verified, magnitudes approximate); the ~$86B "whisper"; there is
no standalone Q1 FY27 Gaming figure.

---

## 3. Product roadmap & cadence

Committed ~one-generation-per-year cadence: **Blackwell (2024) → Blackwell Ultra (2025) → Rubin
(2026) → Rubin Ultra (2027) → Feynman (2028)**. Generation names are NVIDIA-confirmed; exact years
partly press/secondary.

- **Blackwell (B100/B200, GB200 NVL72)** — shipping, the volume installed base. Jensen: "king of
  inference today."
- **Blackwell Ultra (B300/GB300)** — shipping, ramping hard through 2026. Unit numbers
  (~129% YoY, ~60k racks) are press/secondary — treat as estimates.
- **Vera Rubin (Rubin R100 GPU + Vera CPU, VR200 NVL72)** — **announced at GTC 2026 (Mar 16-19);
  NVIDIA said May 31 2026 it is in "full production" with volume shipments beginning "this fall"
  (autumn 2026).** As of today it is **NOT yet shipping at scale** — don't describe it as shipping.
  Adds Spectrum-X Ethernet Photonics (co-packaged optics) + BlueField-4 DPUs. Detailed specs
  (~336B transistors, 288 GB HBM4, TSMC N3) are press/secondary.
- **Rubin Ultra (2027, "Kyber" NVL576)** and **Feynman (2028)** — announced, not shipping. Press/secondary.

---

## 4. Supply chain (the governor on revenue — supply-, not demand-constrained)

- **TSMC (foundry, single most critical dependency).** Blackwell on 4NP, Rubin on N3. **CoWoS
  advanced packaging is the binding constraint** — reported sold out through 2025 into 2026, NVIDIA
  ~60% of locked capacity. (Capacity figures press/secondary.)
- **HBM memory:** SK Hynix (lead HBM4 supplier), Micron (ramping HBM4 2026), Samsung (qualifying).
  HBM co-bonds during CoWoS, so memory + packaging supply are coupled.
- **ODM / server makers (NVIDIA-confirmed Vera Rubin partners):** Dell, HPE, Lenovo, Supermicro,
  Foxconn/Hon Hai, Pegatron, Wistron, Wiwynn, Quanta, GIGABYTE, ASUS, and others.
- **Networking (in-house, a moat not just a supplier):** NVLink (scale-up), Spectrum-X Ethernet +
  Photonics/CPO (scale-out), BlueField-4 DPUs.
- **Leading indicators to watch:** TSMC CoWoS commentary, SK Hynix / Micron HBM4 ramp, Foxconn /
  Supermicro rack shipments — these often move before NVIDIA confirms.

---

## 5. Catalysts & risks (2025-2026)

- **China / export controls (material swing factor):** Apr 2025 H20 license requirement → $4.5B
  charge; Jul 2025 partial reversal; Jan 2026 BIS reportedly shifted H200 review to case-by-case
  (≤50% of US volume) — *verify against the Federal Register before relying on it*. NVIDIA no
  longer includes China in its baseline forecast (China was ~1/5 of DC revenue at peak).
- **Sovereign AI:** Saudi Arabia / HUMAIN AI-factory partnership (confirmed); Europe unveiled 35
  new NVIDIA AI supercomputers (Jun 2026, confirmed). Saudi/xAI ~600k-chip figure is press.
- **AI-lab investments:** reports of NVIDIA stakes in OpenAI (~$30B within a larger round) and
  ~$10B in Anthropic — figures vary by source, **not confirmed**.
- **Competition:** AMD MI400 series (MI450 in 2026, OpenAI ~6 GW deal), Google TPU (Ironwood), AWS
  Trainium2/3, Microsoft Maia. Inference cost-sensitivity favors challengers; NVIDIA's bull case
  is the full-stack CUDA + networking + rack-scale moat.

---

## 6. Who / what to monitor (there is no "Musk" here)

Jensen Huang does **not** post constantly like Musk. Signal is concentrated in set-pieces:
**CES (Jan) → GTC San Jose (Mar) → GTC Taipei/COMPUTEX (May/Jun) → earnings (Feb/May/Aug/Nov)**.
CFO **Colette Kress**'s guidance on the earnings call is the most market-moving recurring event
(watch supply, China baseline, DC guide).

Ground-truth channels: **NVIDIA Newsroom** (nvidianews.nvidia.com), **NVIDIA Blog**
(blogs.nvidia.com), **Investor Relations** (investor.nvidia.com), **SEC filings** (10-K/10-Q, 8-K).
Supply signals surface first in TSMC / SK Hynix / Micron earnings and Taiwanese trade press
(lower-confidence, useful early warning).

---

## 7. Sources (primary in bold)

- **NVIDIA Q1 FY2027 8-K (SEC, May 20 2026)** — sec.gov/Archives/edgar/data/1045810/…/q1fy27pr.htm
- **NVIDIA Q4 & FY2026 8-K (SEC, Feb 25 2026)** — …/q4fy26pr.htm
- **NVIDIA Q3 FY2026 8-K (SEC, Nov 19 2025)** — …/q3fy26pr.htm
- **NVIDIA 10-K FY2026 (SEC)** — China/risk factors
- **NVIDIA Newsroom** — Q4 & FY2026 results; Vera Rubin full production (May 31 2026); GTC 2026;
  Saudi/HUMAIN; Europe 35 supercomputers
- CNBC earnings coverage (Q3 FY26, Q4 FY26, Q1 FY27); Fortune; Futurum; S&P Global (consensus, reaction)
- DigiTimes / Silicon Analysts / WccfTech (CoWoS, HBM4, GB300 — secondary, flagged)
- AMD MI400 (TechPowerUp); AWS Trainium3 (SemiAnalysis); BIS H200 policy (secondary — verify)
