package configs

import (
	"os"
	"testing"
)

func TestResolveAPIKey(t *testing.T) {
	t.Setenv("CODEX_LLM_KEY_TEST", "  sk-test-resolved  ")
	t.Setenv("CODEX_LLM_EMPTY_TEST", "")
	t.Setenv("CODEX_LLM_ABSENT_TEST", "")
	if err := os.Unsetenv("CODEX_LLM_ABSENT_TEST"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key, env, want string
		fails                bool
	}{
		{"literal", "sk-literal-test", "", "sk-literal-test", false},
		{"bare name stays literal", "CODEX_LLM_KEY_TEST", "", "CODEX_LLM_KEY_TEST", false},
		{"explicit expression", "${CODEX_LLM_KEY_TEST}", "", "sk-test-resolved", false},
		{"explicit field", "", "CODEX_LLM_KEY_TEST", "sk-test-resolved", false},
		{"explicit field wins", "sk-other", "CODEX_LLM_KEY_TEST", "sk-test-resolved", false},
		{"empty expression variable", "${CODEX_LLM_EMPTY_TEST}", "", "", true},
		{"empty field variable", "", "CODEX_LLM_EMPTY_TEST", "", true},
		{"missing expression", "${CODEX_LLM_ABSENT_TEST}", "", "", true},
		{"missing explicit field", "sk-other", "CODEX_LLM_ABSENT_TEST", "", true},
		{"invalid expression", "${CODEX_LLM_KEY_TEST", "", "", true},
		{"invalid name", "", "bad name", "", true},
		{"empty config", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveAPIKey(tc.key, tc.env)
			if (err != nil) != tc.fails || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.fails)
			}
		})
	}
}
