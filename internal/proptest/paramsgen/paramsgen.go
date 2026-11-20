// Package paramsgen is test-only machinery for the credential-handling
// property tests (internal/model, internal/store, internal/api). It holds
//
//   - a small JSON AST (Node) that, unlike encoding/json values, keeps
//     duplicate keys, key order and key spelling, so a generated document
//     can be rendered in shapes the typed producers never emit;
//   - a rapid generator for op-params documents that plants a unique canary
//     string at every credential position and a unique keep marker at
//     non-credential positions;
//   - an independent reference model of the two documented credential
//     transforms: the operator-facing redaction (model.RedactParams) and the
//     terminal-op scrub (store.ScrubTerminalOperationCredentials).
//
// The reference model deliberately imports nothing from internal/model or
// internal/store: the secret paths are restated here from the OpParams doc
// comment, and the walkers are written from the doc comments of RedactParams
// and mustBuildScrubSQL, not from their code. A drift between this list and
// model.SecretParamPaths is itself a test failure (see the model property
// test).
package paramsgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"pgregory.net/rapid"
)

// Marker is the documented redaction placeholder, restated.
const Marker = "[redacted]"

// Each is the path segment standing for "every element of this array".
const Each = "[]"

// SecretPaths restates every credential location in an op's params, from
// the secret:"true" tags documented on model.OpParams.
var SecretPaths = [][]string{
	{"bmc_runtime", "password"},
	{"pdu_runtime", "outlets", Each, "password"},
	{"amt_runtime", "password"},
	{"tailscale_runtime", "auth_key"},
	{"monitor_runtime", "password"},
	{"monitor_runtime", "unconfirmed", Each, "password"},
}

// Kind is a JSON value kind.
type Kind int

const (
	Null Kind = iota
	Bool
	Num
	Str
	Arr
	Obj
)

// Member is one key/value pair of an object. An object's Members may repeat
// a key.
type Member struct {
	Key string
	Val *Node
}

// Node is a JSON value.
type Node struct {
	Kind    Kind
	B       bool
	N       string // number literal, rendered verbatim
	S       string
	Elems   []*Node
	Members []Member
}

func NullN() *Node          { return &Node{Kind: Null} }
func BoolN(b bool) *Node    { return &Node{Kind: Bool, B: b} }
func NumN(lit string) *Node { return &Node{Kind: Num, N: lit} }
func StrN(s string) *Node   { return &Node{Kind: Str, S: s} }
func ArrN(e ...*Node) *Node { return &Node{Kind: Arr, Elems: e} }
func ObjN(m ...Member) *Node {
	return &Node{Kind: Obj, Members: m}
}
func M(k string, v *Node) Member { return Member{Key: k, Val: v} }

// Clone deep-copies n.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := &Node{Kind: n.Kind, B: n.B, N: n.N, S: n.S}
	for _, e := range n.Elems {
		c.Elems = append(c.Elems, e.Clone())
	}
	for _, m := range n.Members {
		c.Members = append(c.Members, Member{Key: m.Key, Val: m.Val.Clone()})
	}
	return c
}

// Style controls rendering choices that must not change a document's
// meaning: whitespace and whether object keys are written with \u escapes.
type Style struct {
	Spaced     bool
	EscapeKeys bool
}

// Render writes n as JSON text. Duplicate keys and member order are kept.
func Render(n *Node, st Style) []byte {
	var b bytes.Buffer
	render(&b, n, st)
	return b.Bytes()
}

func render(b *bytes.Buffer, n *Node, st Style) {
	sp := func() {
		if st.Spaced {
			b.WriteString(" \n\t")
		}
	}
	switch n.Kind {
	case Null:
		b.WriteString("null")
	case Bool:
		b.WriteString(strconv.FormatBool(n.B))
	case Num:
		b.WriteString(n.N)
	case Str:
		b.Write(quote(n.S))
	case Arr:
		b.WriteByte('[')
		for i, e := range n.Elems {
			if i > 0 {
				b.WriteByte(',')
			}
			sp()
			render(b, e, st)
		}
		sp()
		b.WriteByte(']')
	case Obj:
		b.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			sp()
			if st.EscapeKeys {
				b.Write(escapeAll(m.Key))
			} else {
				b.Write(quote(m.Key))
			}
			sp()
			b.WriteByte(':')
			sp()
			render(b, m.Val, st)
		}
		sp()
		b.WriteByte('}')
	}
}

