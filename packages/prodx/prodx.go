// Package prodx holds the production integration clients for the Meridian
// compliance-suite services (HARDENING.md H1/H3):
//
//   - Postgres via pgx/v5, selected by DATABASE_URL. DocStore mirrors each
//     service's dev JSONL document store as a JSONB document table with
//     idempotent DDL (CREATE TABLE IF NOT EXISTS). OutboxStore mirrors the
//     file outbox (SPEC §1.1 producer pattern) in Postgres.
//   - Kafka (Redpanda) producer via franz-go, selected by KAFKA_BROKERS,
//     implementing the same Publish(topic, key, value) shape the services'
//     dev inproc bus / file outbox relay use.
//
// Selection rule (H1): the constructors return (nil, nil) when the prod env
// var is unset, so services keep their dev fallback with zero config and
// startup never fails because a prod var is missing.
package prodx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
)

// ---------------- Postgres ----------------

// PGFromEnv returns a pgx pool when DATABASE_URL is set, else (nil, nil).
// Logs the H1 profile line for component=store.
func PGFromEnv(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Printf("profile=dev component=store")
		return nil, nil
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgx ping: %w", err)
	}
	log.Printf("profile=prod component=store (postgres)")
	return pool, nil
}

// DocStore is a JSONB document store keyed by (collection, id), matching the
// dev stores' "append-only, last write wins by id" semantics via UPSERT.
type DocStore struct {
	pool  *pgxpool.Pool
	table string
}

// NewDocStore creates (idempotently) the document table
// <prefix>_docs(collection text, id text, doc jsonb, updated_at timestamptz).
func NewDocStore(ctx context.Context, pool *pgxpool.Pool, prefix string) (*DocStore, error) {
	d := &DocStore{pool: pool, table: prefix + "_docs"}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  collection text NOT NULL,
  id text NOT NULL,
  doc jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (collection, id)
)`, d.table)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return nil, fmt.Errorf("docstore ddl: %w", err)
	}
	return d, nil
}

// Put upserts a JSON document.
func (d *DocStore) Put(ctx context.Context, collection, id string, doc []byte) error {
	_, err := d.pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (collection, id, doc, updated_at) VALUES ($1,$2,$3,now())
		 ON CONFLICT (collection, id) DO UPDATE SET doc = EXCLUDED.doc, updated_at = now()`, d.table),
		collection, id, doc)
	return err
}

// List returns all documents of a collection ordered by first-seen update
// time then id (stable enough to mirror the dev store's insertion order).
func (d *DocStore) List(ctx context.Context, collection string) ([][]byte, error) {
	rows, err := d.pool.Query(ctx,
		fmt.Sprintf(`SELECT doc FROM %s WHERE collection=$1 ORDER BY updated_at, id`, d.table), collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// OutboxStore mirrors the file outbox in Postgres: one row per event with
// monotonically increasing seq, status pending|published|failed.
type OutboxStore struct {
	pool  *pgxpool.Pool
	table string
}

// OutboxRow mirrors the dev outbox row shape.
type OutboxRow struct {
	Seq       int64
	Topic     string
	Envelope  []byte
	Status    string
	Attempts  int
	CreatedAt time.Time
}

// NewOutboxStore creates (idempotently) <prefix>_outbox.
func NewOutboxStore(ctx context.Context, pool *pgxpool.Pool, prefix string) (*OutboxStore, error) {
	o := &OutboxStore{pool: pool, table: prefix + "_outbox"}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  seq bigserial PRIMARY KEY,
  topic text NOT NULL,
  envelope jsonb NOT NULL,
  status text NOT NULL DEFAULT 'pending',
  attempts int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
)`, o.table)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return nil, fmt.Errorf("outbox ddl: %w", err)
	}
	return o, nil
}

// Publish appends a pending outbox row and returns its seq.
func (o *OutboxStore) Publish(ctx context.Context, topic string, envelope []byte) (int64, error) {
	var seq int64
	err := o.pool.QueryRow(ctx,
		fmt.Sprintf(`INSERT INTO %s (topic, envelope) VALUES ($1,$2) RETURNING seq`, o.table),
		topic, envelope).Scan(&seq)
	return seq, err
}

// Pending returns rows that still need relaying (pending or failed).
func (o *OutboxStore) Pending(ctx context.Context, limit int) ([]OutboxRow, error) {
	rows, err := o.pool.Query(ctx,
		fmt.Sprintf(`SELECT seq, topic, envelope, status, attempts, created_at FROM %s
		 WHERE status IN ('pending','failed') ORDER BY seq LIMIT $1`, o.table), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.Seq, &r.Topic, &r.Envelope, &r.Status, &r.Attempts, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Mark sets the status/attempts of a row.
func (o *OutboxStore) Mark(ctx context.Context, seq int64, status string, attempts int) error {
	ct, err := o.pool.Exec(ctx,
		fmt.Sprintf(`UPDATE %s SET status=$1, attempts=$2 WHERE seq=$3`, o.table), status, attempts, seq)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("outbox seq %d not found", seq)
	}
	return nil
}

// ---------------- Kafka ----------------

// Producer is a franz-go producer for nrs.* topics (KAFKA_BROKERS).
type Producer struct {
	client *kgo.Client
}

// ProducerFromEnv returns a producer when KAFKA_BROKERS is set, else
// (nil, nil). Logs the H1 profile line for component=bus.
func ProducerFromEnv() (*Producer, error) {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		log.Printf("profile=dev component=bus")
		return nil, nil
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(brokers, ",")...),
		kgo.DefaultProduceTopic(""),
		kgo.RequiredAcks(kgo.AllISRAcks()),
	)
	if err != nil {
		return nil, fmt.Errorf("franz-go client: %w", err)
	}
	log.Printf("profile=prod component=bus (kafka brokers=%s)", brokers)
	return &Producer{client: cl}, nil
}

// Publish synchronously produces one record (topic set per record).
func (p *Producer) Publish(ctx context.Context, topic string, key, value []byte) error {
	if p == nil || p.client == nil {
		return errors.New("kafka producer not configured")
	}
	rec := &kgo.Record{Topic: topic, Key: key, Value: value}
	return p.client.ProduceSync(ctx, rec).FirstErr()
}

// Close flushes and closes the producer.
func (p *Producer) Close() {
	if p == nil || p.client == nil {
		return
	}
	p.client.Close()
}
