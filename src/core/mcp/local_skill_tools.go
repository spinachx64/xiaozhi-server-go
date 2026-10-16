package mcp

import (
	"context"
	"fmt"
	"xiaozhi-server-go/src/core/types"
)

func (c *LocalClient) registerSkillTools() error {
	schema := ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"name": map[string]any{"type": "string", "description": "技能目录中的准确技能名称"},
		},
		Required: []string{"name"},
	}
	if err := c.AddTool("load_skill", "按需加载本地技能完整说明。任务符合技能目录中的用途时，先加载再执行。", schema,
		func(ctx context.Context, args map[string]any) (interface{}, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name, ok := args["name"].(string)
			if !ok || name == "" {
				return skillResult("", fmt.Errorf("name must be a non-empty string")), nil
			}
			body, err := c.skills.Load(name)
			return skillResult(body, err), nil
		}); err != nil {
		return err
	}
	return c.AddTool("read_skill_resource", "按需读取技能目录内的 UTF-8 参考资料；path 相对于该技能目录。仅在已加载说明引用且任务需要时调用。",
		ToolInputSchema{Type: "object", Properties: map[string]any{
			"name": map[string]any{"type": "string", "description": "技能名称"},
			"path": map[string]any{"type": "string", "description": "技能内的相对文件路径，例如 references/format.md"},
		}, Required: []string{"name", "path"}},
		func(ctx context.Context, args map[string]any) (interface{}, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name, nameOK := args["name"].(string)
			path, pathOK := args["path"].(string)
			if !nameOK || !pathOK || name == "" || path == "" {
				return skillResult("", fmt.Errorf("name and path must be non-empty strings")), nil
			}
			body, err := c.skills.ReadResource(name, path)
			return skillResult(body, err), nil
		})
}

func skillResult(body string, err error) types.ActionResponse {
	if err != nil {
		body = "技能加载失败：" + err.Error()
	}
	if body == "" {
		body = "参考资料为空。"
	}
	return types.ActionResponse{Action: types.ActionTypeReqLLM, Result: body}
}
