// Learn Agent the Hard Way — 练习 6：工具注册表
//
// 练习 5 只有一个工具，switch 一下就分发完了。第二个工具来的时候，
// 你要改三个地方；第三个来的时候，你就烦了。烦，就是重构的信号。
// 这一章：tool 接口 + 注册表，从此加工具 = 加一行。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ---- bash：特权工具 ----

// 超时是双层的：不传用默认值，传了也有上限——上限保护的是你，不是模型。
const (
	defaultBashTimeout = 30 * time.Second
	maxBashTimeout     = 120 * time.Second
	maxBashOutput      = 8 * 1024 // 字节。工具结果会原样进上下文，必须封顶
)

// workDir 在启动时定死。每次 bash 调用都是一个全新进程，
// 模型在命令里 cd 到哪里，都随那个进程一起消失——工作目录由 harness 持有。
var workDir, _ = os.Getwd()

type bashTool struct{}

func (bashTool) definition() toolSpec {
	return toolSpec{
		Name: "bash",
		Description: "在系统 shell 里运行一条命令，返回 stdout 和 stderr 。 " +
			"命令总是在固定的工作目录执行 cd 不会跨调用生效。" +
			"默认 30 秒超时，如果预计时间更长就传入 timeout（整数秒，上限 120）。" +
			"能用 read_file/write_file/edit_file 完成的事儿，优先用那些专用工具。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "要执行的 shell 命令",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "超时秒数，可选，默认 30，上限 120",
				},
			},
			"required": []string{"command"},
		},
	}
}

func (bashTool) execute(args string) string {
	var in struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误：参数不是合法 json ：" + err.Error()
	}
	if strings.TrimSpace(in.Command) == "" {
		return "错误：command 不能为空"
	}
	dbt := defaultBashTimeout
	if in.Timeout > 0 {
		dbt = time.Duration(in.Timeout) * time.Second
		if dbt > maxBashTimeout {
			return fmt.Sprintf("错误：timeout 最大 %d 秒，要跑更久的命令，把它拆小或者放弃在一次调用里等它", int(maxBashTimeout.Seconds()))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbt)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput() // stdout 和 stderr 合在一起，模型两样都要看

	text := tail(string(out), maxBashOutput)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// 被杀也要把已产生的输出交回去——死前的输出往往就是死因
		return fmt.Sprintf("错误：命令超时 %s 被终止。被杀前的输出：\n%s", dbt, text)
	}
	if err != nil {
		// 非零退出不是异常，是情报：让模型自己读 exit code 和错误输出。
		return fmt.Sprintf("%s\n[%v]", text, err)
	}
	if text == "" {
		return "(命令成功，无输出)"
	}
	return text
}

// tail 超长时保留结尾——命令的结论和报错几乎总在最后，开头多半是刷屏。
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// len(s) - max 是计算倒数第 max 个字符的起始索引。
	cut := s[len(s)-max:]
	// strings.IndexByte(cut, '\n')：在刚截取的 cut 字符串中，寻找第一个换行符 \n 出现的位置索引 i。
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:] // 对齐到整行，别吐半截行
	}
	return fmt.Sprintf("[... 前面 %d 字节被截断，只保留结尾 ...]\n%s", len(s)-len(cut), cut)
}

// ---- 工具层 ----

// toolSpec 是发给模型的声明，和练习 5 相同。
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// tool 是每个工具要实现的接口：一份给模型看的声明，一个真正干活的函数。
// octo 里同名接口也是这两个方法——这不是巧合，是这件事的最小形状。
type tool interface {
	definition() toolSpec
	execute(args string) string
}

// readFileTool 就是练习 5 的 read_file，装进接口的壳。
type readFileTool struct{}

func (readFileTool) definition() toolSpec {
	return toolSpec{
		Name:        "read_file",
		Description: "读取一个本地文件，返回它的文本内容。修改文件前必须先用它读一遍。",
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
	}
}

func (readFileTool) execute(args string) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误：参数不是合法 json：" + err.Error()
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "错误：" + err.Error()
	}
	return string(data)
}

// writeFileTool 整个写入一个文件（不存在则创建，存在则覆盖）。
type writeFileTool struct{}

func (writeFileTool) definition() toolSpec {
	return toolSpec{
		Name:        "write_file",
		Description: "把内容完整写入一个文件。文件不存在就创建，存在就整个覆盖。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "目标文件路径",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "要写入的完整内容",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (writeFileTool) execute(args string) string {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误：参数不是合法 json：" + err.Error()
	}
	if err := os.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
		return "写入错误：" + err.Error()
	}
	return fmt.Sprintf("已写入 %s (%d) 字节", in.Path, len(in.Content))
}

// editFileTool 精确替换文件中的一段文本。octo 的设计原样蒸馏：
// old_string 必须在文件里恰好出现一次——多了说明定位不唯一，少了说明找错了，
// 两种都拒绝执行。这比"按行号改"可靠得多：行号在模型的记忆里会漂，原文不会。
type editFileTool struct{}

func (editFileTool) definition() toolSpec {
	return toolSpec{
		Name: "edit_file",
		Description: "在已有文件里做一次精确替换。old_string 必须与文件现有内容逐字一致，" +
			"且只出现一次——不唯一时请带上足够的上下文再试。文件必须已存在（创建用 write_file）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "目标文件路径",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "要找到的原文，必须唯一",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "替换的新文本，可以为空（等于删除）",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "是否全部替换，为 true 时替换全部出现， 不再要求唯一",
				},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

func (editFileTool) execute(args string) string {
	var in struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误: 参数不是合法 JSON: " + err.Error()
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "错误: " + err.Error()
	}
	text := string(data)
	switch n := strings.Count(text, in.OldString); {
	case in.OldString == "":
		return "错误: old_string 不能为空"
	case n == 0:
		return "错误: old_string 在文件里找不到——和 read_file 看到的原文逐字对一下"
	case n > 1 && in.ReplaceAll:
		return fmt.Sprintf("错误: old_string 出现了 %d 次，无法确定改哪一处——多带几行上下文让它唯一", n)
	}

	text = strings.Replace(text, in.OldString, in.NewString, 1)
	if err := os.WriteFile(in.Path, []byte(text), 0o644); err != nil {
		return "错误: " + err.Error()
	}
	return "已替换 " + in.Path + " 中的一处文本"
}

