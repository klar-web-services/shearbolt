package github

import "testing"

const marker = "steps.shearbolt-build.outputs.shearbolt-reached != ''"

func TestMarkerExpr(t *testing.T) {
	if got := markerExpr("shearbolt-build"); got != marker {
		t.Errorf("markerExpr = %q, want %q", got, marker)
	}
}

func TestWithMarker(t *testing.T) {
	for cond, want := range map[string]string{
		"":                                   marker,
		"  ":                                 marker,
		"github.ref == 'refs/heads/main'":    "(github.ref == 'refs/heads/main') && " + marker,
		"${{ github.event_name == 'push' }}": "(github.event_name == 'push') && " + marker,
		"a || b":                             "(a || b) && " + marker,
	} {
		if got := withMarker(cond, "shearbolt-build"); got != want {
			t.Errorf("withMarker(%q) = %q, want %q", cond, got, want)
		}
	}
}

func TestContinuedIf(t *testing.T) {
	for cond, want := range map[string]string{
		"":                                       marker,
		"github.ref == 'refs/heads/main'":        "success() && (github.ref == 'refs/heads/main') && " + marker,
		"${{ github.ref == 'refs/heads/main' }}": "success() && (github.ref == 'refs/heads/main') && " + marker,
		"a || b":                                 "success() && (a || b) && " + marker,
		// safeIf first: a status check is wrapped in success() && (...), then unwrapped.
		"always()":            "success() && (success() && (always())) && " + marker,
		"${{ !cancelled() }}": "success() && (success() && (!cancelled())) && " + marker,
	} {
		got, err := continuedIf(cond, "shearbolt-build")
		if err != nil || got != want {
			t.Errorf("continuedIf(%q) = %q, %v, want %q", cond, got, err, want)
		}
	}
	// Combined with other text, ${{ }} would make the whole condition a non-empty string, which
	// is always true.
	for _, cond := range []string{"always() && ${{ github.ref == 'x' }}", "github.ref == ${{ env.REF }}"} {
		if got, err := continuedIf(cond, "shearbolt-build"); err == nil {
			t.Errorf("continuedIf(%q) = %q, want an error", cond, got)
		}
	}
}

func TestStripMarker(t *testing.T) {
	for cond, want := range map[string]string{
		marker:                  "",
		"${{ " + marker + " }}": "",
		"(github.ref == 'refs/heads/main') && " + marker:                   "github.ref == 'refs/heads/main'",
		"${{ (github.ref == 'refs/heads/main') && " + marker + " }}":       "github.ref == 'refs/heads/main'",
		"(a || b) && " + marker:                                            "a || b",
		"(a)  &&  steps.shearbolt-build.outputs.shearbolt-reached  !=  ''": "a",
	} {
		got, ok := stripMarker(cond, "shearbolt-build")
		if !ok || got != want {
			t.Errorf("stripMarker(%q) = %q, %v, want %q", cond, got, ok, want)
		}
	}
	// What withMarker adds, stripMarker removes.
	for _, cond := range []string{"", "a || b", "github.ref == 'refs/heads/main'"} {
		if got, ok := stripMarker(withMarker(cond, "x"), "x"); !ok || got != cond {
			t.Errorf("stripMarker(withMarker(%q)) = %q, %v", cond, got, ok)
		}
	}
	for _, cond := range []string{
		"",
		"a",
		"(a) && steps.other.outputs.shearbolt-reached != ''",
		"(a) || (b) && " + marker,
		"a && " + marker,
		marker + " && a",
		"a || " + marker,
	} {
		if got, ok := stripMarker(cond, "shearbolt-build"); ok || got != cond {
			t.Errorf("stripMarker(%q) = %q, %v, want it unchanged", cond, got, ok)
		}
	}
}

func TestMarkerGuarded(t *testing.T) {
	for cond, want := range map[string]string{
		marker:                  "shearbolt-build",
		"${{ " + marker + " }}": "shearbolt-build",
		"steps.b-2.outputs.shearbolt-reached != ''":                      "b-2",
		"success() && (github.ref == 'refs/heads/main') && " + marker:    "shearbolt-build",
		"${{ success() && (a || b) && " + marker + " }}":                 "shearbolt-build",
		"success()  &&  (a)\n&& steps.x.outputs.shearbolt-reached != ''": "x",
		"success() && (x == ') && ') && " + marker:                       "shearbolt-build",
	} {
		if id, ok := markerGuarded(cond); !ok || id != want {
			t.Errorf("markerGuarded(%q) = %q, %v, want %q", cond, id, ok, want)
		}
	}
	for _, cond := range []string{
		"",
		"a",
		"a || steps.x.outputs.shearbolt-reached != ''",
		"steps.x.outputs.shearbolt-reached != '' && a",
		"success() && (a) || (b) && steps.x.outputs.shearbolt-reached != ''",
		"(a) && steps.x.outputs.shearbolt-reached != ''",
		"always() && (a) && steps.x.outputs.shearbolt-reached != ''",
		"success() && (a) && steps.x.outputs.shearbolt-image != ''",
		"steps.x.outputs.shearbolt-reached == ''",
		"steps..outputs.shearbolt-reached != ''",
	} {
		if id, ok := markerGuarded(cond); ok {
			t.Errorf("markerGuarded(%q) = %q, true, want false", cond, id)
		}
	}
}
