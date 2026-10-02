// Package sdk is a typed Go client for a sandboxlab control plane, and the
// domain types that client speaks.
//
// It is generated from the OpenAPI document at ../api/openapi.yaml, which is
// the source of truth for this API: the server's routes, the web console, the
// `sandbox` CLI and this package are all described by it, and the generated file
// is what keeps them from drifting.
//
// The package is deliberately outside internal/. Anything under internal/ can
// only be imported by this module, which would make it useless to the person
// most likely to want it — someone writing a program against a deployment. For
// the same reason the domain types live here rather than being re-exported:
// Go does not allow a method on an alias of another package's type, so
// Template.Validate and Sandbox.TTLRemaining have to be defined where the types
// are.
//
// Regenerating:
//
//	make sdk
//
// types.gen.go is generated and must not be edited. Everything else here is
// hand-written and is safe to change:
//
//	domain.go  methods on the generated types, the catalog, id rules
//	client.go  the client's constructor, auth, and the address helpers
//	errors.go  turning a response into an error a caller can act on
//
//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../api/openapi.yaml
package sdk
