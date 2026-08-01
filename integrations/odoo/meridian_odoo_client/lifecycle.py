"""NRS 8-step e-invoice lifecycle — Odoo-side status model.

Mirrors services/einvoicing/nrs_lifecycle.go. The Go service is the source
of truth; the Odoo addon mirrors the coarse status on account.move
(nrs_clearance_status) so users can track and retry from the invoice form.
"""

from __future__ import annotations

LIFECYCLE_STEPS = [
    "1-create-store",
    "2-irn-generate",
    "3-irn-validate",
    "4-irn-sign",
    "5-schema-validate",
    "6-invoice-sign",
    "7-transmit",
    "8-confirm",
]

# Odoo-side selection values for account.move.nrs_clearance_status
ODOO_STATUSES = [
    "draft",        # not yet submitted
    "submitted",    # accepted by the service, lifecycle running (received)
    "signed",       # steps 1-6 done; core fields locked
    "transmitted",  # step 7 done; stakeholders notified
    "confirmed",    # step 8 done; reconciled
    "failed",       # lifecycle failed at some step (see nrs_last_error)
]

# Go CanonicalInvoice.Status -> Odoo status
_SERVICE_STATUS_MAP = {
    "received": "submitted",
    "signed": "signed",
    "transmitted": "transmitted",
    "confirmed": "confirmed",
    "failed": "failed",
}

# Permitted forward transitions (failed is reachable from any active state;
# resubmission after failure goes back to submitted via a new IRN run).
_TRANSITIONS = {
    "draft": {"submitted", "failed"},
    "submitted": {"signed", "failed"},
    "signed": {"transmitted", "failed"},
    "transmitted": {"confirmed", "failed"},
    "confirmed": set(),
    "failed": {"submitted"},  # retry / resubmit
}


def map_service_status(service_status: str) -> str:
    """Map a Meridian service status string to the Odoo clearance status."""
    return _SERVICE_STATUS_MAP.get((service_status or "").lower(), "submitted")


def valid_transition(current: str, new: str) -> bool:
    return new in _TRANSITIONS.get(current, set())


def next_status(current: str, event: str) -> str:
    """Advance the Odoo-side status for a lifecycle event.

    Events: 'submitted', 'signed', 'transmitted', 'confirmed', 'failed',
    'retry'. Returns the new status; raises ValueError on an illegal
    transition (callers should log, not crash, on out-of-order webhooks).
    """
    if event == "retry":
        target = "submitted"
    elif event in ODOO_STATUSES:
        target = event
    else:
        target = map_service_status(event)
    if not valid_transition(current, target):
        raise ValueError("illegal lifecycle transition %s -> %s" % (current, target))
    return target
