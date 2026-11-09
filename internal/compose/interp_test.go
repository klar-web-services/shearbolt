package compose

import (
	"reflect"
	"strings"
	"testing"
)

func TestInterpolate(t *testing.T) {
	env := func(name string) (string, bool) {
		v, ok := map[string]string{"TAG": "1.2", "EMPTY": ""}[name]
		return v, ok
	}
	tests := []struct {
		in      string
		out     string
		missing []missingVar
		err     string // substring of the error; "" for none
	}{
		{in: "nginx:1.27", out: "nginx:1.27"},
		{in: "app:$TAG", out: "app:1.2"},
		{in: "app:${TAG}", out: "app:1.2"},
		{in: "app:${NOPE}", missing: []missingVar{{name: "NOPE"}}},
		{in: "app:$NOPE", missing: []missingVar{{name: "NOPE"}}},
		{in: "app:${NOPE:-x}", out: "app:x"},
		{in: "app:${NOPE-x}", out: "app:x"},
		{in: "app:${EMPTY:-x}", out: "app:x"},
		{in: "app:${EMPTY-x}", out: "app:"},
		{in: "app:${NOPE:?set it}", missing: []missingVar{{name: "NOPE", message: "set it"}}},
		{in: "app:${EMPTY?m}", out: "app:"},
		{in: "app:${EMPTY:?m}", missing: []missingVar{{name: "EMPTY", message: "m"}}},
		{in: "app:${TAG:+alt}", out: "app:alt"},
		{in: "app:${NOPE:+alt}", out: "app:"},
		{in: "app:${EMPTY+alt}", out: "app:alt"},
		{in: "a$$b", out: "a$b"},
		{in: "app:${A:-${B}}", err: "nest"},
		{in: "app:${TAG", err: "unclosed"},
		{in: "app:$", err: "lone $"},
		{in: "app:$-x", err: "starts no variable"},
		{in: "app:${1A}", err: "variable name"},
		{in: "app:${TAG/x}", err: "understands"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			out, missing, err := interpolate(tt.in, env)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want one containing %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(missing, tt.missing) {
				t.Fatalf("missing = %v, want %v", missing, tt.missing)
			}
			if len(tt.missing) == 0 && out != tt.out {
				t.Fatalf("out = %q, want %q", out, tt.out)
			}
		})
	}
}
