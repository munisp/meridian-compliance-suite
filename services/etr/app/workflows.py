"""wf-etr-compute / wf-globe-extract / wf-filingpack-build workflow functions.

Dev in-process runner with retry + step trace (mirrors core temporal-sdkx
contract; production wires the same functions via Temporal).
"""
from __future__ import annotations

import time
from typing import Any, Callable

from fastapi import HTTPException

from .engine import compute
from .gir import build_filing_pack, build_gir_xml
from .models import ComputeRequest


def run_steps(name: str, steps: list[tuple[str, Callable[[], Any]]]) -> dict:
    trace = []
    result: Any = None
    for step_name, fn in steps:
        start = time.perf_counter()
        attempts = 0
        err: Exception | None = None
        while attempts < 3:
            attempts += 1
            try:
                result = fn()
                err = None
                break
            except Exception as exc:  # retry policy: 3 attempts
                err = exc
                time.sleep(0.05 * attempts)
        entry = {"name": step_name, "attempts": attempts,
                 "duration_ms": round((time.perf_counter() - start) * 1000, 2),
                 "status": "ok" if err is None else "failed"}
        if err is not None:
            entry["detail"] = str(err)
            trace.append(entry)
            return {"workflow": name, "status": "failed", "steps": trace}
        trace.append(entry)
    return {"workflow": name, "status": "completed", "steps": trace, "result": result}


def wf_etr_compute(ctx, params: dict) -> dict:
    req = ComputeRequest(**params)
    group = ctx.store.get_group(req.group_id)
    if not group:
        raise HTTPException(404, f"group {req.group_id} not found")
    entities = ctx.store.entities(req.group_id)
    if not entities:
        raise HTTPException(422, f"group {req.group_id} has no constituent entities")
    holder: dict = {}

    def do_compute():
        holder["comp"] = compute(ctx.packs, group, entities, req)
        return {"computation_id": holder["comp"].id,
                "total_topup_kobo": holder["comp"].total_topup_kobo}

    def do_persist():
        ctx.store.put_computation(holder["comp"])
        return {"persisted": True}

    out = run_steps("wf-etr-compute", [
        ("load-entities", lambda: {"entities": len(entities)}),
        ("compute-etr", do_compute),
        ("persist", do_persist),
    ])
    if out["status"] == "completed":
        out["computation"] = holder["comp"].model_dump()
    return out


def wf_globe_extract(ctx, params: dict) -> dict:
    """wf-globe-extract: pull pack-driven GloBE parameters for a fiscal year."""
    year = int(params.get("fiscal_year", 2025))

    def extract():
        payroll_bps, assets_bps = ctx.packs.sbie_bps(year)
        return {
            "fiscal_year": year,
            "minimum_rate_bps": ctx.packs.minimum_rate_bps(),
            "sbie": {"payroll_bps": payroll_bps, "assets_bps": assets_bps},
            "revenue_threshold_ngn_kobo": ctx.packs.revenue_threshold_kobo(),
            "excluded_entity_types": ctx.packs.excluded_entity_types(),
            "qdmtt_jurisdictions": ctx.packs.qdmtt_jurisdictions(),
            "pope_threshold_bps": ctx.packs.pope_threshold_bps(),
            "pack_versions": ctx.packs.versions(),
        }

    return run_steps("wf-globe-extract", [("extract-params", extract)])


def wf_filingpack_build(ctx, params: dict) -> dict:
    comp_id = params.get("computation_id", "")
    comp = ctx.store.get_computation(comp_id)
    if not comp:
        raise HTTPException(404, f"computation {comp_id} not found")
    group = ctx.store.get_group(comp.group_id)
    entities = ctx.store.entities(comp.group_id)
    holder: dict = {}

    def gir():
        holder["gir"] = build_gir_xml(ctx.packs, comp, group, entities)
        return {"gir_bytes": len(holder["gir"])}

    def fpack():
        holder["fpack"] = build_filing_pack(ctx.packs, comp, group, entities)
        return {"filing_pack_id": holder["fpack"]["filing_pack_id"]}

    out = run_steps("wf-filingpack-build", [("build-gir", gir), ("build-filing-pack", fpack)])
    if out["status"] == "completed":
        out["gir_xml"] = holder["gir"]
        out["filing_pack"] = holder["fpack"]
    return out


WORKFLOWS = {
    "wf-etr-compute": wf_etr_compute,
    "wf-globe-extract": wf_globe_extract,
    "wf-filingpack-build": wf_filingpack_build,
}
