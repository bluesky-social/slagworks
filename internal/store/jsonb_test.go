package store

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestJSONBSafe(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no escape", `{"a":"b"}`, `{"a":"b"}`},
		{"other escapes kept", `{"a":"\u00e9\n\"\u0001\/"}`, `{"a":"\u00e9\n\"\u0001\/"}`},
		{"NUL", `{"error":"bad \u0000 label"}`, `{"error":"bad \ufffd label"}`},
		{"NUL in a key", `{"\u0000":1}`, `{"\ufffd":1}`},
		{"adjacent NULs", `"\u0000\u0000"`, `"\ufffd\ufffd"`},
		{"escaped backslash, then the text u0000", `"\\u0000"`, `"\\u0000"`},
		{"escaped backslash, then NUL", `"\\\u0000"`, `"\\\ufffd"`},
		{"surrogate pairs kept", `"\ud83d\ude00 \uD83D\uDE00"`, `"\ud83d\ude00 \uD83D\uDE00"`},
		{"lone high surrogate", `"\ud800x"`, `"\ufffdx"`},
		{"lone high surrogate ending the string", `"\ud800"`, `"\ufffd"`},
		{"lone low surrogate", `"\udc00"`, `"\ufffd"`},
		{"high surrogate, then another escape", `"\ud800\u0041"`, `"\ufffd\u0041"`},
		{"two high surrogates, then a low one", `"\ud800\ud800\udc00"`, `"\ufffd\ud800\udc00"`},
		{"high surrogate, then NUL", `"\ud800\u0000"`, `"\ufffd\ufffd"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			got := jsonbSafe(in)
			if string(got) != tc.want {
				t.Errorf("jsonbSafe(%s) = %s, want %s", tc.in, got, tc.want)
			}
			if !json.Valid(got) {
				t.Errorf("jsonbSafe(%s) = %s, which is not valid JSON", tc.in, got)
			}
			if tc.in == tc.want && &got[0] != &in[0] {
				t.Errorf("jsonbSafe(%s) copied a document it did not change", tc.in)
			}
		})
	}
	if got := jsonbSafe(nil); got != nil {
		t.Errorf("jsonbSafe(nil) = %q, want nil (a NULL column)", got)
	}
}

// Every writer of operations.result and machines.commission_data stores a
// document holding an escape jsonb refuses, with the escape replaced by
// U+FFFD, instead of failing. A bmc_reconcile op, for one, completes with a
// TimeError that can quote a BMC's error body.
// CommitCommissionResult has its own test beside the replay tests.
func TestJSONBWriters_StoreEscapesJSONBRefuses(t *testing.T) {
	doc := []byte(`{"error":"NUL \u0000, lone surrogate \ud800, pair \ud83d\ude00"}`)
	const want = "NUL \uFFFD, lone surrogate \uFFFD, pair \U0001F600"
	opResult := func(t *testing.T, st *Store, _ *model.Machine, op *model.Operation) []byte {
		got, err := st.GetOperation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != model.OpCompleted && got.Status != model.OpFailed {
			t.Errorf("op is still %s", got.Status)
		}
		return got.Result
	}
	for _, tc := range []struct {
		name  string
		write func(st *Store, m *model.Machine, op *model.Operation) error
		read  func(t *testing.T, st *Store, m *model.Machine, op *model.Operation) []byte
	}{
		{"CompleteOperation", func(st *Store, _ *model.Machine, op *model.Operation) error {
			return st.CompleteOperation(op.ID, doc)
		}, opResult},
		{"CompleteOperationWithState", func(st *Store, _ *model.Machine, op *model.Operation) error {
			_, err := st.CompleteOperationWithState(op.ID, "", doc)
			return err
		}, opResult},
		{"CompleteActiveOperation", func(st *Store, m *model.Machine, op *model.Operation) error {
			_, err := st.CompleteActiveOperation(m.ID, op.Type, m.State, doc)
			return err
		}, opResult},
		{"FailOperation", func(st *Store, _ *model.Machine, op *model.Operation) error {
			return st.FailOperation(op.ID, doc)
		}, opResult},
		{"StoreCommissionData", func(st *Store, m *model.Machine, _ *model.Operation) error {
			return st.StoreCommissionData(m.ID, doc)
		}, func(t *testing.T, st *Store, m *model.Machine, _ *model.Operation) []byte {
			got, err := st.GetMachine(m.ID)
			if err != nil {
				t.Fatal(err)
			}
			return got.CommissionData
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-jsonb-" + tc.name, State: model.MachineStateCommissioned}
			if err := st.CreateMachine(m); err != nil {
				t.Fatal(err)
			}
			op := &model.Operation{MachineID: m.ID, Type: model.OpBMCReconcile, Status: model.OpRunning}
			if err := st.CreateOperation(op); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(st, m, op); err != nil {
				t.Fatalf("write: %v", err)
			}
			var stored struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(tc.read(t, st, m, op), &stored); err != nil {
				t.Fatalf("decode stored document: %v", err)
			}
			if stored.Error != want {
				t.Errorf("stored error = %q, want %q", stored.Error, want)
			}
		})
	}
}
