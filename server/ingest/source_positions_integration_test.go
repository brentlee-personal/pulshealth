package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestIntegration_FamilyHealthLateWorkoutAndPointConflicts(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	parent := f.uuid(0x7b0)
	body := func(slot, samples, deletions, routes, series int, lines ...string) string {
		return strings.Replace(ndjsonFull(t, samples, deletions, routes, 0, series, 0, lines...),
			"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f", f.uuid(slot), 1)
	}
	route := `{"route":{"workoutUUID":"` + parent + `","points":[{"t":1718000001000,"lat":52,"lon":13}]}}`
	series := `{"series":{"workoutUUID":"` + parent + `","type":"HKQuantityTypeIdentifierHeartRate","unit":"count/min","points":[{"t":1718000001000,"value":120}]}}`
	if status, _ := f.post(t, actor, body(0x7b1, 0, 0, 1, 1, route, series)); status != http.StatusOK {
		t.Fatalf("early points status=%d", status)
	}
	workout := strings.Replace(workoutSample, "33333333-3333-4333-8333-333333333333", parent, 1)
	if status, _ := f.post(t, actor, body(0x7b2, 1, 0, 0, 0, workout)); status != http.StatusOK {
		t.Fatalf("late parent status=%d", status)
	}
	var links int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM workouts w
		JOIN workout_route_points p ON p.user_id=w.user_id AND p.workout_uuid=w.uuid
		JOIN workout_series_points s ON s.user_id=w.user_id AND s.workout_uuid=w.uuid
		WHERE w.user_id=$1 AND w.uuid<>$2::uuid`, actor.user, parent).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatal("late workout did not adopt its source-bound point parent")
	}
	changed := strings.Replace(route, `"lat":52`, `"lat":53`, 1)
	if status, _ := f.post(t, actor, body(0x7b3, 0, 0, 1, 0, changed)); status != http.StatusConflict {
		t.Fatalf("changed route point status=%d", status)
	}
	changedSeries := strings.Replace(series, `"value":120`, `"value":121`, 1)
	if status, _ := f.post(t, actor, body(0x7b4, 0, 0, 0, 1, changedSeries)); status != http.StatusConflict {
		t.Fatalf("changed series point status=%d", status)
	}
	if status := f.get(t, actor, "/v1/routes", nil); status != http.StatusForbidden {
		t.Fatalf("device GPS read status=%d", status)
	}
	deletion := `{"deleted":{"uuid":"` + parent + `","type":"HKWorkoutTypeIdentifier"}}`
	if status, _ := f.post(t, actor, body(0x7b5, 0, 1, 0, 0, deletion)); status != http.StatusOK {
		t.Fatalf("delete parent status=%d", status)
	}
	if status, _ := f.post(t, actor, body(0x7b6, 1, 0, 1, 1, workout, route, series)); status != http.StatusOK {
		t.Fatalf("delayed deleted content status=%d", status)
	}
	for _, table := range []string{"workouts", "workout_route_points", "workout_series_points"} {
		var count int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", actor.user).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("deleted content revived in %s", table)
		}
	}
}

func TestIntegration_FamilyHealthUnknownSourceDeleteBlocksLateRecord(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	sample := f.uuid(0x7c0)
	deletion := `{"deleted":{"uuid":"` + sample + `","type":"HKQuantityTypeIdentifierHeartRate"}}`
	body := strings.Replace(ndjson(t, 0, 1, deletion), "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f", f.uuid(0x7c1), 1)
	if status, _ := f.post(t, actor, body); status != http.StatusOK {
		t.Fatalf("unknown delete status=%d", status)
	}
	if status, result := f.post(t, actor, f.body(f.uuid(0x7c2), sample, 62.5)); status != http.StatusOK || string(result["accepted"]) != "0" {
		t.Fatal("late source record was not suppressed")
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM quantity_samples WHERE user_id=$1", actor.user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unknown deletion did not block the later sample")
	}
}

func TestIntegration_FamilyHealthConflictRollsBackSourceDelete(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	first, second := f.uuid(0x7d0), f.uuid(0x7d1)
	for i, id := range []string{first, second} {
		if status, _ := f.post(t, actor, f.body(f.uuid(0x7d2+i), id, 62.5)); status != http.StatusOK {
			t.Fatalf("initial status=%d", status)
		}
	}
	body := strings.Replace(f.body(f.uuid(0x7d4), second, 72.5), `"deletionCount":0`, `"deletionCount":1`, 1)
	body += `{"deleted":{"uuid":"` + first + `","type":"HKQuantityTypeIdentifierHeartRate"}}` + "\n"
	if status, _ := f.post(t, actor, body); status != http.StatusConflict {
		t.Fatalf("conflict status=%d", status)
	}
	var rows, tombstones int
	if err := f.admin.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM quantity_samples WHERE user_id=$1),
		(SELECT count(*) FROM family_source_tombstones WHERE user_id=$1)`, actor.user).Scan(&rows, &tombstones); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || tombstones != 0 {
		t.Fatal("record conflict committed a partial source deletion")
	}
}
