package main

import (
	"math"
	"strings"
	"testing"
)

func TestParseBatch_SourceNumericRepresentation(t *testing.T) {
	t.Run("quantity negative zero", func(t *testing.T) {
		body := ndjson(t, 1, 0, strings.Replace(hrSample, `"value":62.5`, `"value":-0`, 1))
		batch, err := ParseFamilyBatch(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if !math.Signbit(*batch.Samples[0].Value) {
			t.Fatal("validation changed the source number representation")
		}
	})
	t.Run("ECG source precision", func(t *testing.T) {
		line := `{"uuid":"11111111-1111-4111-8111-111111111111","type":"HKDataTypeIdentifierElectrocardiogram","kind":"ecg","start":1718000000000,"end":1718000005000,"ecg":{"classification":"sinusRhythm","symptomsStatus":"none","voltagesUV":[1.000000059604644775390624999999]}}`
		batch, err := ParseFamilyBatch(strings.NewReader(ndjson(t, 1, 0, line)))
		if err != nil {
			t.Fatal(err)
		}
		// The source is just below the midpoint between two float32 values.
		if batch.Samples[0].ECG.VoltagesUV[0] != 1 {
			t.Fatal("validation rounded the source before the ECG decoder")
		}
	})
}
