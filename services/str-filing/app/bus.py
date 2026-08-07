"""Kafka intake for AML risk events (topic ``nrs.aml.str.created``).

This repo has no shared kafka-python bus wrapper (the platform envelope bus
is Go, packages/shared/envelope; Python services are HTTP-first), so this
module is the consumer: a thin kafka-python consumer, env-gated by
KAFKA_BOOTSTRAP_SERVERS. When unset the service runs HTTP-intake-only (dev
default, same zero-deps posture as sibling services).

Message contract (produced by kyc-engine in meridian-inclusion-suite on
PEP/EDD escalation or sanctions hits):

    {
      "tenant_id": "t-...",            // required
      "idempotency_key": "kyc-engine:<case_id>:<screening_id>",  // required
      "subject_ref": "customer/tin ref",
      "report_type": "STR",            // STR | CTR
      "payload": {...},                // AML case facts (JSON object)
      "actor": "kyc-engine"
    }

The consumer delegates to the same intake function as the REST endpoint, so
idempotency and audit semantics are identical for both paths.
"""
from __future__ import annotations

import json
import logging
import os
import threading

log = logging.getLogger("str-filing.bus")

TOPIC = "nrs.aml.str.created"


def kafka_enabled() -> bool:
    return bool(os.environ.get("KAFKA_BOOTSTRAP_SERVERS"))


def start_consumer(intake, stop: threading.Event) -> threading.Thread | None:
    """intake(event_dict, actor) -> (record_dict, created_bool)."""
    if not kafka_enabled():
        log.info("KAFKA_BOOTSTRAP_SERVERS unset; kafka intake disabled "
                 "(HTTP intake only)")
        return None
    from kafka import KafkaConsumer  # imported lazily; dev needs no broker

    def loop():
        consumer = KafkaConsumer(
            TOPIC,
            bootstrap_servers=os.environ["KAFKA_BOOTSTRAP_SERVERS"].split(","),
            group_id=os.environ.get("STR_KAFKA_GROUP", "str-filing"),
            enable_auto_commit=False,
            value_deserializer=lambda b: json.loads(b.decode("utf-8")),
            auto_offset_reset="earliest",
        )
        log.info("consuming %s", TOPIC)
        while not stop.is_set():
            for msg in consumer.poll(timeout_ms=500).values():
                for record in msg:
                    try:
                        intake(record.value, actor="kafka:" + TOPIC)
                        consumer.commit()
                    except ValueError as exc:
                        # poison message: commit past it, it can never intake
                        log.error("invalid STR event offset=%s: %s",
                                  record.offset, exc)
                        consumer.commit()
                    except Exception:
                        log.exception("intake failed offset=%s; will retry",
                                      record.offset)

    t = threading.Thread(target=loop, daemon=True, name="str-kafka-consumer")
    t.start()
    return t
