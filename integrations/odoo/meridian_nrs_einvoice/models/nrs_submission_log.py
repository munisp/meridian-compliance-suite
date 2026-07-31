# -*- coding: utf-8 -*-
"""NRS submission error queue / audit log.

One record per submission attempt (or webhook event). Failed submissions
land here with state 'error' and are retried by the cron with linear
backoff until max_attempts is reached.
"""

from odoo import fields, models


class NrsSubmissionLog(models.Model):
    _name = "nrs.submission.log"
    _description = "NRS e-Invoice Submission Log"
    _order = "create_date desc"

    move_id = fields.Many2one(
        "account.move", string="Invoice", required=True,
        ondelete="cascade", index=True,
    )
    company_id = fields.Many2one(
        related="move_id.company_id", store=True, readonly=True,
    )
    irn = fields.Char(string="IRN", index=True)
    payload = fields.Text(string="NRS Payload (JSON)", readonly=True)
    state = fields.Selection(
        selection=[
            ("queued", "Queued"),
            ("sent", "Sent"),
            ("error", "Error"),
        ],
        default="queued", required=True, index=True,
    )
    attempts = fields.Integer(default=0)
    max_attempts = fields.Integer(default=5)
    last_error = fields.Text(readonly=True)
    next_retry = fields.Datetime(string="Next Retry")
    event = fields.Char(string="Webhook Event", readonly=True)

    def action_retry_now(self):
        """Manual retry from the list/form view: requeue, cron picks it up."""
        for rec in self:
            rec.write({"state": "queued", "next_retry": fields.Datetime.now()})
        return True
