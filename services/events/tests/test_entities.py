"""Tier rollup, ticker resolution and the event-type guess (contract §3.1, §3.3, §3.4, §9.15).

Three deterministic resolvers. The tests that matter most are the negative ones: an unresolved
document contributes NO ticker rather than a guessed one, and a tie in primary selection breaks the
same way every time.
"""
from __future__ import annotations

import json

import pytest

from app.config import SOURCE_TIERS
from app.entities import (
    ALIAS_MIN_TOKEN_LENGTH,
    EVENT_TYPES,
    PROVIDER_TIER,
    RELEVANCE_PRIMARY,
    RELEVANCE_SECONDARY_ALIAS,
    RELEVANCE_SECONDARY_PROVIDER,
    RESOLUTION_ALIAS,
    RESOLUTION_FILER,
    RESOLUTION_PROVIDER,
    TIER_RANK,
    guess_event_type,
    highest_tier,
    official_confirmed,
    primary_ticker,
    resolve_tickers,
    tier_for_provider,
)


def doc(**overrides) -> dict:
    row = {
        "provider": "marketaux",
        "title": "NVIDIA headline",
        "raw_tickers": json.dumps(["NVDA"]),
        "source_tier": "professional",
    }
    row.update(overrides)
    return row


# ---- tiers ---------------------------------------------------------------------------------------


def test_every_contract_provider_has_a_tier():
    """§3.1's provider list is closed; a provider without a tier would silently fall to a default."""
    assert set(PROVIDER_TIER) == {
        "marketaux", "sec-edgar", "google-news-rss", "alphavantage", "fred", "fmp",
        "federal-reserve", "company-ir",
        # Phase 2 — the two statistical agencies that publish the releases FRED redistributes.
        "bls", "bea",
    }


def test_official_providers_are_the_official_ones():
    assert tier_for_provider("sec-edgar") == "official"
    assert tier_for_provider("fred") == "official"
    assert tier_for_provider("federal-reserve") == "official"
    assert tier_for_provider("company-ir") == "official"
    assert tier_for_provider("marketaux") == "professional"
    assert tier_for_provider("google-news-rss") == "professional"


def test_the_discussion_tier_is_honoured_but_unmapped():
    """No provider maps to `discussion` today — adding one is a contract amendment. The tier must
    still rank and roll up correctly wherever it is read."""
    assert "discussion" not in PROVIDER_TIER.values()
    assert "discussion" in TIER_RANK
    assert set(TIER_RANK) == set(SOURCE_TIERS)
    assert highest_tier(["discussion"]) == "discussion"
    assert highest_tier(["discussion", "professional"]) == "professional"


def test_tier_rollup_takes_the_highest_not_the_alphabetical():
    """'official' < 'professional' alphabetically, so a lexical max() here picks the wrong tier."""
    assert highest_tier(["professional", "official"]) == "official"
    assert highest_tier(["official", "professional"]) == "official"
    assert max(["professional", "official"]) == "professional"  # what NOT to do


def test_official_confirmed_needs_an_official_document():
    assert official_confirmed(["professional", "official"]) == 1
    assert official_confirmed(["professional", "discussion"]) == 0
    assert official_confirmed([]) == 0


# ---- ticker resolution ---------------------------------------------------------------------------


def test_sec_filer_outranks_everything():
    resolved = resolve_tickers(doc(
        provider="sec-edgar",
        raw_tickers=json.dumps(["NVDA", "AMD"]),
        title="8-K filed 2026-08-14 - Results of Operations",
    ))
    assert resolved[0].ticker == "NVDA"
    assert resolved[0].priority == RESOLUTION_FILER
    assert [r.priority for r in resolved] == [RESOLUTION_FILER, RESOLUTION_PROVIDER]


def test_a_non_sec_provider_has_no_filer():
    resolved = resolve_tickers(doc(raw_tickers=json.dumps(["NVDA"])))
    assert resolved[0].priority == RESOLUTION_PROVIDER


