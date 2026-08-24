"""Pydantic models for the T9 ETR service (Pillar Two / GloBE).

Money is integer kobo everywhere (SPEC §1.3). Ownership percentages are
basis points (10000 = 100%).
"""
from __future__ import annotations

from typing import Literal, Optional

from pydantic import BaseModel, Field


class Group(BaseModel):
    id: str
    name: str
    consolidated_revenue_kobo: int = 0
    upe_jurisdiction: str = "NG"


class ConstituentEntity(BaseModel):
    id: str
    group_id: str
    name: str
    jurisdiction: str  # ISO 3166-1 alpha-2
    entity_type: str = "operating"  # operating|governmental|non_profit|pension_fund|investment_fund_upe|international_org
    parent_id: Optional[str] = None  # None => UPE
    ownership_bps: int = 10000  # parent's stake in this entity
    is_upe: bool = False
    is_pope: bool = False  # partially-owned parent entity
    net_income_kobo: int = 0
    covered_taxes_kobo: int = 0
    payroll_kobo: int = 0
    tangible_assets_kobo: int = 0
    revenue_kobo: int = 0
    employees: int = 0
    # CFC fields
    is_cfc: bool = False
    cfc_owner_id: Optional[str] = None
    cfc_taxes_kobo: int = 0  # taxes paid by owner on CFC income (pushdown pool)
    # NTA-basis extras
    development_levy_kobo: int = 0
    fines_penalties_kobo: int = 0
    priority_credit_kobo: int = 0
    stock_comp_kobo: int = 0
    excluded_dividends_kobo: int = 0
    asymmetric_fx_kobo: int = 0


class ComputeRequest(BaseModel):
    group_id: str
    fiscal_year: int = Field(ge=2024, le=2033)
    basis: Literal["dual", "nta", "globe"] = "dual"
    # DEPRECATED (audit fix B2-#10): client-supplied mirror of the reg-watch
    # gate. The server IGNORES this field; the QDMTT path activates only when
    # the reg-watch qdmtt_upgrade gate is resolved as armed server-side
    # (app/gates.py). Field retained for API backward compatibility only.
    qdmtt_upgrade: bool = False


class StepTrace(BaseModel):
    step_no: int
    name: str
    rule_ref: str
    pack: str
    inputs: dict
    outputs: dict
    narrate: str


class JurisdictionResult(BaseModel):
    jurisdiction: str
    basis: str
    entities: list[str]
    net_income_kobo: int
    covered_taxes_kobo: int
    sbie_kobo: int
    excess_profit_kobo: int
    etr_bps: int
    topup_pct_bps: int
    topup_kobo: int
    qdmtt_applied: bool
    qdmtt_kobo: int
    residual_topup_kobo: int


class IIRAllocation(BaseModel):
    from_jurisdiction: str
    to_entity_id: str
    to_entity_name: str
    mechanism: str  # iir_upe|iir_pope
    amount_kobo: int
    ownership_bps: int


class Computation(BaseModel):
    id: str
    group_id: str
    fiscal_year: int
    basis: str
    created_at: str
    in_scope: bool
    scope_reason: str
    qdmtt_upgrade: bool
    jurisdictions: list[JurisdictionResult]
    iir_allocations: list[IIRAllocation]
    total_topup_kobo: int
    cfc_pool_kobo: int
    trace: list[StepTrace]
    pack_versions: dict[str, str]
    digest: str = ""
