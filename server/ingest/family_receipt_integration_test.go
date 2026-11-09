package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func familyWireReceipt(t *testing.T, body map[string]json.RawMessage) FamilyBatchReceipt {
	t.Helper()
	data, ok := body["receipt"]
	if !ok {
		t.Fatal("personal response omitted the committed family receipt")
	}
	var receipt FamilyBatchReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestIntegration_FamilyHealthCompleteReceiptUsesBusinessLines(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	parent := f.uuid(0x7f0)
	sample := strings.Split(f.body(f.uuid(0x7f1), f.uuid(0x7f2), 62.5), "\n")[1]
	route := `{"route":{"workoutUUID":"` + parent + `","points":[{"t":1718000001000,"lat":52,"lon":13},{"t":1718000002000,"lat":52.1,"lon":13.1}]}}`
	series := `{"series":{"workoutUUID":"` + parent + `","type":"HKQuantityTypeIdentifierHeartRate","unit":"count/min","points":[{"t":1718000001000,"value":120},{"t":1718000002000,"value":121}]}}`
	deletion := `{"deleted":{"uuid":"` + f.uuid(0x7f3) + `","type":"HKQuantityTypeIdentifierHeartRate"}}`
	body := strings.Replace(ndjsonFull(t, 1, 1, 2, 0, 2, 0, sample, deletion, route, route, series, series),
		"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f", f.uuid(0x7f4), 1)
	status, first := f.post(t, actor, body)
	if status != http.StatusOK {
		t.Fatalf("mixed batch status=%d", status)
	}
	receipt := familyWireReceipt(t, first)
	_, generation := familyDeviceBinding(t, f, actor)
	sum := sha256.Sum256([]byte(body))
	if receipt.MemberID != actor.user || receipt.DeviceGeneration != generation || receipt.BatchID != f.uuid(0x7f4) ||
		receipt.BodyDigest != hex.EncodeToString(sum[:]) || receipt.State != "committed" ||
		receipt.RecordCount != 6 || receipt.Accepted != 3 || receipt.Duplicate != 2 || receipt.Deleted != 1 || receipt.Suppressed != 0 ||
		receipt.TransactionID == "" || receipt.CommittedAt.IsZero() {
		t.Fatalf("committed receipt did not describe six input lines: %+v", receipt)
	}
	if string(first["routePoints"]) != "2" || string(first["seriesPoints"]) != "2" {
		t.Fatal("legacy point counters changed meaning")
	}
	if status, replay := f.post(t, actor, body); status != http.StatusOK || !reflect.DeepEqual(first, replay) {
		t.Fatal("exact batch retry changed its original receipt")
	}
	secondBody := strings.Replace(body, f.uuid(0x7f4), f.uuid(0x7f5), 1)
	status, second := f.post(t, actor, secondBody)
	if status != http.StatusOK {
		t.Fatalf("fresh duplicate batch status=%d", status)
	}
	duplicate := familyWireReceipt(t, second)
	if duplicate.RecordCount != 6 || duplicate.Accepted != 0 || duplicate.Duplicate != 5 || duplicate.Deleted != 1 || duplicate.Suppressed != 0 {
		t.Fatal("fresh duplicate batch used physical row counts")
	}
	var stored []byte
	if err := f.admin.QueryRow(f.ctx, `SELECT result->'receipt' FROM family_batch_receipts WHERE user_id=$1 AND device_generation=$2 AND batch_id=$3`, actor.user, generation, receipt.BatchID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var persistent FamilyBatchReceipt
	if err := json.Unmarshal(stored, &persistent); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt, persistent) {
		t.Fatal("wire acknowledgement differs from the committed database receipt")
	}
}

func TestIntegration_FamilyHealthReceiptHeaderAndSuppression(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	headerOnly := strings.Split(f.body(f.uuid(0x800), f.uuid(0x801), 62.5), "\n")[0]
	headerOnly = strings.Replace(headerOnly, `"sampleCount":1`, `"sampleCount":0`, 1) + "\n"
	request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/batches", strings.NewReader(headerOnly))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+actor.token)
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Family-Receipt") != familyReceiptVersion {
		t.Fatal("family receiver did not require a structured acknowledgement")
	}
	sample := f.uuid(0x802)
	deletion := `{"deleted":{"uuid":"` + sample + `","type":"HKQuantityTypeIdentifierHeartRate"}}`
	deletedBody := strings.Replace(ndjson(t, 0, 1, deletion), "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f", f.uuid(0x803), 1)
	if status, _ := f.post(t, actor, deletedBody); status != http.StatusOK {
		t.Fatalf("delete status=%d", status)
	}
	status, body := f.post(t, actor, f.body(f.uuid(0x804), sample, 62.5))
	if status != http.StatusOK {
		t.Fatalf("suppressed batch status=%d", status)
	}
	receipt := familyWireReceipt(t, body)
	if receipt.RecordCount != 1 || receipt.Accepted != 0 || receipt.Duplicate != 0 || receipt.Deleted != 0 || receipt.Suppressed != 1 {
		t.Fatal("suppressed source content was called accepted or duplicate")
	}
}
