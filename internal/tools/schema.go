package tools

import (
	"github.com/google/jsonschema-go/jsonschema"
)

// outputSchemaFor generates a JSON schema for type T to use as a tool OutputSchema.
// Panics if schema generation fails (programming error).
// Do not use for types that contain self-referential fields — use a hand-written schema instead.
func outputSchemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic("outputSchemaFor: " + err.Error())
	}
	return schema
}

// enumPatch injects an enum constraint into a generated JSON schema property.
type enumPatch struct {
	property string
	values   []any
}

// inputSchemaWithEnums generates a JSON schema for type T, then injects
// enum constraints for the specified properties. Panics on schema generation
// failure or missing property (both indicate a programming error).
func inputSchemaWithEnums[T any](patches ...enumPatch) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic("inputSchemaWithEnums: " + err.Error())
	}
	for _, p := range patches {
		prop, ok := schema.Properties[p.property]
		if !ok {
			panic("inputSchemaWithEnums: property " + p.property + " not found in schema")
		}
		prop.Enum = p.values
	}
	return schema
}

// toAny converts a string slice to []any for use with jsonschema.Schema.Enum.
func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Shared enum value sets used across multiple tool input schemas.
var (
	enumSortOrder    = toAny([]string{"asc", "desc"})
	enumHTTPMethod   = toAny([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"})
	enumSeverity     = toAny([]string{"DEFAULT", "DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"})
	enumWindow       = toAny([]string{"15m", "30m", "1h", "3h", "6h", "24h"})
	enumBaselineMode = toAny([]string{"prev_window", "same_weekday_hour", "pre_event"})
	enumProfileType  = toAny([]string{"CPU", "WALL", "HEAP", "THREADS", "CONTENTION", "PEAK_HEAP", "HEAP_ALLOC"})
	enumSortBy       = toAny([]string{"self", "cumulative"})
	enumTraceOrderBy = toAny([]string{"trace_id", "trace_id desc", "name", "name desc", "duration", "duration desc", "start", "start desc"})
	enumTraceView    = toAny([]string{"MINIMAL", "ROOTSPAN", "COMPLETE"})
)
