package store

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
)

// scrubTerminalCredentialsSQL is built once from model.SecretParamPaths, the
// single definition of which params fields are credentials.
var scrubTerminalCredentialsSQL = mustBuildScrubSQL(model.SecretParamPaths())

// ScrubTerminalOperationCredentials removes every credential field
// (model.SecretParamPaths) from the stored params of each operation in a
// terminal status (opstate.TerminalStatuses), in one statement, and returns
// how many rows it rewrote. Active ops keep their credentials: the worker
// that executes them reads params over the op stream, including on a
// reconnect resync. A terminal op never needs them again (retries create a
// new op and resolve credentials afresh).
//
// It writes only rows that still hold a credential key, so a run with
// nothing to do changes nothing; it does read the params of every terminal
// row to decide that (no index), which is milliseconds at thousands of ops.
// It touches no column but params: no version bump, no op event. It is
// idempotent, and safe to run concurrently with itself and with op
// transitions, because nothing else writes a terminal row.
//
// Global runs it at startup and on every op-sweep tick, so a terminal op
// keeps its credentials in the table for at most one sweep interval (a
// minute) while global is up, or until global's next start if it is down.
func (s *Store) ScrubTerminalOperationCredentials() (int64, error) {
	res := s.db.Exec(scrubTerminalCredentialsSQL, opstate.TerminalStatuses)
	if res.Error != nil {
		return 0, fmt.Errorf("scrub credentials from terminal operations: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// paramKeyRe is the JSON key shape the generated SQL embeds unquoted-safe:
// snake_case identifiers only, so no key can carry a quote, a brace, or a
// gorm placeholder.
var paramKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// mustBuildScrubSQL renders the scrub UPDATE. Each credential path is one
// step of a LATERAL chain over the row's params (step N reads step N-1's
// result), so every path is applied without nesting the whole document
// expression inside itself. Every step leaves the document unchanged when
// the path's shape doesn't match (an array where an object belongs, say),
// so no row the typed union never produces can fail the statement and keep
// every other row's credentials in place. The outer WHERE writes only rows
// the chain actually changed.
func mustBuildScrubSQL(paths []model.SecretParamPath) string {
	if len(paths) == 0 {
		panic("store: no secret param paths; the scrub would do nothing")
	}
	var laterals []string
	prev := "op.params"
	for i, p := range paths {
		for _, k := range p {
			if k != model.EachElement && !paramKeyRe.MatchString(k) {
				panic(fmt.Sprintf("store: secret param key %q is not a snake_case identifier; the scrub SQL embeds keys verbatim", k))
			}
		}
		alias := fmt.Sprintf("step%d", i+1)
		laterals = append(laterals, fmt.Sprintf("CROSS JOIN LATERAL (SELECT %s AS params) AS %s", scrubStep(prev, p, 0), alias))
		prev = alias + ".params"
	}
	return fmt.Sprintf(`UPDATE operations AS o SET params = s.params
FROM (SELECT op.id, %s AS params FROM operations AS op
%s
WHERE op.status IN ? AND jsonb_typeof(op.params) = 'object') AS s
WHERE o.id = s.id AND o.params IS DISTINCT FROM s.params`, prev, strings.Join(laterals, "\n"))
}

// scrubStep returns a jsonb expression for doc (a SQL expression) with the
// credential at path removed, or doc unchanged when path's shape doesn't
// match. depth names the array-element aliases of nested EachElement steps.
func scrubStep(doc string, path model.SecretParamPath, depth int) string {
	i := slices.Index(path, model.EachElement)
	if i < 0 {
		// #- errors when it must step into an array with a key, so require
		// the credential's parent to be an object; #> yields NULL (not an
		// object) when any ancestor is not traversable by key.
		parent := doc
		if len(path) > 1 {
			parent = fmt.Sprintf("%s #> '%s'", doc, pgTextArray(path[:len(path)-1]))
		}
		return fmt.Sprintf("CASE WHEN jsonb_typeof(%s) = 'object' THEN %s #- '%s' ELSE %s END",
			parent, doc, pgTextArray(path), doc)
	}
	arr, rest := pgTextArray(path[:i]), path[i+1:]
	elem, ord := fmt.Sprintf("e%d", depth), fmt.Sprintf("n%d", depth)
	return fmt.Sprintf("CASE WHEN jsonb_typeof(%[1]s #> '%[2]s') = 'array' THEN jsonb_set(%[1]s, '%[2]s', "+
		"(SELECT coalesce(jsonb_agg(%[3]s ORDER BY %[5]s), '[]'::jsonb) FROM jsonb_array_elements(%[1]s #> '%[2]s') WITH ORDINALITY AS a%[6]d(%[4]s, %[5]s))) "+
		"ELSE %[1]s END",
		doc, arr, scrubStep(elem, rest, depth+1), elem, ord, depth)
}

// pgTextArray renders keys as a Postgres text[] literal body for #>, #- and
// jsonb_set, e.g. {bmc_runtime,password}. Keys are paramKeyRe-checked.
func pgTextArray(keys []string) string {
	return "{" + strings.Join(keys, ",") + "}"
}
