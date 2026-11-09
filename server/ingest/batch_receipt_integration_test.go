package main

import (
	"reflect"
	"testing"
)

func assertStoredBatchReplay(t *testing.T, first, replay IngestResult) {
	t.Helper()
	if !replay.DuplicateBatch || first.receipt == nil || replay.receipt == nil {
		t.Fatal("replay must carry the original durable receipt")
	}
	if first.receipt.TransactionID != replay.receipt.TransactionID || !first.receipt.CommittedAt.Equal(replay.receipt.CommittedAt) {
		t.Fatal("replay changed the commit identity or time")
	}
	first.receipt, replay.receipt = nil, nil
	first.DuplicateBatch, replay.DuplicateBatch = false, false
	first.Retries, replay.Retries = 0, 0
	if !reflect.DeepEqual(first, replay) {
		t.Fatal("replay changed committed counts, transaction or timestamp")
	}
}
