package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
)

// ErrOperationBinding marks a permanent identity/provenance rejection. Callers
// may drop the report instead of retrying; database transport errors remain
// unwrapped and retryable.
var ErrOperationBinding = errors.New("operation binding rejected")

func bindingErrorf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrOperationBinding)
}

// ValidateOperationBinding verifies that an observation is tied to the exact
// operation and machine it claims. alreadyCompleted is true for an accepted
// replay of a completed operation. Commission replays additionally require
// the stored result to be semantically identical; deploy_complete has
// idempotent side effects and may race with the later phone-home completion.
func (s *Store) ValidateOperationBinding(opID, machineID uuid.UUID, wantType model.OperationType, result json.RawMessage) (alreadyCompleted bool, err error) {
	if opID == uuid.Nil || machineID == uuid.Nil {
		return false, bindingErrorf("operation and machine IDs are required")
	}
	op, err := s.GetOperation(opID)
	if err != nil {
		return false, err
	}
	return validateOperationBinding(op, machineID, wantType, result)
}

func validateOperationBinding(op *model.Operation, machineID uuid.UUID, wantType model.OperationType, result json.RawMessage) (bool, error) {
	if op.MachineID != machineID {
		return false, bindingErrorf("operation %s belongs to machine %s, not %s", op.ID, op.MachineID, machineID)
	}
	if op.Type != wantType {
		return false, bindingErrorf("operation %s has type %s, want %s", op.ID, op.Type, wantType)
	}
	switch op.Status {
	case model.OpPending, model.OpClaimed, model.OpRunning:
		return false, nil
	case model.OpCompleted:
		if wantType == model.OpDeploy {
			// deploy_complete is sent before the installed OS phones home, but
			// observations and phone-home op updates are acknowledged on
			// independent paths. A transient observation failure can therefore
			// be retried after phone-home has completed this exact deploy op.
			// The handler's PG/AWS writes are idempotent, so accept that replay;
			// the machine and operation checks above still reject cross-host,
			// wrong-type, and fabricated-operation reports.
			return true, nil
		}
		if wantType == model.OpCommission && len(op.Result) > 0 && jsonSemanticallyEqual(op.Result, result) {
			return true, nil
		}
		return false, bindingErrorf("operation %s is already completed with a different result", op.ID)
	default:
		return false, bindingErrorf("operation %s is terminal (%s)", op.ID, op.Status)
	}
}

// CommitCommissionResult atomically stores inventory, completes the exact
// commission operation, and advances the same operation's machine. It never
// searches by machine/type, so another operation cannot be completed as a
// fallback if opID is stale or mismatched.
func (s *Store) CommitCommissionResult(opID, machineID uuid.UUID, data json.RawMessage) (*OpOutcome, bool, error) {
	now := time.Now().UTC()
	var outcome *OpOutcome
	var alreadyCompleted bool
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var op model.Operation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&op, "id = ?", opID).Error; err != nil {
			return err
		}
		already, err := validateOperationBinding(&op, machineID, model.OpCommission, data)
		if err != nil {
			return err
		}
		if already {
			alreadyCompleted = true
			return nil
		}

		res := tx.Model(&model.Machine{}).Where("id = ?", machineID).Updates(map[string]any{
			"commission_data": data,
			"commissioned_at": now,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return bindingErrorf("machine %s not found", machineID)
		}

		res = tx.Model(&op).Where("status IN ?", opstate.ActiveStatuses).Updates(map[string]any{
			"status":       model.OpCompleted,
			"completed_at": now,
			"result":       data,
			"version":      gorm.Expr("version + 1"),
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return bindingErrorf("operation %s ceased to be active", opID)
		}
		if err := emitTerminalEvent(tx, &op, model.OpCompleted, "", now); err != nil {
			return fmt.Errorf("emit terminal event: %w", err)
		}
		if err := setMachineState(tx, machineID, model.MachineStateCommissioned); err != nil {
			return err
		}
		post, err := refetchOp(tx, opID)
		if err != nil {
			return err
		}
		outcome = &OpOutcome{Op: *post, MachineState: model.MachineStateCommissioned}
		return nil
	})
	return outcome, alreadyCompleted, err
}

// jsonSemanticallyEqual reports whether a and b encode the same JSON value.
// One side is usually read back from a jsonb column, which discards
// whitespace, reorders object keys and re-spells numbers (1e3 becomes 1000, -0
// becomes 0), so a byte-identical redelivery must still compare equal to it.
// Numbers therefore compare by exact decimal value, never through float64,
// which would also collapse distinct large integers.
func jsonSemanticallyEqual(a, b []byte) bool {
	var av, bv any
	ad := json.NewDecoder(bytes.NewReader(a))
	ad.UseNumber()
	bd := json.NewDecoder(bytes.NewReader(b))
	bd.UseNumber()
	if ad.Decode(&av) != nil || bd.Decode(&bv) != nil {
		return false
	}
	return reflect.DeepEqual(decimalNumbers(av), decimalNumbers(bv))
}

// decimalNumbers replaces, in place, every json.Number in a decoded document
// with its decimalValue and returns the document.
func decimalNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = decimalNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = decimalNumbers(e)
		}
	case json.Number:
		if d, ok := parseDecimal(x); ok {
			return d
		}
	}
	return v
}

// decimalValue is the exact value of a JSON number: (-1 if neg) * digits *
// 10^exp, where digits has no leading or trailing zeros. Every spelling of
// the same value maps to the same decimalValue. Zero is the zero
// decimalValue, so -0 equals 0, as it does in jsonb.
type decimalValue struct {
	neg    bool
	digits string
	exp    int64
}

// parseDecimal reduces n, a number the JSON decoder has already validated,
// to its decimalValue. It works on the spelling, so the cost is linear in its
// length however large the exponent is. ok is false when the exponent does
// not fit in 32 bits; such a number cannot be stored in a Postgres numeric,
// so the caller leaves it as a json.Number, which compares by spelling.
func parseDecimal(n json.Number) (decimalValue, bool) {
	s := string(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var exp int64
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return decimalValue{}, false
		}
		exp, s = e, s[:i]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	// The value is the integer intPart+frac times 10^(exp-len(frac)).
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return decimalValue{}, true
	}
	sig := strings.TrimRight(digits, "0")
	exp += int64(len(digits)-len(sig)) - int64(len(frac))
	return decimalValue{neg: neg, digits: sig, exp: exp}, true
}