def test_marketaux_body_only_entities_are_not_company_links():
    """Marketaux reports every entity found in the article body. A body-level NVDA mention must
    not turn a Broadcom headline into NVDA news; the raw provider metadata remains untouched in the
    source document and only the event-company mapping is narrowed here.
    """
    resolved = resolve_tickers(doc(
        title="Broadcom shares rise after a product announcement",
        raw_tickers=json.dumps(["NVDA", "AVGO"]),
    ))
    assert [(row.ticker, row.priority) for row in resolved] == [
        ("AVGO", RESOLUTION_PROVIDER),
    ]
    assert primary_ticker(resolved) == "AVGO"


def test_marketaux_exclusion_phrases_are_not_subject_mentions():
    resolved = resolve_tickers(doc(
        title="Not Nvidia. Not AMD. This semiconductor company could lead the market.",
        raw_tickers=json.dumps(["NVDA", "AMD"]),
    ))
    assert resolved == []


def test_marketaux_exact_ticker_in_title_is_a_subject_mention():
    resolved = resolve_tickers(doc(
        title="NVDA reports quarterly results",
        raw_tickers=json.dumps(["NVDA"]),
    ))
    assert primary_ticker(resolved) == "NVDA"


def test_alias_matching_is_case_insensitive_and_word_bounded():
    resolved = resolve_tickers(doc(
        raw_tickers="[]", title="Nvidia and Advanced Micro Devices both raised guidance"
    ))
    assert {r.ticker for r in resolved} == {"NVDA", "AMD"}
    assert {r.priority for r in resolved} == {RESOLUTION_ALIAS}


def test_an_alias_inside_a_longer_word_does_not_match():
    resolved = resolve_tickers(doc(raw_tickers="[]", title="Metadata pipelines at scale"))
    assert resolved == []


def test_an_unresolvable_document_contributes_no_ticker():
    """No ticker rather than a guessed one — an invented company link would poison every feed that
    filters on it."""
    resolved = resolve_tickers(doc(
        raw_tickers="[]", title="Regulators publish updated guidance for market participants"
    ))
    assert resolved == []
    assert primary_ticker(resolved) is None


def test_short_tokens_are_never_alias_matched():
    assert ALIAS_MIN_TOKEN_LENGTH == 3
    from app.entities import TICKER_ALIASES

    for aliases in TICKER_ALIASES.values():
        for alias in aliases:
            assert len(alias) >= ALIAS_MIN_TOKEN_LENGTH


def test_raw_tickers_are_uppercased_trimmed_and_deduped():
    resolved = resolve_tickers(doc(
        provider="alphavantage",
        raw_tickers=json.dumps([" nvda ", "NVDA", "amd"]),
    ))
    assert [r.ticker for r in resolved] == ["AMD", "NVDA"] or \
           [r.ticker for r in resolved] == ["NVDA", "AMD"]
    assert len(resolved) == 2


def test_malformed_raw_tickers_do_not_raise():
    assert resolve_tickers(doc(raw_tickers="not json", title="x")) == []
    assert resolve_tickers(doc(raw_tickers=json.dumps([None, "", "!!!"]), title="x")) == []


# ---- primary selection ---------------------------------------------------------------------------


def test_primary_is_the_earliest_mentioned_at_the_strongest_priority():
    resolved = resolve_tickers(doc(
        raw_tickers=json.dumps(["NVDA", "GOOGL"]),
        title="Alphabet and NVIDIA expand their cloud accelerator partnership",
    ))
    assert primary_ticker(resolved) == "GOOGL"  # "Alphabet" appears first


def test_primary_ties_break_lexically_and_stably():
    """Two provider-reported tickers, neither named in the title: the result must not depend on
    which order the provider happened to list them in. Structured providers retain this fallback;
    Marketaux deliberately requires a headline subject because its entity list includes body-only
    mentions."""
    a = resolve_tickers(doc(
        provider="alphavantage", raw_tickers=json.dumps(["NVDA", "AMD"]),
        title="New export restrictions announced"
    ))
    b = resolve_tickers(doc(
        provider="alphavantage", raw_tickers=json.dumps(["AMD", "NVDA"]),
        title="New export restrictions announced"
    ))
    assert primary_ticker(a) == primary_ticker(b) == "AMD"


