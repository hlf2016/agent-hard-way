package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const maxReadBytes = 4 * 1024

// toolCall 是模型发起的一次工具调用。注意 Arguments 是 JSON **字符串**，不是对象——
// 模型逐字生成它，协议原样转交，解析是你的事。
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// message 长出了两个新字段。它现在能表达三件事：
// 人说的话（Role=user）、模型的回话或调用请求（Role=assistant，可能带 ToolCalls）、
// 工具的汇报（Role=tool，带 ToolCallID）——第四种也是最后一种 role，到齐了。
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`   // assistant 请求调用工具时才有值
	ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时必填：这是对哪次调用的答复
}

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// tool 是发给模型的工具声明。Parameters 是一份 JSON Schema——
// 模型看到的全部就是这些字段：叫什么、干什么、怎么传参。
type tool struct {
	Type     string   `json:"type"` // 固定 "function"
	Function toolSpec `json:"function"`
}

type request struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	Tools     []tool    `json:"tools"`
}

type response struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"` // 新值登场："tool_calls"
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// tools 声明我们的第一个工具：read_file。
var tools = []tool{{
	Type: "function",
	Function: toolSpec{
		Name:        "read_file",
		Description: "读取一个本地文件，返回它的文件内容",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要读取的文件路径（相对或绝对）",
				},
			},
			"required": []string{"path"},
		},
	},
}, {
	Type: "function",
	Function: toolSpec{
		Name:        "list_files",
		Description: "列出目录内容",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要读取的文件路径",
				},
			},
			"required": []string{"path"},
		},
	},
}}

// execute 按名字分发工具调用。未知工具返回干净的错误文本而不是崩溃——
// 错误也是回填给模型的合法结果，它看得懂，还会自己想办法。
func execute(name, args string) string {
	switch name {
	case "read_file":
		return readFile(args)
	case "list_files":
		return listFiles(args)
	default:
		return "错误: 位置空间 " + name
	}
}

func readFile(args string) string {
	var data []byte
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误，参数不是合法 json ： " + err.Error()
	}
	f, err := os.Open(in.Path)
	if err != nil {
		return "错误： " + err.Error()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "错误： " + err.Error()
	}
	totalSize := info.Size()
	if totalSize > maxReadBytes {
		buf, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
		if err != nil {
			return "错误: " + err.Error()
		}
		return fmt.Sprintf("%s\n[截断：文件共 %d 字节]", string(buf), totalSize)
	}

	data, err = os.ReadFile(in.Path)
	if err != nil {
		// 不要 panic，不要 os.Exit——把失败告诉模型，它会调整。
		return "错误： " + err.Error()
	}

	return string(data)
}

func listFiles(args string) string {
	var r struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &r); err != nil {
		return "错误，参数不是合法 json：" + err.Error()
	}
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		return "列出文件失败：" + err.Error()
	}
	var sb strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/" // 目录加上斜杠以便区分
		}
		sb.WriteString("- " + name + "\n")
	}
	return sb.String()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, `用法: ./ex05 "你的任务"`)
		os.Exit(1)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	model := os.Getenv("MODEL")
	if apiKey == "" || model == "" {
		fmt.Fprintln(os.Stderr, "需要环境变量 OPENAI_API_KEY 和 MODEL")
		fmt.Fprintln(os.Stderr, `例: export OPENAI_API_KEY=sk-xxxx`)
		fmt.Fprintln(os.Stderr, `    export MODEL=deepseek-v4-flash`)
		fmt.Fprintln(os.Stderr, `    export OPENAI_BASE_URL=https://api.deepseek.com/v1  # 不设则默认 OpenAI 官方`)
		os.Exit(1)
	}
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}

	history := []message{
		{
			Role:    "user",
			Content: os.Args[1],
		},
	}
	// agent loop。注意它和练习 3 的 REPL 是同一个循环，
	// 只是对话的另一方从"人"换成了"工具"。
	const maxRounds = 10 // 保险丝：防模型在工具里打转，烧光你的钱包
	for round := 1; round <= maxRounds; round++ {
		r, err := send(base, apiKey, model, history)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		msg := r.Choices[0].Message
		// 模型的回复原样塞回历史——包括 tool_calls。
		// 少了它，下一轮模型看不到自己发起过调用，协议直接报错。
		history = append(history, msg)
		// 练习 1 的纪律在这里派上大用场：循环走哪条路，看 finish_reason。
		if r.Choices[0].FinishReason != "tool_calls" {
			fmt.Println(msg.Content)
			fmt.Fprintf(os.Stderr, "\n[共 %d 轮 . 最后一轮输入 %d tokens  . finish_reason=%s]\n", round, r.Usage.PromptTokens, r.Choices[0].FinishReason)
			return
		}
		// 模型要调工具。逐个执行，每个调用回填一条 role:"tool" 消息
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(os.Stderr, "round [%d] %s(%s)\n", round, tc.Function.Name, tc.Function.Arguments)
			result := execute(tc.Function.Name, tc.Function.Arguments)
			history = append(history, message{
				Role:       "tool",
				ToolCallID: tc.ID, // 一次调用一张回执，靠这个 ID 对上号
				Content:    result,
			})
		}
	}
	fmt.Fprintf(os.Stderr, "达到 %d 轮上限，停止 \n", maxRounds)
	os.Exit(1)
}

// send 就是练习 1 的非流式请求，多带一个 tools 字段。
func send(base, apiKey, model string, history []message) (response, error) {
	var r response
	body, _ := json.Marshal(request{
		Model:     model,
		Messages:  history,
		MaxTokens: 4096,
		Tools:     tools,
	})
	req, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return r, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return r, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("HTTP %d - %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("解析失败 %w \n 原始响应 %s", err, raw)
	}
	if r.Error != nil {
		return r, fmt.Errorf("请求 api 错误 [%s]: %s", r.Error.Type, r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return r, fmt.Errorf("空响应: %s", raw)
	}
	return r, nil
}
