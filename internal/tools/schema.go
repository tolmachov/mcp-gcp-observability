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

// propPatch adds a constraint to one property of a generated input schema.
// jsonschema.For derives only types and descriptions from struct tags, so
// value constraints the SDK must enforce before the handler runs are layered
// on here.
type propPatch struct {
	property string
	apply    func(*jsonschema.Schema)
}

// enumProp restricts property to values, in the given order.
func enumProp[T ~string](property string, values []T) propPatch {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = string(v)
	}
	return propPatch{property, func(s *jsonschema.Schema) { s.Enum = enum }}
}

// nonEmptyProp rejects an empty string for a required string property.
func nonEmptyProp(property string) propPatch {
	return propPatch{property, func(s *jsonschema.Schema) { s.MinLength = new(1) }}
}

// nonNegativeValueIndex rejects a negative value_index on the profiler tools.
var nonNegativeValueIndex = propPatch{"value_index", func(s *jsonschema.Schema) { s.Minimum = new(0.0) }}

// inputSchemaFor generates a JSON schema for type T, then applies the
// property patches. Panics on schema generation failure or missing property
// (both indicate a programming error).
func inputSchemaFor[T any](patches ...propPatch) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic("inputSchemaFor: " + err.Error())
	}
	for _, p := range patches {
		prop, ok := schema.Properties[p.property]
		if !ok {
			panic("inputSchemaFor: property " + p.property + " not found in schema")
		}
		p.apply(prop)
	}
	return schema
}

// projectInputSchema generates the public schema for a project-scoped tool.
// Pinned deployments expose no project_id at all. Unpinned deployments make
// it required. Unknown fields are rejected in both modes.
func projectInputSchema[T any](policy ProjectPolicy, patches ...propPatch) *jsonschema.Schema {
	schema := inputSchemaFor[T](patches...)
	schema.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
	if policy.Pinned() {
		delete(schema.Properties, "project_id")
		return schema
	}
	prop, ok := schema.Properties["project_id"]
	if !ok {
		panic("projectInputSchema: project_id not found")
	}
	prop.Description = "GCP project ID; required by this unpinned deployment"
	prop.Pattern = projectIDPattern.String()
	// ProjectInput.ProjectID is omitempty, so project_id is never already required.
	schema.Required = append(schema.Required, "project_id")
	return schema
}

// Enum value sets owned by the tool layer. Domain enums (severities, profile
// types, error windows) live in gcpdata next to the code that interprets them.
var (
	httpMethods    = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	sortOrders     = []string{"asc", "desc"}
	profileSortBys = []string{"self", "cumulative"}
	traceOrderBys  = []string{"trace_id", "trace_id desc", "name", "name desc", "duration", "duration desc", "start", "start desc"}
	traceViews     = []string{"MINIMAL", "ROOTSPAN", "COMPLETE"}
)
