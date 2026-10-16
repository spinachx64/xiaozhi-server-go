package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xiaozhi-server-go/src/configs"
	"xiaozhi-server-go/src/core/function"
	"xiaozhi-server-go/src/core/types"
	"xiaozhi-server-go/src/core/utils"
)

func TestLocalSkillToolRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	dir := filepath.Join(root, "skills", "example")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: example\ndescription: Example purpose\n---\nPRIVATE_INSTRUCTIONS"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("PRIVATE_REFERENCE"), 0644); err != nil {
		t.Fatal(err)
	}
	logger, err := utils.NewLogger(&utils.LogCfg{LogDir: t.TempDir(), LogFile: "test.log", LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	c, err := NewLocalClient(logger, &configs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := &Manager{localClient: c, logger: logger, clients: map[string]MCPClient{"local": c}, funcHandler: function.NewFunctionRegistry()}
	m.registerTools(c.GetAvailableTools())
	if catalog := m.SkillCatalog(); !strings.Contains(catalog, "Example purpose") || strings.Contains(catalog, "PRIVATE_") {
		t.Fatal(catalog)
	}
	names := map[string]bool{}
	for _, tool := range c.GetAvailableTools() {
		names[tool.Function.Name] = true
	}
	if !names["local_load_skill"] || !names["local_read_skill_resource"] {
		t.Fatal(names)
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"local_load_skill", map[string]any{"name": "example"}, "PRIVATE_INSTRUCTIONS"},
		{"local_read_skill_resource", map[string]any{"name": "example", "path": "notes.md"}, "PRIVATE_REFERENCE"},
		{"local_load_skill", map[string]any{"name": 123}, "技能加载失败"},
		{"local_load_skill", map[string]any{"name": "missing"}, "技能加载失败"},
		{"local_read_skill_resource", map[string]any{"name": "example", "path": "../../outside"}, "技能加载失败"},
	} {
		if !m.IsMCPTool(tc.tool) || !m.funcHandler.FunctionExists(tc.tool) {
			t.Fatalf("tool is not routed: %s", tc.tool)
		}
		result, err := m.ExecuteTool(context.Background(), tc.tool, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		action, ok := result.(types.ActionResponse)
		if !ok || action.Action != types.ActionTypeReqLLM {
			t.Fatalf("unexpected result: %#v", result)
		}
		text, ok := action.Result.(string)
		if !ok || !strings.Contains(text, tc.want) {
			t.Fatalf("%s: %#v", tc.tool, result)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CallTool(ctx, "local_load_skill", map[string]any{"name": "example"}); err == nil {
		t.Fatal("canceled request accepted")
	}
}

func TestNoSkillsDoesNotRegisterLoaders(t *testing.T) {
	t.Chdir(t.TempDir())
	logger, err := utils.NewLogger(&utils.LogCfg{LogDir: t.TempDir(), LogFile: "test.log", LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	c, _ := NewLocalClient(logger, &configs.Config{})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.HasTool("load_skill") || c.HasTool("read_skill_resource") || c.skills.Catalog() != "" {
		t.Fatal("empty registry exposed skill tools")
	}
}
