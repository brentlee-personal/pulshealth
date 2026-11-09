package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

//go:generate sh -c "cp ../../docs/protocol/schema/*.json protocol/; cp ../../docs/protocol/catalog.json protocol/catalog.json"
//go:embed protocol/*.json
var protocolFiles embed.FS

type familyCatalogType struct {
	Identifier                string
	Kind                      string
	Unit                      *string
	AllowedAggregateFunctions []string
}

type familyProtocolContract struct {
	schemas map[string]*jsonschema.Resolved
	types   map[string]familyCatalogType
}

var errFamilyRecord = errors.New("record violates the family source contract")

var familyProtocol = sync.OnceValues(func() (*familyProtocolContract, error) {
	entries, err := protocolFiles.ReadDir("protocol")
	if err != nil {
		return nil, err
	}
	registry := map[string]*jsonschema.Schema{}
	names := map[string]*jsonschema.Schema{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".schema.json") {
			continue
		}
		data, err := protocolFiles.ReadFile("protocol/" + entry.Name())
		if err != nil {
			return nil, err
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			return nil, err
		}
		registry[schema.ID] = &schema
		names[strings.TrimSuffix(entry.Name(), ".schema.json")] = &schema
	}
	contract := &familyProtocolContract{
		schemas: make(map[string]*jsonschema.Resolved, len(names)),
		types:   map[string]familyCatalogType{},
	}
	for name, schema := range names {
		resolved, err := schema.Resolve(&jsonschema.ResolveOptions{
			Loader: func(uri *url.URL) (*jsonschema.Schema, error) {
				schema, ok := registry[uri.String()]
				if !ok {
					return nil, errors.New("protocol reference is not embedded")
				}
				return schema, nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", name, err)
		}
		contract.schemas[name] = resolved
	}
	data, err := protocolFiles.ReadFile("protocol/catalog.json")
	if err != nil {
		return nil, err
	}
	var catalog struct{ Types []familyCatalogType }
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	if len(catalog.Types) == 0 {
		return nil, errors.New("empty protocol catalog")
	}
	for _, item := range catalog.Types {
		contract.types[item.Identifier] = item
	}
	return contract, nil
})

func validateFamilyLine(data []byte, value any) error {
	var name string
	switch value.(type) {
	case *BatchHeader:
		return nil
	case *Sample:
		name = "sample"
	case *Deletion:
		name = "deletion"
	case *RouteLine:
		name = "route"
	case *SeriesLine:
		name = "series"
	case *AggregateLine:
		name = "aggregate"
	case *ActivitySummaryLine:
		name = "activity-summary"
	case *ProfileLine:
		name = "profile"
	default:
		return errFamilyRecord
	}
	contract, err := familyProtocol()
	if err != nil {
		return errFamilyRecord
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return errFamilyRecord
	}
	if err := contract.schemas[name].Validate(raw); err != nil {
		return errFamilyRecord
	}
	switch item := value.(type) {
	case *Sample:
		definition, ok := contract.types[item.Type]
		if !ok || definition.Kind != item.Kind {
			return errFamilyRecord
		}
		if item.Kind == "quantity" && (item.Value == nil || !sameFamilyUnit(item.Unit, definition.Unit)) {
			return errFamilyRecord
		}
		for _, fact := range []*string{item.SourceName, item.SourceBundleID, item.SourceVersion, item.Device} {
			if fact != nil && utf8.RuneCountInString(*fact) > 256 {
				return errFamilyRecord
			}
		}
		end := item.End
		if end == 0 {
			end = item.Start
		}
		if !familyTimeAgrees(item.Start, item.StartContext) || !familyTimeAgrees(end, item.EndContext) {
			return errFamilyRecord
		}
		if item.Workout != nil {
			for _, event := range item.Workout.Events {
				if !familySpanAgrees(event.Start, event.End, event.StartContext, event.EndContext) {
					return errFamilyRecord
				}
			}
			for _, activity := range item.Workout.Activities {
				if !familySpanAgrees(activity.Start, activity.End, activity.StartContext, activity.EndContext) {
					return errFamilyRecord
				}
			}
		}
		if dose := item.MedicationDose; dose != nil && dose.ScheduledAtContext != nil {
			if dose.ScheduledAt == nil || !familyTimeAgrees(*dose.ScheduledAt, dose.ScheduledAtContext) {
				return errFamilyRecord
			}
		}
	case *Deletion:
		if _, ok := contract.types[item.Deleted.Type]; !ok {
			return errFamilyRecord
		}
	case *RouteLine:
		for _, point := range item.Route.Points {
			if !familyTimeAgrees(point.T, point.TemporalContext) {
				return errFamilyRecord
			}
		}
	case *SeriesLine:
		definition, ok := contract.types[item.Series.Type]
		if !ok || definition.Kind != "quantity" || !sameFamilyUnit(item.Series.Unit, definition.Unit) {
			return errFamilyRecord
		}
		for _, point := range item.Series.Points {
			if !familyTimeAgrees(point.T, point.TemporalContext) {
				return errFamilyRecord
			}
		}
	case *AggregateLine:
		aggregate := item.Aggregate
		definition, ok := contract.types[aggregate.Type]
		if !ok || definition.Kind != "quantity" || !sameFamilyUnit(aggregate.Unit, definition.Unit) ||
			!slices.Contains(definition.AllowedAggregateFunctions, aggregate.Func) {
			return errFamilyRecord
		}
		if !familyTimeAgrees(aggregate.BucketStart, aggregate.BucketStartContext) ||
			!familyTimeAgrees(aggregate.BucketEnd, aggregate.BucketEndContext) {
			return errFamilyRecord
		}
	case *ActivitySummaryLine:
		if !familyTimeAgrees(item.ActivitySummary.Date, item.ActivitySummary.TemporalContext) {
			return errFamilyRecord
		}
	}
	return nil
}

func sameFamilyUnit(actual, canonical *string) bool {
	return actual != nil && canonical != nil && *actual == *canonical
}

func familySpanAgrees(start float64, end *float64, startContext, endContext *TemporalContext) bool {
	if !familyTimeAgrees(start, startContext) {
		return false
	}
	if endContext == nil {
		return true
	}
	return end != nil && familyTimeAgrees(*end, endContext)
}

func familyTimeAgrees(instant float64, context *TemporalContext) bool {
	if context == nil {
		return true
	}
	known := context.Source == "healthkit_metadata" && context.Confidence == "recorded" ||
		context.Source == "device_current" && context.Confidence == "inferred"
	if !known || context.TimeZoneID == "Local" {
		return true
	}
	location, err := time.LoadLocation(context.TimeZoneID)
	// Unknown source zones remain unverifiable; never substitute the host zone.
	if err != nil {
		return true
	}
	_, offset := msToTime(instant).In(location).Zone()
	return offset == int(context.UTCOffsetSeconds)
}
