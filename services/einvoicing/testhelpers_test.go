package main

import (
	"path/filepath"

	"github.com/munisp/meridian-compliance-suite/packages/shared/envelope"
)

func newTestOutbox(dir string) (*envelope.Outbox, error) {
	return envelope.NewOutbox(filepath.Join(dir, "outbox.jsonl"))
}
