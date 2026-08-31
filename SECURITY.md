# Security policy

## Reporting a vulnerability

Please report security issues **privately**. Do not open a public GitHub issue, and do not disclose
the problem publicly until a fix is available.

**Contact:** use GitHub's private vulnerability reporting — the **"Report a vulnerability"**
button under this repository's **Security** tab. The report reaches the maintainers privately.

Include as much of the following as you can:

- what the issue is, and the class of problem (auth bypass, injection, data exposure, …);
- the affected component (`gateway`, `auth`, `journal`, `services/llm`, …) and version or commit;
- minimal reproduction steps, or a proof-of-concept that demonstrates impact;
- what an attacker gains — read access to another user's data, session forgery, remote code
  execution, and so on.

We will acknowledge your report, keep you updated while we investigate, and credit you in the fix
notes unless you would rather stay anonymous.

**There is no bug bounty.** This is an unfunded open-source project; we cannot offer payment.

## Scope

Attestel is **self-hosted software**. Reports should concern the code in this repository — for
example:

- authentication and session handling (`auth`, the shared HMAC session cookie, `AUTH_SECRET`);
- authorization gaps between users (one account reading another's theses, journal, evidence,
  alerts or feedback);
- injection, path traversal, SSRF, or unsafe deserialization in any service;
- secret leakage through logs, API responses or the built frontend bundle;
- container or deployment defaults that are insecure out of the box.

Out of scope:

- vulnerabilities in third-party data providers or in a model runtime you point Attestel at;
- issues that require an attacker to already have shell or database access on your host;
- your own deployment's configuration (an exposed port, a default `AUTH_SECRET` left unchanged, a
  database reachable from the internet) — though we do want to hear about insecure *defaults*
  shipped in this repository;
- missing hardening headers with no demonstrated impact, and automated-scanner output without a
  working reproduction.

## Operator notes

If you deploy Attestel, at minimum:

- set a real, random `AUTH_SECRET` — the value in `.env.example` is
  `dev-insecure-change-me` and is meant to fail your review;
- never commit a real `.env`; only `.env.example` belongs in git;
- keep the internal service ports (8001–8004, 8095–8099) off the public internet — nginx on the
  single public port is the intended boundary, and the feedback service in particular expects
  network isolation *as well as* its admin allow-list (`FEEDBACK_ADMIN_UIDS`, which fails closed
  when empty);
- put a real contact address in `SEC_USER_AGENT` and `EVENTS_CONTACT_UA` before enabling any
  provider that writes to the shared event corpus.

Attestel holds no money and executes no trades, so there is no payment surface to attack — but it
does hold your research, your theses and your journal. Treat those as the sensitive data they are.
