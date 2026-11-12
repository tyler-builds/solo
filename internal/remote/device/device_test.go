package device

import "testing"

// Names come from the other end and are shown to a person deciding whether to trust it: what could
// hide or reorder text is dropped.
func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"Tyler's phone":               "Tyler's phone",
		"  spaced  ":                  "spaced",
		"bell\x07 and\nnewline":       "bell andnewline",
		"\u202egnp.exe":               "gnp.exe",
		"zero\u200bwidth\ufeff":       "zerowidth",
		"\u2066isolate\u2069":         "isolate",
		"":                            "",
		"\x00\x01":                    "",
		"\u00dcn\u00efc\u00f6d\u00e9": "\u00dcn\u00efc\u00f6d\u00e9",
		"0123456789012345678901234567890123456789-and-more": "0123456789012345678901234567890123456789",
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