type listDirTool struct{}

func (listDirTool) definition() toolSpec {
	return toolSpec{
		Name:        "list_dir",
		Description: "列出当前目录下内容",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要列出的文件目录",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (listDirTool) execute(args string) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误： 参数不是合法 json：" + err.Error()
	}
	entries, err := os.ReadDir(in.Path)
	if err != nil {
		return "错误：列出文件失败：" + err.Error()
	}
	var sb strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		sb.WriteString("- " + name + "\n")
	}
	return sb.String()
}

// ---- 注册表层 ----

// registry 按名字分发工具调用，并在这一层安装横切纪律。
// 纪律装在注册表而不是某个工具里，因为它管的是工具**之间**的关系。
type registry struct {
	tools   map[string]tool
	order   []string        // 保证声明顺序 发给模型的列表要稳定
	hasRead map[string]bool // read-before-write 记录这个会话里读过哪些文件
}

func newRegistry(tools ...tool) *registry {
	r := &registry{tools: map[string]tool{}, hasRead: map[string]bool{}}
	for _, tool := range tools {
		spec := tool.definition()
		r.tools[spec.Name] = tool
		r.order = append(r.order, spec.Name)
	}
	return r
}

// definition() 生成发给模型的 tools 数组
func (r *registry) definitions() []map[string]any {
	var out []map[string]any
	for _, toolName := range r.order {
		tool := r.tools[toolName]
		out = append(out, map[string]any{
			"type":     "function",
			"function": tool.definition(),
		})
	}
	return out
}

// execute 查表分发执行 改文件的调用先过 read-before-write 检查
// 没读过不允许改文件  模型会先去读然后带着事实回来
func (r *registry) execute(name, args string) string {
	t, ok := r.tools[name]
	if !ok {
		return "错误：未知工具" + name
	}
	if name == "write_file" || name == "edit_file" {
		if path := pathOf(args); path != "" && fileExists(path) && !r.hasRead[path] {
			return "错误：" + path + " 已存在但这个会话里还没读到它，先用 read_file 看一眼，再来修改"
		}
	}

	result := t.execute(args)
	// 调用成功就记账 读过的文件可以改 刚写完的文件模型知道最新内容 也是读过
	if path := pathOf(args); path != "" && !strings.HasPrefix(result, "错误：") {
		r.hasRead[path] = true
	}

	return result
}

func pathOf(args string) string {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	return in.Path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- 协议层：和练习 5 相同 ----
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type request struct {
	Model     string           `json:"model"`
	Messages  []message        `json:"messages"`
	MaxTokens int              `json:"max_tokens,omitempty"`
	Tools     []map[string]any `json:"tools,omitempty"`
}

type response struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, `用法: ./ex06 "你的任务"`)
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

	// 全部工具在这里注册。加第四个工具 = 在这里加一行，别处一个字不用动。
	reg := newRegistry(
		readFileTool{},
		writeFileTool{},
		editFileTool{},
		listDirTool{},
		bashTool{},
	)

	history := []message{{Role: "user", Content: os.Args[1]}}
	// agent loop 的结构和练习 5 完全一样。变化只有两处：
	// 工具声明从注册表拿（reg.definitions），分发交给注册表（reg.execute）。
	const maxRounds = 10
	for round := 1; round <= maxRounds; round++ {
		r, err := send(base, apiKey, model, history, reg.definitions())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		msg := r.Choices[0].Message
		history = append(history, msg)
		if r.Choices[0].FinishReason != "tool_calls" {
			fmt.Println(msg.Content)
			fmt.Fprintf(os.Stderr, "\n[共 %d 轮 . 最后一轮输入 %d tokens . finish_reason=%s]\n", round, maxRounds, r.Choices[0].FinishReason)
			return
		}

		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(os.Stderr, "[round %d] %s(%s)\n", round, tc.Function.Name, tc.Function.Arguments)
			result := reg.execute(tc.Function.Name, tc.Function.Arguments)
			history = append(history, message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}
	fmt.Errorf("达到 %d 轮上限，停止", maxRounds)
	os.Exit(1)
}

func send(base, apiKey, model string, history []message, tools []map[string]any) (response, error) {
	var r response
	body, _ := json.Marshal(request{
		Model:     model,
		Messages:  history,
		MaxTokens: 4096,
		Tools:     tools,
	})
	req, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewBuffer(body))
	if err != nil {
		return r, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return r, fmt.Errorf("请求失败： %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// 判断响应状态码是否 200
	if resp.StatusCode != 200 {
		return r, fmt.Errorf("HTTP %d - %s", resp.StatusCode, raw)
	}

	// 判断响应体能否json解析
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("解析失败 %w\n,原始响应：%s", err, raw)
	}

	// 判断响应的 Error 字段是否为空
	if r.Error != nil {
		return r, fmt.Errorf("API 错误：[%s]: %s", r.Error.Type, r.Error.Message)
	}

	// 判断 r.Choices 的长度是否不为 0
	if len(r.Choices) == 0 {
		return r, fmt.Errorf("空响应：%s", raw)
	}

	return r, nil
}
