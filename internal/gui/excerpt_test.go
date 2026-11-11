package gui

import "testing"

func TestThoughtExcerpt(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"empty", "", ""},
		{"whitespace", "  \n\n \t\n", ""},
		{"one finished line", "Check the branch first.\n", "Check the branch first."},
		{"short and unfinished", "Check the bra", ""},
		{"long and unfinished", "Three independent tasks in two repos, so I'll sta", "Three independent tasks in two repos, so I'll"},
		{"unfinished, ends in a space", "Three independent tasks in two repos ", "Three independent tasks in two repos"},
		{"newest paragraph", "First I'll look at the auth module.\n\nThe refresh path drops the retry.\nIt should retry once.", "The refresh path drops the retry."},
		{"newest paragraph is too short yet", "First I'll look at the auth module.\n\nThe refr", "First I'll look at the auth module."},
		{"blank line after", "First thought.\n\nSecond thought.\n\n", "Second thought."},
		{"whitespace-only line separates", "First thought.\n  \t\nSecond thought.\n", "Second thought."},
		{"crlf", "First thought.\r\n\r\nSecond thought.\r\n", "Second thought."},
		{"bold heading", "**Planning the threads**\n\nnot yet", "Planning the threads"},
		{"heading", "## Checking the `refresh.ts` retry\n", "Checking the refresh.ts retry"},
		{"bullet", "- Look at **session.ts** first\n- then the tests", "Look at session.ts first"},
		{"numbered", "1. Spawn a thread for web\n", "Spawn a thread for web"},
		{"quote", "> the user wants it in main\n", "the user wants it in main"},
		{"rule only", "Earlier thought.\n\n---\n", "Earlier thought."},
	} {
		if got := thoughtExcerpt(c.text); got != c.want {
			t.Errorf("%s: thoughtExcerpt(%q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}
