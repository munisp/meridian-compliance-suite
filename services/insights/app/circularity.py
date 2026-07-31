"""I8 — Invoice-chain circularity detector.

Detects A->B->C->A VAT-padding loops in invoice flows (carousel-style fraud
indicator). In-process directed multigraph with a FalkorDB-ready interface.

REAL: graph build + cycle detection + VAT exposure aggregation.
SIMULATED: FalkorDBGraph backend (raises unless FALKORDB_URL is configured).
"""
from __future__ import annotations

import os
from abc import ABC, abstractmethod
from dataclasses import dataclass, field


@dataclass(frozen=True)
class Edge:
    seller: str
    buyer: str
    invoice_id: str
    vat_kobo: int


class GraphStore(ABC):
    @abstractmethod
    def add_edge(self, edge: Edge) -> None: ...

    @abstractmethod
    def edges_from(self, node: str) -> list[Edge]: ...

    @abstractmethod
    def nodes(self) -> list[str]: ...


class InMemoryGraph(GraphStore):
    def __init__(self) -> None:
        self._adj: dict[str, list[Edge]] = {}

    def add_edge(self, edge: Edge) -> None:
        self._adj.setdefault(edge.seller, []).append(edge)
        self._adj.setdefault(edge.buyer, self._adj.get(edge.buyer, []))

    def edges_from(self, node: str) -> list[Edge]:
        return self._adj.get(node, [])

    def nodes(self) -> list[str]:
        return list(self._adj)


class FalkorDBGraph(GraphStore):
    """FalkorDB-backed store (SIMULATED until FALKORDB_URL is wired)."""

    def __init__(self, url: str | None = None) -> None:
        self.url = url or os.environ.get("FALKORDB_URL", "")
        if not self.url:
            raise RuntimeError("FalkorDBGraph is SIMULATED: set FALKORDB_URL to enable")

    def add_edge(self, edge: Edge) -> None:  # pragma: no cover
        raise NotImplementedError

    def edges_from(self, node: str) -> list[Edge]:  # pragma: no cover
        raise NotImplementedError

    def nodes(self) -> list[str]:  # pragma: no cover
        raise NotImplementedError


@dataclass
class Cycle:
    path: list[str]
    invoice_ids: list[str]
    vat_exposure_kobo: int

    def as_dict(self) -> dict:
        return {"path": self.path, "length": len(self.path) - 1,
                "invoice_ids": self.invoice_ids,
                "vat_exposure_kobo": self.vat_exposure_kobo}


def find_cycles(graph: GraphStore, max_len: int = 5,
                min_vat_kobo: int = 1) -> list[Cycle]:
    """DFS for simple cycles up to max_len edges. Canonicalises rotation so
    each loop is reported once (lexicographically smallest start node)."""
    found: dict[tuple, Cycle] = {}

    def dfs(start: str, node: str, path: list[str], edges: list[Edge]) -> None:
        if len(edges) >= max_len:
            return
        for e in graph.edges_from(node):
            if e.buyer == start and edges:
                ring = path + [start]
                # canonical rotation: rotate so smallest node first
                body = ring[:-1]
                i = body.index(min(body))
                key = tuple(body[i:] + body[:i])
                vat = sum(x.vat_kobo for x in edges + [e])
                if vat >= min_vat_kobo and key not in found:
                    found[key] = Cycle(ring, [x.invoice_id for x in edges + [e]], vat)
            elif e.buyer not in path and len(path) < max_len:
                dfs(start, e.buyer, path + [e.buyer], edges + [e])

    for n in graph.nodes():
        dfs(n, n, [n], [])
    return sorted(found.values(), key=lambda c: -c.vat_exposure_kobo)


def build_graph(invoices: list[dict]) -> InMemoryGraph:
    """invoices: [{invoice_id, supplier_tin, customer_tin, vat_kobo}]"""
    g = InMemoryGraph()
    for inv in invoices:
        s, b = inv.get("supplier_tin"), inv.get("customer_tin")
        if s and b and s != b:
            g.add_edge(Edge(s, b, inv.get("invoice_id", ""), int(inv.get("vat_kobo") or 0)))
    return g
