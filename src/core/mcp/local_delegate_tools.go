package mcp

import (
	"context"

	"xiaozhi-server-go/src/core/types"
)

// AddToolDelegate 注册后台任务工具。任务状态属于单个连接，
// 因此这里只把参数转交给连接级处理器，由 ConnectionHandler 管理任务。
func (c *LocalClient) AddToolDelegate() error {
	callHandler := func(funcName string) HandlerFunc {
		return func(ctx context.Context, args map[string]any) (interface{}, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return types.ActionResponse{
				Action: types.ActionTypeCallHandler,
				Result: types.ActionResponseCall{FuncName: funcName, Args: args},
			}, nil
		}
	}

	if err := c.AddTool("delegate_task",
		"把需要多步骤处理、联网检索、读写文件、编写代码或预计超过十秒的任务交给后台智能体。调用后系统会先告诉用户正在处理，完成后自动播报结果，本轮不要再重复回答。",
		ToolInputSchema{Type: "object", Properties: map[string]any{
			"task":    map[string]any{"type": "string", "description": "完整、独立可执行的任务描述，包含用户的全部要求"},
			"context": map[string]any{"type": "string", "description": "可选。对话中与任务相关的背景信息"},
		}, Required: []string{"task"}},
		callHandler("mcp_handler_delegate_task")); err != nil {
		return err
	}
	if err := c.AddTool("task_status",
		"用户询问后台任务进度时调用，返回当前设备的后台任务状态。",
		ToolInputSchema{Type: "object", Properties: map[string]any{}, Required: []string{}},
		callHandler("mcp_handler_task_status")); err != nil {
		return err
	}
	return c.AddTool("cancel_task",
		"用户要求取消后台任务时调用。不传 task_id 时取消当前设备的全部后台任务。",
		ToolInputSchema{Type: "object", Properties: map[string]any{
			"task_id": map[string]any{"type": "string", "description": "可选。要取消的任务编号"},
		}, Required: []string{}},
		callHandler("mcp_handler_cancel_task"))
}
