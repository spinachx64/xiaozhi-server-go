package mcp

import (
	"context"
	"encoding/json"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"xiaozhi-server-go/src/core/types"
	"xiaozhi-server-go/src/core/utils"
)

func TestRemoteHTTPDiscoveryAndResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": protocol.LATEST_PROTOCOL_VERSION, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "test", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			name := "first"
			cursor := "page2"
			if req.Params["cursor"] == "page2" {
				name = "second"
				cursor = ""
			}
			result = map[string]any{"tools": []any{map[string]any{"name": name, "description": "test tool", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}, "nextCursor": cursor}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "one"}, map[string]string{"type": "text", "text": "two"}}}
		default:
			t.Errorf("unexpected method %q", req.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	logger, err := utils.NewLogger(&utils.LogCfg{LogDir: t.TempDir(), LogFile: "test.log", LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	cfg, err := convertConfig(map[string]interface{}{"url": srv.URL, "transport": "streamable_http"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.IsReady() || !c.HasTool("mcp_first") || !c.HasTool("mcp_second") {
		t.Fatal("discovery or pagination failed")
	}
	response, err := c.CallTool(context.Background(), "mcp_first", nil)
	if err != nil {
		t.Fatal(err)
	}
	action, ok := response.(types.ActionResponse)
	if !ok || action.Result != "one\ntwo" || action.Action != types.ActionTypeReqLLM {
		t.Fatalf("invalid dialogue result: %#v", response)
	}
}

func TestInvalidMCPTransportAndResultErrors(t *testing.T) {
	for _, cfg := range []*Config{{Enabled: true}, {Enabled: true, URL: "http://localhost", Transport: "unsupported"}} {
		if _, err := NewClient(cfg, nil); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	for _, result := range []*protocol.CallToolResult{nil, {}, {IsError: true, Content: []protocol.Content{protocol.TextContent{Type: "text", Text: "failed"}}}} {
		text := toolResultText(result)
		if strings.TrimSpace(text) == "" {
			t.Fatal("empty result cannot resume dialogue")
		}
		if result != nil && result.IsError && !strings.Contains(text, "failed") {
			t.Fatal(text)
		}
	}
}