func quote(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// escapeAll writes s as a JSON string with its first rune as a \uXXXX
// escape, so the key's bytes differ from its decoded spelling.
func escapeAll(s string) []byte {
	r := []rune(s)
	if len(r) == 0 || r[0] > 0x7f {
		return quote(s)
	}
	rest := quote(string(r[1:]))
	return []byte(fmt.Sprintf(`"\u%04x%s`, r[0], rest[1:]))
}

// Parse reads JSON text into a Node, keeping duplicate keys and order.
func Parse(raw []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return n, nil
}

func parseValue(dec *json.Decoder) (*Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case nil:
		return NullN(), nil
	case bool:
		return BoolN(v), nil
	case json.Number:
		return NumN(v.String()), nil
	case string:
		return StrN(v), nil
	case json.Delim:
		switch v {
		case '[':
			n := &Node{Kind: Arr}
			for dec.More() {
				e, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.Elems = append(n.Elems, e)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '{':
			n := &Node{Kind: Obj}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T", kt)
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.Members = append(n.Members, Member{Key: k, Val: val})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// Normalize returns n as a JSON object store sees it: for a repeated key
// only the last member survives (encoding/json maps and Postgres jsonb
// agree on this).
func Normalize(n *Node) *Node {
	c := &Node{Kind: n.Kind, B: n.B, N: n.N, S: n.S}
	for _, e := range n.Elems {
		c.Elems = append(c.Elems, Normalize(e))
	}
	last := map[string]int{}
	for i, m := range n.Members {
		last[m.Key] = i
	}
	for i, m := range n.Members {
		if last[m.Key] == i {
			c.Members = append(c.Members, Member{Key: m.Key, Val: Normalize(m.Val)})
		}
	}
	return c
}

// Canon renders the normalized form of n with sorted keys and numbers
// reduced to their float64 value, so two documents with the same meaning
// compare equal as strings.
func Canon(n *Node) string {
	var b strings.Builder
	canon(&b, Normalize(n))
	return b.String()
}

func canon(b *strings.Builder, n *Node) {
	switch n.Kind {
	case Null:
		b.WriteString("null")
	case Bool:
		b.WriteString(strconv.FormatBool(n.B))
	case Num:
		f, err := strconv.ParseFloat(n.N, 64)
		if err != nil {
			b.WriteString("num(" + n.N + ")")
			return
		}
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	case Str:
		b.Write(quote(n.S))
	case Arr:
		b.WriteByte('[')
		for i, e := range n.Elems {
			if i > 0 {
				b.WriteByte(',')
			}
			canon(b, e)
		}
		b.WriteByte(']')
	case Obj:
		ms := append([]Member(nil), n.Members...)
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].Key < ms[j].Key })
		b.WriteByte('{')
		for i, m := range ms {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(quote(m.Key))
			b.WriteByte(':')
			canon(b, m.Val)
		}
		b.WriteByte('}')
	}
}

// Strings returns every string in n: object keys and string values, at any
// depth, duplicates included.
func Strings(n *Node) []string {
	var out []string
	var walk func(*Node)
	walk = func(n *Node) {
		switch n.Kind {
		case Str:
			out = append(out, n.S)
		case Arr:
			for _, e := range n.Elems {
				walk(e)
			}
		case Obj:
			for _, m := range n.Members {
				out = append(out, m.Key)
				walk(m.Val)
			}
		default:
		}
	}
	walk(n)
	return out
}

// RefRedact is the reference model of the operator-facing redaction, written
// from RedactParams' doc comment:
//
//   - every non-empty credential at a secret path becomes the marker;
//   - an absent key, a JSON null, and an empty-string credential are left;
//   - a credential path whose shape cannot be walked (anything but an object
//     where a key is looked up, anything but an array where every element is
//     walked) is withheld whole as the marker;
//   - everything else keeps its value.
//
// It redacts under every member that carries a path key, so a document with
// a repeated key has no member left showing a credential. RedactParams
// instead drops every member of a repeated path key but the last; the two
// agree when compared through Canon, which keeps only the last member.
func RefRedact(n *Node) *Node {
	out := n.Clone()
	for _, p := range SecretPaths {
		out = refRedactAt(out, p)
	}
	return out
}

func refRedactAt(n *Node, path []string) *Node {
	if n.Kind == Null {
		return n
	}
	if len(path) == 0 {
		if n.Kind == Str && n.S == "" {
			return n
		}
		return StrN(Marker)
	}
	if path[0] == Each {
		if n.Kind != Arr {
			return StrN(Marker)
		}
		for i := range n.Elems {
			n.Elems[i] = refRedactAt(n.Elems[i], path[1:])
		}
		return n
	}
	if n.Kind != Obj {
		return StrN(Marker)
	}
	for i := range n.Members {
		if n.Members[i].Key == path[0] {
			n.Members[i].Val = refRedactAt(n.Members[i].Val, path[1:])
		}
	}
	return n
}

// RefScrub is the reference model of the terminal-op scrub, written from the
// doc comments of ScrubTerminalOperationCredentials and mustBuildScrubSQL:
// every credential key is removed, whatever its value, wherever its ancestors
// have the typed union's shape (objects, and an array for the outlet list);
// where the shape does not match, that part of the document is unchanged. A
// params value that is not a JSON object is not touched at all. The input is
// the normalized document (what jsonb stores).
func RefScrub(n *Node) *Node {
	out := Normalize(n)
	if out.Kind != Obj {
		return out
	}
	for _, p := range SecretPaths {
		refScrubAt(out, p)
	}
	return out
}

func refScrubAt(n *Node, path []string) {
	if path[0] == Each {
		if n.Kind != Arr {
			return
		}
		for _, e := range n.Elems {
			refScrubAt(e, path[1:])
		}
		return
	}
	if n.Kind != Obj {
		return
	}
	if len(path) == 1 {
		kept := n.Members[:0]
		for _, m := range n.Members {
			if m.Key != path[0] {
				kept = append(kept, m)
			}
		}
		n.Members = kept
		return
	}
	for _, m := range n.Members {
		if m.Key == path[0] {
			refScrubAt(m.Val, path[1:])
		}
	}
}

// Options select which document classes Draw produces.
type Options struct {
	// DupKeys lets an object repeat a key (valid JSON; the typed producers
	// never emit it and jsonb collapses it).
	DupKeys bool
	// CaseKeys lets a known key be written with different letter case
	// (encoding/json matches struct fields case-insensitively).
	CaseKeys bool
	// Malformed lets a credential path hold the wrong JSON kind (a string
	// where the runtime object belongs, a nested array in the outlet list).
	Malformed bool
	// NonObjectRoot lets the whole params value be something other than an
	// object.
	NonObjectRoot bool
	// TypedValues restricts credential values to what the typed union can
	// hold (a string, possibly empty, or an absent key) and keeps runtime
	// blocks non-null, so most documents decode.
	TypedValues bool
}

// Doc is one generated params document.
type Doc struct {
	Root *Node
	// Canaries are the strings planted only where a credential (or a value
	// withheld with one) sits. None may appear in a redacted view.
	Canaries []string
	// WalkableCanaries is the subset planted at a secret path whose
	// ancestors all have the typed shape: the scrub must remove these.
	WalkableCanaries []string
	// Keeps are the strings planted at non-credential positions.
	Keeps []string
	Style Style
	// Features counts generated shapes, for coverage reporting.
	Features map[string]int
}

// Raw renders the document.
func (d *Doc) Raw() []byte { return Render(d.Root, d.Style) }

type gen struct {
	t    *rapid.T
	o    Options
	d    *Doc
	seq  int
	deep int // >0 while under a shape the walk cannot enter
}

func (g *gen) feat(name string) { g.d.Features[name]++ }

func (g *gen) canary() string {
	g.seq++
	c := fmt.Sprintf("CNR%dQZ", g.seq)
	g.d.Canaries = append(g.d.Canaries, c)
	if g.deep == 0 {
		g.d.WalkableCanaries = append(g.d.WalkableCanaries, c)
	}
	return c
}

func (g *gen) keep() string {
	g.seq++
	k := fmt.Sprintf("KEEP%dQZ", g.seq)
	g.d.Keeps = append(g.d.Keeps, k)
	return k
}

// decorate wraps a marker in characters that JSON, HTML-escaping encoders
// and SQL treat specially, so an escaping bug cannot hide or split it.
func (g *gen) decorate(s string) string {
	pre := rapid.SampledFrom([]string{"", "", "", `"`, `\`, "<", "&", "'", "é", "\n", "{", "$1", "?", "[redacted]"}).Draw(g.t, "pre")
	post := rapid.SampledFrom([]string{"", "", "", `"`, `\`, ">", "'", "}", " ", "::jsonb"}).Draw(g.t, "post")
	return pre + s + post
}

func (g *gen) key(k string) string {
	if g.o.CaseKeys && rapid.IntRange(0, 5).Draw(g.t, "caseKey") == 0 {
		g.feat("case_variant_key")
		switch rapid.IntRange(0, 2).Draw(g.t, "caseKind") {
		case 0:
			return strings.ToUpper(k)
		case 1:
			return strings.ToUpper(k[:1]) + k[1:]
		default:
			return k[:len(k)-1] + strings.ToUpper(k[len(k)-1:])
		}
	}
	return k
}

// plain returns an arbitrary non-credential value carrying keep markers.
func (g *gen) plain(depth int) *Node {
	max := 5
	if depth >= 2 {
		max = 3
	}
	switch rapid.IntRange(0, max).Draw(g.t, "plainKind") {
	case 0:
		return StrN(g.decorate(g.keep()))
	case 1:
		return NumN(rapid.SampledFrom([]string{"0", "1", "17", "623", "1.5", "-3"}).Draw(g.t, "num"))
	case 2:
		return BoolN(rapid.Bool().Draw(g.t, "bool"))
	case 3:
		return NullN()
	case 4:
		n := rapid.IntRange(0, 3).Draw(g.t, "arrLen")
		a := &Node{Kind: Arr}
		for range n {
			a.Elems = append(a.Elems, g.plain(depth+1))
		}
		return a
	default:
		n := rapid.IntRange(0, 3).Draw(g.t, "objLen")
		o := &Node{Kind: Obj}
		for range n {
			// Credential-looking key names at non-credential paths must
			// survive: the redaction addresses paths, not names.
			k := rapid.SampledFrom([]string{"password", "auth_key", "note", "outlets", "ipmi_cipher", "bmc_runtime"}).Draw(g.t, "plainKey")
			if hasKey(o, k) {
				continue
			}
			o.Members = append(o.Members, M(k, g.plain(depth+1)))
		}
		return o
	}
}

func hasKey(o *Node, k string) bool {
	for _, m := range o.Members {
		if m.Key == k {
			return true
		}
	}
	return false
}

// secretValue returns the value planted at a credential key, and whether the
// key should be present at all.
func (g *gen) secretValue() (*Node, bool) {
	lo := 0
	if g.o.TypedValues {
		lo = 7
		switch rapid.IntRange(0, 7).Draw(g.t, "typedSecretKind") {
		case 0:
			g.feat("secret_absent")
			return nil, false
		case 1:
			g.feat("secret_empty")
			return StrN(""), true
		default:
		}
	}
	switch rapid.IntRange(lo, 13).Draw(g.t, "secretKind") {
	case 0:
		g.feat("secret_absent")
		return nil, false
	case 1:
		g.feat("secret_empty")
		return StrN(""), true
	case 2:
		g.feat("secret_null")
		return NullN(), true
	case 3:
		g.feat("secret_number")
		return NumN("12345"), true
	case 4:
		g.feat("secret_object")
		return ObjN(M("v", StrN(g.canary()))), true
	case 5:
		g.feat("secret_array")
		return ArrN(StrN(g.canary()), StrN(g.canary())), true
	case 6:
		g.feat("secret_bool")
		return BoolN(true), true
	default:
		g.feat("secret_string")
		return StrN(g.decorate(g.canary())), true
	}
}

// wrongShape returns a value of a kind the walk cannot enter (not an object
// when wantObj, not an array otherwise), carrying canaries: the redaction
// withholds it whole.
func (g *gen) wrongShape(wantObj bool, inner func() *Node) *Node {
	g.feat("malformed_shape")
	g.deep++
	defer func() { g.deep-- }()
	switch rapid.IntRange(0, 4).Draw(g.t, "wrongKind") {
	case 0:
		return StrN(g.decorate(g.canary()))
	case 1:
		return NumN("7")
	case 2:
		return BoolN(false)
	case 3:
		if wantObj {
			return ArrN(inner())
		}
		return inner() // an object where an array belongs
	default:
		if wantObj {
			return ArrN(StrN(g.canary()), inner())
		}
		return StrN(g.canary())
	}
}

// object builds an object from field builders, optionally repeating one key
// (DupKeys) with freshly generated content.
func (g *gen) object(fields []func() (string, *Node, bool)) *Node {
	o := &Node{Kind: Obj}
	for _, f := range fields {
		k, v, ok := f()
		if ok {
			o.Members = append(o.Members, M(k, v))
		}
	}
	if g.o.DupKeys && len(fields) > 0 && rapid.IntRange(0, 3).Draw(g.t, "dup") == 0 {
		f := fields[rapid.IntRange(0, len(fields)-1).Draw(g.t, "dupField")]
		k, v, ok := f()
		if ok {
			g.feat("dup_key")
			at := rapid.IntRange(0, len(o.Members)).Draw(g.t, "dupAt")
			o.Members = append(o.Members[:at], append([]Member{M(k, v)}, o.Members[at:]...)...)
		}
	}
	rapid.Just(0).Draw(g.t, "shuffleAnchor")
	return o
}

func (g *gen) field(k string, v func() *Node) func() (string, *Node, bool) {
	return func() (string, *Node, bool) {
		if rapid.IntRange(0, 4).Draw(g.t, "omit:"+k) == 0 {
			return "", nil, false
		}
		return g.key(k), v(), true
	}
}

func (g *gen) secretField(k string) func() (string, *Node, bool) {
	return func() (string, *Node, bool) {
		v, ok := g.secretValue()
		return g.key(k), v, ok
	}
}

func (g *gen) keepStr() *Node { return StrN(g.decorate(g.keep())) }

func (g *gen) runtime(name string, build func() *Node) func() (string, *Node, bool) {
	return func() (string, *Node, bool) {
		if rapid.IntRange(0, 2).Draw(g.t, "omit:"+name) == 0 {
			return "", nil, false
		}
		g.feat("runtime:" + name)
		if !g.o.TypedValues && rapid.IntRange(0, 9).Draw(g.t, "nullRuntime") == 0 {
			g.feat("runtime_null")
			return g.key(name), NullN(), true
		}
		if g.o.Malformed && rapid.IntRange(0, 5).Draw(g.t, "malformed") == 0 {
			return g.key(name), g.wrongShape(true, build), true
		}
		return g.key(name), build(), true
	}
}

func (g *gen) bmc() *Node {
	return g.object([]func() (string, *Node, bool){
		g.field("expected_chassis_serial", g.keepStr),
		g.field("username", g.keepStr),
		g.secretField("password"),
		g.field("extra_config", func() *Node { return g.plain(1) }),
		g.field("note", func() *Node { return g.plain(1) }),
	})
}

func (g *gen) outlet() *Node {
	return g.object([]func() (string, *Node, bool){
		g.field("pdu_host", g.keepStr),
		g.field("outlet", func() *Node { return NumN("5") }),
		g.field("serial", g.keepStr),
		g.field("username", g.keepStr),
		g.secretField("password"),
	})
}

func (g *gen) outlets() *Node { return g.credentialArray("outlet", g.outlet) }

// credentialArray generates an array whose elements are credential-bearing
// objects (elem), with the null and malformed variants the options allow.
// label names the draws and the per-element feature.
func (g *gen) credentialArray(label string, elem func() *Node) *Node {
	if g.o.Malformed && rapid.IntRange(0, 6).Draw(g.t, label+"sMalformed") == 0 {
		return g.wrongShape(false, elem)
	}
	if !g.o.TypedValues && rapid.IntRange(0, 9).Draw(g.t, label+"sNull") == 0 {
		return NullN()
	}
	n := rapid.IntRange(0, 4).Draw(g.t, label+"Count")
	a := &Node{Kind: Arr}
	for range n {
		g.feat(label)
		switch {
		case rapid.IntRange(0, 9).Draw(g.t, label+"Null") == 0:
			a.Elems = append(a.Elems, NullN())
		case g.o.Malformed && rapid.IntRange(0, 5).Draw(g.t, label+"Malformed") == 0:
			a.Elems = append(a.Elems, g.wrongShape(true, elem))
		default:
			a.Elems = append(a.Elems, elem())
		}
	}
	return a
}

func (g *gen) pdu() *Node {
	return g.object([]func() (string, *Node, bool){
		g.field("outlets", g.outlets),
		g.field("downstream_serial", g.keepStr),
		g.field("outlet", func() *Node { return BoolN(true) }),
		// A password key directly under pdu_runtime is not a secret path.
		g.field("password", g.keepStr),
	})
}

func (g *gen) amt() *Node {
	return g.object([]func() (string, *Node, bool){
		g.field("host", g.keepStr),
		g.field("username", g.keepStr),
		g.secretField("password"),
	})
}

func (g *gen) tailscale() *Node {
	return g.object([]func() (string, *Node, bool){
		g.secretField("auth_key"),
		g.field("hostname", g.keepStr),
		g.field("tags", func() *Node { return ArrN(g.keepStr(), g.keepStr()) }),
		// password is not tailscale_runtime's credential key.
		g.field("password", g.keepStr),
	})
}

func (g *gen) unconfirmed() *Node {
	return g.object([]func() (string, *Node, bool){
		g.secretField("password"),
		// auth_key is not an unconfirmed password's credential key.
		g.field("auth_key", g.keepStr),
	})
}

func (g *gen) monitor() *Node {
	return g.object([]func() (string, *Node, bool){
		g.secretField("password"),
		g.field("unconfirmed", func() *Node { return g.credentialArray("unconfirmed", g.unconfirmed) }),
		g.field("auth_key", g.keepStr),
	})
}

// Draw generates one params document.
func Draw(t *rapid.T, o Options) *Doc {
	g := &gen{t: t, o: o, d: &Doc{Features: map[string]int{}}}
	g.d.Style = Style{
		Spaced:     rapid.IntRange(0, 3).Draw(t, "spaced") == 3,
		EscapeKeys: rapid.IntRange(0, 5).Draw(t, "escapeKeys") == 5,
	}
	if g.d.Style.EscapeKeys {
		g.feat("escaped_keys")
	}
	root := func() *Node {
		return g.object([]func() (string, *Node, bool){
			g.field("destructive", func() *Node { return BoolN(true) }),
			g.field("boot_device", g.keepStr),
			g.runtime("bmc_runtime", g.bmc),
			g.runtime("pdu_runtime", g.pdu),
			g.runtime("amt_runtime", g.amt),
			g.runtime("tailscale_runtime", g.tailscale),
			g.runtime("monitor_runtime", g.monitor),
			// Root-level credential-looking keys are not secret paths.
			g.field("password", g.keepStr),
			g.field("auth_key", g.keepStr),
			g.field("zz_unknown", func() *Node { return g.plain(0) }),
		})
	}
	if o.NonObjectRoot && rapid.IntRange(0, 9).Draw(t, "nonObjectRoot") == 0 {
		g.feat("non_object_root")
		g.d.Root = g.wrongShape(true, root)
	} else {
		g.d.Root = root()
	}
	return g.d
}
