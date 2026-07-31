"""meridian_odoo_client — pure-Python bridge between Odoo and the Meridian
NRS e-invoicing service (services/einvoicing).

No Odoo imports live here: this package is importable and unit-testable
without an Odoo host. The Odoo addon (meridian_nrs_einvoice) wraps it.
"""

from .client import MeridianClient, MeridianAPIError
from .schema import (
    NRSPayloadError,
    TAX_CATEGORIES,
    build_irn,
    build_nrs_invoice,
    date_stamp,
    kobo_to_ngn,
    ngn_to_kobo,
    parse_irn,
    round_bps_half_up,
    valid_irn,
    valid_service_id,
)
from .hmac_verify import sign_webhook, verify_webhook_signature
from .lifecycle import (
    LIFECYCLE_STEPS,
    ODOO_STATUSES,
    map_service_status,
    next_status,
    valid_transition,
)

__all__ = [
    "MeridianClient",
    "MeridianAPIError",
    "NRSPayloadError",
    "TAX_CATEGORIES",
    "build_irn",
    "build_nrs_invoice",
    "date_stamp",
    "kobo_to_ngn",
    "ngn_to_kobo",
    "parse_irn",
    "round_bps_half_up",
    "valid_irn",
    "valid_service_id",
    "sign_webhook",
    "verify_webhook_signature",
    "LIFECYCLE_STEPS",
    "ODOO_STATUSES",
    "map_service_status",
    "next_status",
    "valid_transition",
]

__version__ = "1.0.0"
