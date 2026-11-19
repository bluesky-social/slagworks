package inventory

import "testing"

// stubRunner returns a fixed dmidecode -s output (and error) regardless of
// args, and reports dmidecode as absent when unavailable is set.
type stubRunner struct {
	out         string
	err         error
	unavailable bool
}

func (s stubRunner) Run(name string, args ...string) error { return s.err }
func (s stubRunner) Output(name string, args ...string) (string, error) {
	return s.out, s.err
}
func (s stubRunner) Available(name string) bool { return !s.unavailable }

func TestSystemSerial_Normalization(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"real serial", "M12345ABC678", "M12345ABC678"},
		{"real serial with whitespace", "  M12345ABC678\n", "M12345ABC678"},
		{"to be filled", "To Be Filled By O.E.M.", ""},
		{"to be filled lowercase", "to be filled by o.e.m.", ""},
		{"default string", "Default string", ""},
		{"system serial number placeholder", "System Serial Number", ""},
		{"unprogrammed supermicro serial", "0123456789", ""},
		{"not specified", "Not Specified", ""},
		{"none", "None", ""},
		{"zero", "0", ""},
		{"all zeros", "00000000", ""},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SystemSerial(stubRunner{out: tt.out})
			if got != tt.want {
				t.Fatalf("SystemSerial(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}
