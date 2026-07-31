package main

// NRS 8-step e-invoice lifecycle (official NRS/Gention spec), sequential,
// each step explicit, recorded on the workflow run and individually retryable
// via the runner's retry/backoff:
//
//	1 create-store     invoice creation + durable store
//	2 irn-generate     IRN = <InvoiceNumber>-<ServiceID>-<YYYYMMDD>
//	3 irn-validate     format / uniqueness / structure
//	4 irn-sign         sign IRN with taxpayer CSID key -> QR payload
//	5 schema-validate  NRS schema + rp packs, fail fast with descriptive errors
//	6 invoice-sign     CSID signature + core-hash lock (immutability)
//	7 transmit         webhook notifications to stakeholders
//	8 confirm          reconciliation check against store -> status "confirmed"
//
// If the payload already carries an IRN, steps 2-3 short-circuit (the
// client-supplied IRN is format-verified and reused — resubmission after a
// failure is idempotent on that IRN).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	nrsStepCreate   = "1-create-store"
	nrsStepIRNGen   = "2-irn-generate"
	nrsStepIRNValid = "3-irn-validate"
	nrsStepIRNSign  = "4-irn-sign"
	nrsStepSchema   = "5-schema-validate"
	nrsStepSign     = "6-invoice-sign"
	nrsStepTransmit = "7-transmit"
	nrsStepConfirm  = "8-confirm"
)

func nrsFail(srv *Server, inv *CanonicalInvoice, err error) error {
	inv.Status = "failed"
	_, _ = srv.store.Save(inv)
	return err
}

