package zosia

import "testing"

func TestTargetUsesOverrides(t *testing.T) {
	k, _ := testKey(t)
	pub := "test:" + encodeKey(k)
	url, got, err := Target("http://127.0.0.1:3000", pub)
	if err != nil || url != "http://127.0.0.1:3000" || got.ID != "test" {
		t.Fatalf("%q %+v %v", url, got, err)
	}
}

func TestTargetDefaultsToProduction(t *testing.T) {
	url, k, err := Target("", "")
	if err != nil || url != "https://zosiasec.com" || k.ID != "2026a" {
		t.Fatalf("%q %+v %v", url, k, err)
	}
}

func TestParseKeyRejectsMalformedKeys(t *testing.T) {
	for _, s := range []string{"", "nokid", ":abc", "k:not-base64!", "k:AAAA"} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) accepted", s)
		}
	}
}
