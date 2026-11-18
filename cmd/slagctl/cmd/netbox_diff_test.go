package cmd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/api"
)

func TestNetBoxDiffHumanOutputAndExitSemantics(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	for _, tc := range []struct {
		name       string
		matched    bool
		wantErr    bool
		wantResult string
	}{
		{name: "match", matched: true, wantResult: "NetBox bindings match."},
		{name: "mismatch", matched: false, wantErr: true, wantResult: "NetBox binding drift detected."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/machines/"+id.String()+"/netbox-diff", func(w http.ResponseWriter, r *http.Request) {
				matched := tc.matched
				_ = json.NewEncoder(w).Encode(api.NetBoxDiff{
					MachineID: id.String(),
					Matched:   tc.matched,
					Fields: []api.DiffField{
						{Name: "serial", Slagworks: "SN-A", NetBox: "SN-A", Comparable: true, Match: &matched},
						{Name: "hostname", Slagworks: "(not stored)", NetBox: "cpu1.pop1"},
					},
				})
			})
			out, err := execCmd(t, testServer(t, mux), "netbox", "diff", id.String())
			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, cmd.ErrSilent) {
				t.Fatalf("error = %v, want ErrSilent", err)
			}
			for _, want := range []string{"FIELD", "serial", "hostname", tc.wantResult} {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestNetBoxDiffJSONPrintsReportBeforeNonZeroExit(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+id.String()+"/netbox-diff", func(w http.ResponseWriter, r *http.Request) {
		matched := false
		_ = json.NewEncoder(w).Encode(api.NetBoxDiff{
			MachineID: id.String(),
			Missing:   api.MissingNetBox,
			Matched:   false,
			Fields: []api.DiffField{{
				Name: "serial", Slagworks: "SN-A", NetBox: "(unavailable)",
				Comparable: true, Match: &matched,
			}},
		})
	})
	out, err := execCmd(t, testServer(t, mux), "netbox", "diff", id.String(), "--json")
	if !errors.Is(err, cmd.ErrSilent) {
		t.Fatalf("error = %v, want ErrSilent", err)
	}
	var got api.NetBoxDiff
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got.Matched || got.Missing != api.MissingNetBox || len(got.Fields) != 1 {
		t.Fatalf("JSON report = %+v", got)
	}
}
