package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"xiaozhi-server-go/src/core/types"
	"xiaozhi-server-go/src/core/utils"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/sashabaranov/go-openai"
)

// Config 定义MCP客户端配置
type Config struct {
	Enabled       bool     `yaml:"enabled"`
	ServerAddress string   `yaml:"server_address"`
	ServerPort    int      `yaml:"server_port"`
	Namespace     string   `yaml:"namespace"`
	NodeID        string   `yaml:"node_id"`
	ResourceTypes []string `yaml:"resource_types"`
	Command       string   `yaml:"command,omitempty"` // 命令行连接方式
	Args          []string `yaml:"args,omitempty"`    // 命令行参数
	Env           []string `yaml:"env,omitempty"`     // 环境变量
	URL           string   `yaml:"url,omitempty"`
	Transport     string   `yaml:"transport,omitempty"` // streamable_http (default for URL) or sse
}

// Client 封装MCP客户端功能
type Client struct {
	client *mcpclient.Client
	config *Config
	name   string
	tools  []Tool
	ready  bool
	mu     sync.RWMutex
	logger *utils.Logger
}

// NewClient supports subprocess stdio and native remote MCP transports.
func NewClient(config *Config, logger *utils.Logger) (*Client, error) {
	if config == nil || !config.Enabled {
		return nil, fmt.Errorf("MCP client is disabled in config")
	}
	c := &Client{config: config, logger: logger}
	var err error
	switch {
	case config.Command != "":
		c.client, err = mcpclient.NewStdioMCPClient(config.Command, config.Env, config.Args...)
	case config.URL != "":
		switch config.Transport {
		case "", "streamable_http", "streamable-http", "http":
			c.client, err = mcpclient.NewStreamableHttpClient(config.URL)
		case "sse":
			c.client, err = mcpclient.NewSSEMCPClient(config.URL)
		default:
			return nil, fmt.Errorf("unsupported MCP transport %q", config.Transport)
		}
	default:
		return nil, fmt.Errorf("MCP configuration requires command or url")
	}
	if err != nil {
		return nil, fmt.Errorf("create MCP client: %w", err)
	}
	return c, nil
}

func (c *Client) Start(ctx context.Context) error {
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The SDK's stdio constructor already starts its transport.
	if c.config.Command == "" {
		if err := c.client.Start(initCtx); err != nil {
			c.Stop()
			return fmt.Errorf("start MCP transport: %w", err)
		}
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "zhi-server", Version: "1.0.0"}
	result, err := c.client.Initialize(initCtx, req)
	if err != nil {
		c.Stop()
		return fmt.Errorf("initialize MCP client: %w", err)
	}
	c.name = result.ServerInfo.Name
	if err := c.fetchTools(initCtx); err != nil {
		c.Stop()
		return err
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	c.logger.Info("Initialized MCP server: %s %s (%d tools)", c.name, result.ServerInfo.Version, len(c.tools))
	return nil
}

func (c *Client) fetchTools(ctx context.Context) error {
	req := mcp.ListToolsRequest{}
	collected := []Tool{}
	seen := map[mcp.Cursor]bool{}
	for {
		result, err := c.client.ListTools(ctx, req)
		if err != nil {
			return fmt.Errorf("list MCP tools: %w", err)
		}
		for _, tool := range result.Tools {
			required := tool.InputSchema.Required
			if required == nil {
				required = []string{}
			}
			collected = append(collected, Tool{Name: tool.Name, Description: tool.Description,
				InputSchema: ToolInputSchema{Type: tool.InputSchema.Type, Properties: tool.InputSchema.Properties, Required: required}})
		}
		if result.NextCursor == "" {
			break
		}
		if seen[result.NextCursor] {
			return fmt.Errorf("MCP tools pagination repeated cursor")
		}
		seen[result.NextCursor] = true
		req.Params.Cursor = result.NextCursor
	}
	c.mu.Lock()
	c.tools = collected
	c.mu.Unlock()
	return nil
}

func (c *Client) Stop() {
	if c.client != nil {
		_ = c.client.Close()
	}
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
}

// HasTool 检查是否有指定名称的工具
func (c *Client) HasTool(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// 如果有mcp_前缀，则去掉前缀
	if len(name) > 4 && name[:4] == "mcp_" {
		name = name[4:]
	}

	for _, tool := range c.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// GetAvailableTools 获取所有可用工具
func (c *Client) GetAvailableTools() []openai.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]openai.Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		openaiTool := openai.Tool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        fmt.Sprintf("mcp_%s", tool.Name),
				Description: tool.Description,
				Parameters: map[string]interface{}{
					"type":       tool.InputSchema.Type,
					"properties": tool.InputSchema.Properties,
					"required":   tool.InputSchema.Required,
				},
			},
		}

		result = append(result, openaiTool)
	}
	return result
}

// CallTool 调用指定的工具
func (c *Client) CallTool(
	ctx context.Context,
	name string,
	args map[string]any,
) (interface{}, error) {
	// 如果有mcp_前缀，则去掉前缀
	if len(name) > 4 && name[:4] == "mcp_" {
		name = name[4:]
	}
	// 检查工具是否存在
	if !c.HasTool(name) {
		return nil, fmt.Errorf("tool %s not found", name)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	result, err := c.client.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("call MCP tool %s: %w", name, err)
	}
	return types.ActionResponse{Action: types.ActionTypeReqLLM, Result: toolResultText(result)}, nil
}

// The dialogue loop consumes strings, including multi-block and non-text results.
func toolResultText(result *mcp.CallToolResult) string {
	if result == nil {
		return "MCP工具没有返回内容"
	}
	parts := []string{}
	for _, item := range result.Content {
		if text, ok := item.(mcp.TextContent); ok {
			parts = append(parts, text.Text)
		} else {
			data, err := json.Marshal(item)
			if err == nil {
				parts = append(parts, string(data))
			}
		}
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		text = "MCP工具没有返回内容"
	}
	if result.IsError {
		text = "MCP工具返回错误：" + text
	}
	return text
}

// IsReady 检查客户端是否已初始化完成并准备就绪
func (c *Client) IsReady() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// ResetConnection 重置连接状态
func (c *Client) ResetConnection() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 保留工具信息，只重置连接状态
	c.ready = false

	// 如果有活跃连接，优雅关闭
	if c.client != nil {
		// 不完全关闭，只标记为未就绪
		// 在下次Start时会重新建立连接
	}

	return nil
}
