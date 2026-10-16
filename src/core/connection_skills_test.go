package core

import (
	"testing"
	"xiaozhi-server-go/src/core/types"
)

func TestSkillCatalogDoesNotMutateHistory(t *testing.T) {
	history := []types.Message{{Role: "system", Content: "original role"}, {Role: "user", Content: "hello"}}
	for i := 0; i < 2; i++ {
		request := withSkillCatalog(history, "\nskill catalog")
		if request[0].Content != "original role\nskill catalog" || len(request) != 2 {
			t.Fatal(request)
		}
		if history[0].Content != "original role" {
			t.Fatal("catalog leaked into history")
		}
	}
	history[0].Content = "new role"
	if got := withSkillCatalog(history, "\nskill catalog")[0].Content; got != "new role\nskill catalog" {
		t.Fatal(got)
	}
}

func TestSkillCatalogWithoutSystemMessage(t *testing.T) {
	for _, history := range [][]types.Message{nil, {{Role: "user", Content: "hello"}}} {
		request := withSkillCatalog(history, "catalog")
		if len(request) != len(history)+1 || request[0].Role != "system" || request[0].Content != "catalog" {
			t.Fatal(request)
		}
		if got := withSkillCatalog(history, ""); len(got) != len(history) {
			t.Fatal(got)
		}
	}
}
