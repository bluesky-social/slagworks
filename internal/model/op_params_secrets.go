package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// RedactedMarker replaces each non-empty credential in an operator-facing
// copy of an op's params (RedactParams).
const RedactedMarker = "[redacted]"

// EachElement is the SecretParamPath segment that stands for every element
// of a JSON array.
const EachElement = "[]"

// SecretParamPath locates one credential field inside an op's params JSON:
// the object keys from the root, with EachElement where the value at that
// point is an array whose every element is walked.
type SecretParamPath []string

// secretParamPaths is every secret:"true" field reachable from OpParams,
// derived once from the struct tags so the tags stay the single definition.
var secretParamPaths = mustCollectSecretParamPaths()

func mustCollectSecretParamPaths() []SecretParamPath {
	paths, err := collectSecretParamPaths(reflect.TypeFor[OpParams](), nil, map[reflect.Type]bool{})
	if err != nil {
		panic(err)
	}
	return paths
}

// SecretParamPaths returns the location of every credential field in an
// op's params, in OpParams field order. The returned slices are copies.
func SecretParamPaths() []SecretParamPath {
	out := make([]SecretParamPath, len(secretParamPaths))
	for i, p := range secretParamPaths {
		out[i] = slices.Clone(p)
	}
	return out
}

// collectSecretParamPaths walks t (an OpParams-shaped type) and returns the
// JSON path of each secret:"true" field under prefix. It refuses shapes the
// redaction and the stored-credential scrub cannot address, so a field that
// would escape both fails at package init instead of leaking.
func collectSecretParamPaths(t reflect.Type, prefix SecretParamPath, visiting map[reflect.Type]bool) ([]SecretParamPath, error) {
	switch t.Kind() {
	case reflect.Pointer:
		return collectSecretParamPaths(t.Elem(), prefix, visiting)
	case reflect.Slice, reflect.Array:
		return collectSecretParamPaths(t.Elem(), append(slices.Clone(prefix), EachElement), visiting)
	case reflect.Map:
		inner, err := collectSecretParamPaths(t.Elem(), nil, visiting)
		if err != nil {
			return nil, err
		}
		if len(inner) > 0 {
			return nil, fmt.Errorf("op params: %q holds a secret field inside a map value, which the redaction cannot address; use a struct or a slice", strings.Join(prefix, "."))
		}
		return nil, nil
	case reflect.Struct:
		if visiting[t] {
			return nil, fmt.Errorf("op params: %s contains itself; secret paths must be finite", t)
		}
		visiting[t] = true
		defer delete(visiting, t)
		var out []SecretParamPath
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" {
				continue
			}
			if f.Anonymous {
				return nil, fmt.Errorf("op params: %s embeds %s; embedded fields are not supported, name the field", t, f.Type)
			}
			if name == "" {
				name = f.Name
			}
			if name == EachElement {
				return nil, fmt.Errorf("op params: %s.%s uses the reserved JSON key %q", t, f.Name, EachElement)
			}
			path := append(slices.Clone(prefix), name)
			if f.Tag.Get("secret") == "true" {
				if f.Type.Kind() != reflect.String {
					return nil, fmt.Errorf("op params: secret field %s.%s is a %s; secret fields must be strings", t, f.Name, f.Type)
				}
				out = append(out, path)
				continue
			}
			inner, err := collectSecretParamPaths(f.Type, path, visiting)
			if err != nil {
				return nil, err
			}
			out = append(out, inner...)
		}
		return out, nil
	default:
		return nil, nil
	}
}

// redactedJSON returns RedactedMarker as a JSON string, freshly allocated so
// no two results share bytes.
func redactedJSON() json.RawMessage { return json.RawMessage(`"` + RedactedMarker + `"`) }

// RedactParams returns raw with every non-empty credential (each
// SecretParamPaths location) replaced by RedactedMarker, for showing an
// op's params to an operator. Every other key keeps its value: values off
// the credential paths are carried as raw JSON, never decoded. A credential
// path whose shape cannot be walked (a string where the typed union has an
// object, say) is withheld whole as the marker rather than passed through.
//
// raw is never modified. When nothing needs redacting, raw itself is
// returned, so the result must be treated as read-only like the input.
func RedactParams(raw json.RawMessage) json.RawMessage {
	out := raw
	for _, p := range secretParamPaths {
		out, _ = redactAt(out, p)
	}
	return out
}

// Redacted returns a copy of the operation whose Params are RedactParams'
// operator-facing view. The receiver is not modified.
func (o Operation) Redacted() Operation {
	o.Params = RedactParams(o.Params)
	return o
}

// redactAt returns v with the credential at path replaced by the marker,
// and whether anything changed. Absent keys and JSON null values hold no
// credential.
func redactAt(v json.RawMessage, path SecretParamPath) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return v, false
	}
	if len(path) == 0 {
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s == "" {
			return v, false
		}
		return redactedJSON(), true
	}
	if path[0] == EachElement {
		var elems []json.RawMessage
		if err := json.Unmarshal(v, &elems); err != nil {
			return redactedJSON(), true
		}
		changed := false
		for i := range elems {
			var c bool
			elems[i], c = redactAt(elems[i], path[1:])
			changed = changed || c
		}
		if !changed {
			return v, false
		}
		return marshalOrWithhold(elems), true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		return redactedJSON(), true
	}
	child, ok := obj[path[0]]
	if !ok {
		return v, false
	}
	child, changed := redactAt(child, path[1:])
	if !changed {
		return v, false
	}
	obj[path[0]] = child
	return marshalOrWithhold(obj), true
}

// marshalOrWithhold re-encodes a container of json.RawMessage values, all
// either just decoded or the marker. That cannot fail; if it somehow did,
// the whole container is withheld rather than shown.
func marshalOrWithhold(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return redactedJSON()
	}
	return b
}