def test_relevance_constants_are_the_contract_values():
    """§9.15: deterministic, coarse and write-once. The enrichment endpoint rejects `relevance`."""
    assert RELEVANCE_PRIMARY == 1.0
    assert RELEVANCE_SECONDARY_PROVIDER == 0.6
    assert RELEVANCE_SECONDARY_ALIAS == 0.4


def test_alias_only_secondaries_score_lower_than_provider_reported_ones():
    resolved = resolve_tickers(doc(
        raw_tickers=json.dumps(["NVDA"]),
        title="NVIDIA and Broadcom both named in the filing",
    ))
    by_ticker = {r.ticker: r.relevance for r in resolved}
    assert by_ticker["NVDA"] == RELEVANCE_SECONDARY_PROVIDER
    assert by_ticker["AVGO"] == RELEVANCE_SECONDARY_ALIAS


# ---- event type ----------------------------------------------------------------------------------


def test_every_guess_is_a_member_of_the_closed_enum():
    titles = [
        "NVIDIA reports record revenue", "Fed holds rates steady after the FOMC meeting",
        "Company sues rival over patent infringement", "A completely unclassifiable headline",
        "Broadcom raises its full-year guidance", "Board appoints a new chief executive",
    ]
    for title in titles:
        assert guess_event_type(doc(title=title)) in EVENT_TYPES


def test_an_unmatched_title_is_other_not_an_invented_type():
    assert guess_event_type(doc(title="A completely unclassifiable headline")) == "other"


def test_sec_item_labels_drive_the_type():
    """gateway/providers.go renders 8-K item codes into the headline; matching the label beats
    matching the prose that follows it."""
    assert guess_event_type(doc(
        provider="sec-edgar", title="8-K filed 2026-08-14 - Results of Operations"
    )) == "earnings_result"
    assert guess_event_type(doc(
        provider="sec-edgar", title="8-K filed 2026-08-14 - Exec/Director Change"
    )) == "management_change"
    assert guess_event_type(doc(
        provider="sec-edgar", title="8-K filed 2026-08-14 - Acquisition/Disposition"
    )) == "ma_transaction"


def test_an_8k_with_no_recognised_item_falls_through_to_the_title_rules():
    assert guess_event_type(doc(
        provider="sec-edgar", title="8-K filed 2026-08-14 - new export restrictions disclosed"
    )) == "regulatory_action"


def test_fred_defaults_to_a_macro_release():
    assert guess_event_type(doc(provider="fred", title="Series update published")) == "macro_release"


def test_central_bank_outranks_macro_release():
    """Rule order is part of the versioned rule: the FIRST match wins."""
    assert guess_event_type(doc(
        provider="fred", title="FOMC holds rates steady as inflation data cools"
    )) == "central_bank"


@pytest.mark.parametrize("title, expected", [
    ("NVIDIA reports record revenue for the quarter", "earnings_result"),
    ("Broadcom raises its full-year guidance", "earnings_guidance"),
    ("Analyst upgrades the stock after the print", "analyst_revision"),
    ("US widens export controls on advanced chips", "regulatory_action"),
    ("Shareholders file a class action over disclosures", "legal_action"),
    ("Chipmaker to buy a rival networking vendor", "ma_transaction"),
    ("Chief executive resigns after four years", "management_change"),
    ("Board approves a larger share repurchase", "capital_return"),
    ("Company prices senior notes due 2032", "financing"),
    ("Two firms announce a joint venture in packaging", "partnership"),
    ("Vendor unveils its next-generation accelerator", "product_launch"),
    ("Foundry capacity remains tight into next year", "supply_chain"),
])
def test_title_rule_table(title, expected):
    assert guess_event_type(doc(title=title)) == expected


def test_the_guess_is_stable_for_the_same_document():
    """It is an INPUT to cluster_key, so it must never change for a document once computed."""
    document = doc(title="US widens export controls on advanced chips")
    assert len({guess_event_type(document) for _ in range(5)}) == 1
