package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var errBatchConflict = errors.New("batch key conflicts with a committed upload")

type committedBatchReceipt struct {
	TransactionID string
	CommittedAt   time.Time
}

// nil means this transaction reserved the key and must finish it before commit.
func reserveBatchReceipt(ctx context.Context, tx pgx.Tx, b *Batch, userID string, generation int64) (*IngestResult, error) {
	if len(b.bodyDigest) != 32 {
		return nil, errors.New("parsed batch body digest is missing")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO family_batch_receipts
		(user_id, device_generation, batch_id, body_digest) VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id, device_generation, batch_id) DO NOTHING`,
		userID, generation, b.Header.BatchID, b.bodyDigest)
	if err != nil {
		return nil, fmt.Errorf("reserve receipt: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var digest, payload []byte
	var result IngestResult
	result.receipt = &committedBatchReceipt{}
	if err := tx.QueryRow(ctx, `SELECT body_digest, result, transaction_id::text, committed_at
		FROM family_batch_receipts WHERE user_id=$1 AND device_generation=$2 AND batch_id=$3`,
		userID, generation, b.Header.BatchID).Scan(&digest, &payload,
		&result.receipt.TransactionID, &result.receipt.CommittedAt); err != nil {
		return nil, fmt.Errorf("read committed receipt: %w", err)
	}
	if !bytes.Equal(digest, b.bodyDigest) {
		return nil, errBatchConflict
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("decode committed receipt: %w", err)
	}
	result.DuplicateBatch = true
	return &result, nil
}

func finishBatchReceipt(ctx context.Context, tx pgx.Tx, b *Batch, userID string, generation int64, result *IngestResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	result.receipt = &committedBatchReceipt{}
	return tx.QueryRow(ctx, `UPDATE family_batch_receipts SET result=$4::jsonb, committed_at=clock_timestamp()
		WHERE user_id=$1 AND device_generation=$2 AND batch_id=$3
		RETURNING transaction_id::text, committed_at`, userID, generation, b.Header.BatchID, payload).
		Scan(&result.receipt.TransactionID, &result.receipt.CommittedAt)
}
