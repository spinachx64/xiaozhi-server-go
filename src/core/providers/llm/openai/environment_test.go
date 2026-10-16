package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xiaozhi-server-go/src/configs"
	"xiaozhi-server-go/src/core/provider"
	"xiaozhi-server-go/src/core/providers/llm"
	"xiaozhi-server-go/src/core/types"
	"xiaozhi-server-go/src/core/utils"
)

func TestEnvironmentKeyReachesHTTPWithoutChangingStoredConfig(t *testing.T) {
	var want atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+want.Load().(string) {
			t.Error("resolved credential did not reach HTTP authorization")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	logger, err := utils.NewLogger(&utils.LogCfg{LogDir: t.TempDir(), LogFile: "test.log", LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	for _, mode := range []string{"bare", "expression", "field", "literal"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_LLM_HTTP_KEY_TEST", "sk-http-test-one")
			stored := configs.LLMConfig{Type: "openai", ModelName: "test-model", BaseURL: server.URL + "/v1", APIKey: "CODEX_LLM_HTTP_KEY_TEST"}
			switch mode {
			case "expression":
				stored.APIKey = "${CODEX_LLM_HTTP_KEY_TEST}"
			case "field":
				stored.APIKey = ""
				stored.EnvKey = "CODEX_LLM_HTTP_KEY_TEST"
			case "literal":
				stored.APIKey = "sk-http-literal"
			}
			config := &configs.Config{LLM: map[string]configs.LLMConfig{"TestLLM": stored}}
			before := config.ToString()
			for _, key := range []string{"sk-http-test-one", "sk-http-test-two"} {
				t.Setenv("CODEX_LLM_HTTP_KEY_TEST", key)
				switch mode {
				case "literal":
					want.Store("sk-http-literal")
				case "bare":
					// 裸变量名不再读取环境变量，按字面密钥发送。
					want.Store("CODEX_LLM_HTTP_KEY_TEST")
				default:
					want.Store(key)
				}
				created, err := provider.NewLLMFactory("TestLLM", config, logger).Create()
				if err != nil {
					t.Fatal(err)
				}
				model := created.(types.LLMProvider)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				stream, err := model.Response(ctx, "test", []types.Message{{Role: "user", Content: "hello"}})
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				var text strings.Builder
				for part := range stream {
					text.WriteString(part)
				}
				toolStream, err := model.ResponseWithFunctions(ctx, "test", []types.Message{{Role: "user", Content: "hello"}}, nil)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				var toolText strings.Builder
				for part := range toolStream {
					if part.Error != "" {
						t.Error("tool-enabled request failed")
					}
					toolText.WriteString(part.Content)
				}
				cancel()
				model.Cleanup()
				if text.String() != "ok" {
					t.Fatalf("unexpected model response %q", text.String())
				}
				if toolText.String() != "ok" {
					t.Fatal("tool-enabled response was not received")
				}
				if after := config.ToString(); after != before {
					t.Fatal("resolved secret changed stored configuration")
				}
			}
		})
	}
}

func TestExplicitEnvironmentReferenceFailsBeforeProviderInitialization(t *testing.T) {
	t.Setenv("CODEX_LLM_HTTP_MISSING_TEST", "")
	config := &llm.Config{Type: "openai", ModelName: "test", APIKey: "${CODEX_LLM_HTTP_MISSING_TEST}"}
	_, err := llm.Create("openai", config)
	if err == nil || !strings.Contains(err.Error(), "CODEX_LLM_HTTP_MISSING_TEST") {
		t.Fatal(err)
	}
	if config.APIKey != "${CODEX_LLM_HTTP_MISSING_TEST}" {
		t.Fatal("input configuration mutated")
	}
}
