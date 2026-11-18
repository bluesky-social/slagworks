package api

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
)

// operatorJSONSerializer is the API server's echo JSONSerializer, so every
// operator-facing JSON response (c.JSON and friends) passes through it. It
// is the one place op params are redacted for operators: any Operation in
// the response has its credentials replaced by model.RedactedMarker
// (model.RedactParams). Workers receive ops over the WS hub, which never
// touches this serializer, so they keep the real credentials.
//
// A response that holds an Operation in a shape not handled below (a struct
// field, a map value, a pointer slice, anything behind an `any`) is refused
// with an error, failing the request rather than shipping a credential;
// handle the new shape in redactOperations when that happens.
type operatorJSONSerializer struct {
	echo.DefaultJSONSerializer
}

func (s operatorJSONSerializer) Serialize(c *echo.Context, target any, indent string) error {
	redacted, err := redactOperations(target)
	if err != nil {
		return err
	}
	return s.DefaultJSONSerializer.Serialize(c, redacted, indent)
}

// redactOperations returns target with every Operation's params redacted,
// copying rather than modifying: the caller's op shares its Params bytes
// with the op_delta published to workers.
func redactOperations(target any) (any, error) {
	switch v := target.(type) {
	case model.Operation:
		return v.Redacted(), nil
	case *model.Operation:
		if v == nil {
			return v, nil
		}
		r := v.Redacted()
		return &r, nil
	case []model.Operation:
		out := make([]model.Operation, len(v))
		for i := range v {
			out[i] = v[i].Redacted()
		}
		return out, nil
	}
	if holdsOperation(reflect.ValueOf(target), 0) {
		return nil, fmt.Errorf("operator response type %T holds a model.Operation the redaction does not handle; add its shape to redactOperations", target)
	}
	return target, nil
}

var (
	operationType = reflect.TypeFor[model.Operation]()
	mayHoldByType sync.Map // reflect.Type -> bool
)

// maxHoldDepth bounds holdsOperation's walk. JSON responses nest a handful
// of levels; only a cyclic value gets this deep, and it is refused.
const maxHoldDepth = 64

// holdsOperation reports whether v contains a model.Operation anywhere
// json.Marshal would reach it. It descends only into values whose static
// type can hold one (mayHoldOperation), so a typical response type (no
// Operation, no interface inside) costs one cached lookup.
func holdsOperation(v reflect.Value, depth int) bool {
	if !v.IsValid() || !mayHoldOperation(v.Type()) {
		return false
	}
	if v.Type() == operationType || depth > maxHoldDepth {
		return true
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		return !v.IsNil() && holdsOperation(v.Elem(), depth+1)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if holdsOperation(v.Index(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if holdsOperation(it.Key(), depth+1) || holdsOperation(it.Value(), depth+1) {
				return true
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if holdsOperation(v.Field(i), depth+1) {
				return true
			}
		}
	default:
	}
	return false
}

// mayHoldOperation reports whether a value of static type t can contain a
// model.Operation: as a field or element, or behind an interface (which
// only the value can tell). Cached per type; response types are a small
// fixed set.
func mayHoldOperation(t reflect.Type) bool {
	if v, ok := mayHoldByType.Load(t); ok {
		return v.(bool)
	}
	r := typeMayHold(t, map[reflect.Type]bool{})
	mayHoldByType.Store(t, r)
	return r
}

func typeMayHold(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == operationType {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeMayHold(t.Elem(), seen)
	case reflect.Map:
		return typeMayHold(t.Key(), seen) || typeMayHold(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if typeMayHold(t.Field(i).Type, seen) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
