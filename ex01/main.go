package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type message struct {
	Role    string `json:"role"` // "system" / "user" / "assistant"
	Content string `json:"content"`
}

type request struct {
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	MaxTokens           int       `json:"max_tokens,omitempty"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"` // openai 比较新的模型使用
}

type response struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"` // "stop" | "length" | "tool_calls" …
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
		fmt.Fprintln(os.Stderr, `用法: ./ex01 "你的问题"`)
		os.Exit(1)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	model := os.Getenv("MODEL")
	if apiKey == "" || model == "" {
		fmt.Fprintln(os.Stderr, "需要环境变量 OPENAI_API_KEY 和 MODEL")
		fmt.Fprintln(os.Stderr, `例: export OPENAI_API_KEY=<YOUR_API_KEY>`)
		fmt.Fprintln(os.Stderr, `    export MODEL=<YOUR_MODEL>`)
		fmt.Fprintln(os.Stderr, `    export OPENAI_BASE_URL=https://api.deepseek.com/v1`)
		os.Exit(1)
	}
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	body, _ := json.Marshal(request{
		Model:               model,
		MaxTokens:           1000,
		MaxCompletionTokens: 1000,
		Messages: []message{
			{
				Role:    "user",
				Content: os.Args[1],
			},
		},
	})

	req, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "请求失败: ", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		fmt.Fprintf(os.Stderr, "解析失败: %v\n原始响应: %s\n", err, raw)
		os.Exit(1)
	}
	// fmt.Printf("%s\n", raw)
	if r.Error != nil {
		fmt.Fprintf(os.Stderr, "API 错误 [%s]: %s\n", r.Error.Type, r.Error.Message)
		os.Exit(1)
	}
	if len(r.Choices) == 0 {
		fmt.Fprintf(os.Stderr, "空响应: %s\n", raw)
		os.Exit(1)
	}
	fmt.Println(r.Choices[0].Message.Content)
	fmt.Fprintf(os.Stderr, "\n[输入 %d tokens . 输出 %d tokens . finish_reason=%s]\n", r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Choices[0].
		FinishReason)
}
