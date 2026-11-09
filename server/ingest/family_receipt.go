package main

import (
	"encoding/hex"
	"errors"
	"time"
)

const familyReceiptVersion = "family-health-batch-receipt-v1"

type familyLineCounts struct {
	accepted, duplicate, deleted, suppressed int64
	routePoints, seriesPoints                int64
}

type FamilyBatchReceipt struct {
	MemberID         string    `json:"member_id"`
	DeviceGeneration int64     `json:"device_generation"`
	BatchID          string    `json:"batch_id"`
	BodyDigest       string    `json:"body_digest"`
	State            string    `json:"state"`
	RecordCount      int64     `json:"record_count"`
	Accepted         int64     `json:"accepted"`
	Duplicate        int64     `json:"duplicate"`
	Deleted          int64     `json:"deleted"`
	Suppressed       int64     `json:"suppressed"`
	TransactionID    string    `json:"transaction_id"`
	CommittedAt      time.Time `json:"committed_at"`
}

func buildFamilyReceipt(batch *Batch, userID string, generation int64, result *IngestResult, commit *committedBatchReceipt) (*FamilyBatchReceipt, error) {
	count := batch.Header.SampleCount + batch.Header.DeletionCount + batch.Header.RouteCount + batch.Header.SeriesCount +
		batch.Header.AggregateCount + batch.Header.ActivitySummaryCount + batch.Header.ProfileCount
	r := &FamilyBatchReceipt{MemberID: userID, DeviceGeneration: generation, BatchID: batch.Header.BatchID,
		BodyDigest: hex.EncodeToString(batch.bodyDigest), State: "committed", RecordCount: int64(count),
		Accepted:  result.Accepted + batch.familyCounts.accepted,
		Duplicate: result.Duplicates - int64(batch.suppressedSamples) + batch.familyCounts.duplicate,
		Deleted:   batch.familyCounts.deleted, Suppressed: batch.familyCounts.suppressed,
		TransactionID: commit.TransactionID, CommittedAt: commit.CommittedAt.UTC()}
	if !isUUID(r.MemberID) || !isUUID(r.BatchID) || !isUUID(r.TransactionID) || generation < 1 ||
		len(batch.bodyDigest) != 32 || r.CommittedAt.IsZero() || count < 0 || count > maxBatchLines {
		return nil, errors.New("family receipt identity or record count is incomplete")
	}
	for _, n := range []int64{r.Accepted, r.Duplicate, r.Deleted, r.Suppressed} {
		if n < 0 || n > r.RecordCount {
			return nil, errors.New("family receipt count is outside the input batch")
		}
	}
	if r.Accepted+r.Duplicate+r.Deleted+r.Suppressed != r.RecordCount {
		return nil, errors.New("family receipt counts do not cover the input batch")
	}
	if result.RoutePoints != batch.familyCounts.routePoints || result.SeriesPoints != batch.familyCounts.seriesPoints {
		return nil, errors.New("physical point writes differ from planned line outcomes")
	}
	return r, nil
}