// wfNRSEinvoice is the full NRS lifecycle workflow.
func wfNRSEinvoice(ctx context.Context, srv *Server, invoiceID string, rec func(Step)) error {
	inv, ok := srv.store.Get(invoiceID)
	if !ok {
		return fmt.Errorf("invoice %s not found", invoiceID)
	}

	// Step 1: creation/store (the handler persisted the draft; verify + record)
	err := retryActivity(ctx, nrsStepCreate, 3, rec, func() (string, error) {
		if _, ok := srv.store.Get(invoiceID); !ok {
			return "", fmt.Errorf("invoice %s not persisted", invoiceID)
		}
		return "stored draft " + invoiceID, nil
	})
	if err != nil {
		return err
	}

	// Step 2: IRN generation (skipped when the payload supplied one)
	err = retryActivity(ctx, nrsStepIRNGen, 3, rec, func() (string, error) {
		if inv.IRN != "" {
			return "skipped: client-supplied IRN " + inv.IRN, nil
		}
		sid, err := srv.serviceIDs.GetOrAssign(inv.BusinessID)
		if err != nil {
			return "", err
		}
		irn, err := BuildIRN(inv.InvoiceNumber, sid, inv.IssueDate)
		if err != nil {
			return "", err
		}
		inv.ServiceID = sid
		inv.IRN = irn
		return "generated " + irn, nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}

	// Step 3: IRN validation — format, uniqueness, structure
	err = retryActivity(ctx, nrsStepIRNValid, 3, rec, func() (string, error) {
		num, sid, ds, err := ParseIRN(inv.IRN)
		if err != nil {
			return "", err
		}
		if num != inv.InvoiceNumber {
			return "", fmt.Errorf("irn invoice number %q does not match invoice %q", num, inv.InvoiceNumber)
		}
		if want, _ := DateStamp(inv.IssueDate); ds != want {
			return "", fmt.Errorf("irn date stamp %s does not match issue date %s", ds, inv.IssueDate)
		}
		if !ValidServiceID(sid) {
			return "", fmt.Errorf("irn service id %q invalid", sid)
		}
		if existing, found := srv.store.GetByIRN(inv.IRN); found && existing.ID != inv.ID {
			return "", fmt.Errorf("irn %s already used by invoice %s (not unique)", inv.IRN, existing.ID)
		}
		inv.ServiceID = sid
		return "irn valid: format+uniqueness+structure ok", nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}

	// Step 4: IRN signage — sign the IRN with the taxpayer CSID key; the
	// signature feeds the verification QR payload.
	err = retryActivity(ctx, nrsStepIRNSign, 3, rec, func() (string, error) {
		payload := "NRS-IRN-SIGN-V1|" + inv.IRN + "|" + inv.Supplier.TIN + "|" + inv.Hash()
		sig := srv.signer.SignPayload(payload)
		inv.Stamp = &CryptoStamp{
			Algorithm: "ed25519", KeyID: srv.signer.KeyID(),
			IRN: inv.IRN, Payload: payload, Signature: sig,
			StampedAt: time.Now().UTC().Format(time.RFC3339),
		}
		return "irn signed with " + srv.signer.KeyID(), nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}

	// Step 5: invoice schema validation — fail fast with the full descriptive
	// NRS-style error list, then the rp packs.
	err = retryActivity(ctx, nrsStepSchema, 3, rec, func() (string, error) {
		if inv.NRSPayload != "" {
			var n NRSInvoice
			if err := json.Unmarshal([]byte(inv.NRSPayload), &n); err != nil {
				return "", fmt.Errorf("stored NRS payload corrupt: %w", err)
			}
			if errs := n.Validate(); len(errs) > 0 {
				return "", errs
			}
		}
		violations, fatal, err := srv.validator.Validate(inv, false)
		if err != nil {
			return "", err
		}
		inv.Validation = violations
		if fatal {
			return "", fmt.Errorf("fatal validation violations: %d", len(violations))
		}
		return fmt.Sprintf("schema valid (%d pack warnings)", len(violations)), nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}

	// Step 6: invoice signage — CSID signature + core-field lock. After this
	// point the invoice number and all core data are immutable (only
	// payment_status / payment_reference remain mutable via PATCH).
	err = retryActivity(ctx, nrsStepSign, 3, rec, func() (string, error) {
		srv.signer.SignInvoice(inv)
		inv.SignedCoreHash = inv.CoreHash()
		inv.Status = "signed"
		if _, err := srv.store.Save(inv); err != nil {
			return "", err
		}
		return "invoice signed; core fields locked", nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}

	// Step 7: transmission — webhook notifications to the business's
	// registered stakeholders (HMAC-signed, retried).
	err = retryActivity(ctx, nrsStepTransmit, 3, rec, func() (string, error) {
		if err := srv.webhooks.Notify(ctx, inv.BusinessID, "nrs.einvoice.transmitted.v1", map[string]any{
			"irn": inv.IRN, "invoice_id": inv.ID, "status": inv.Status,
			"payable_kobo": inv.PayableKobo,
		}); err != nil {
			return "", err
		}
		env, err := newInvoiceEvent("nrs.einvoice.transmitted.v1", inv)
		if err != nil {
			return "", err
		}
		if err := srv.outbox.Publish("nrs.einvoice.transmitted.v1", env); err != nil {
			return "", err
		}
		inv.Status = "transmitted"
		if _, err := srv.store.Save(inv); err != nil {
			return "", err
		}
		return fmt.Sprintf("transmitted to %d stakeholder endpoint(s)", len(srv.webhooks.Endpoints(inv.BusinessID))), nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}

	// Step 8: confirmation — reconciliation check against the store.
	err = retryActivity(ctx, nrsStepConfirm, 3, rec, func() (string, error) {
		stored, ok := srv.store.Get(inv.ID)
		if !ok {
			return "", fmt.Errorf("invoice %s missing from store at confirmation", inv.ID)
		}
		if stored.IRN != inv.IRN {
			return "", fmt.Errorf("reconciliation: stored irn %q != %q", stored.IRN, inv.IRN)
		}
		if stored.CoreHash() != inv.SignedCoreHash {
			return "", fmt.Errorf("reconciliation: core fields diverged after signage")
		}
		if stored.PayableKobo != inv.PayableKobo {
			return "", fmt.Errorf("reconciliation: payable %d != %d", stored.PayableKobo, inv.PayableKobo)
		}
		inv.Status = "confirmed"
		if _, err := srv.store.Save(inv); err != nil {
			return "", err
		}
		env, err := newInvoiceEvent("nrs.einvoice.confirmed.v1", inv)
		if err != nil {
			return "", err
		}
		if err := srv.outbox.Publish("nrs.einvoice.confirmed.v1", env); err != nil {
			return "", err
		}
		return "reconciled; status confirmed", nil
	})
	if err != nil {
		return nrsFail(srv, inv, err)
	}
	return nil
}
