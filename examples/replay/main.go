package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"
	"github.com/voicefoxai/langfuse-gosdk/langfuse"
)

func main() {
	godotenv.Load()
	fmt.Println("========================================")
	fmt.Println("  Langfuse Replay Example")
	fmt.Println("========================================")

	// 初始化配置
	config := langfuse.DefaultConfig()
	config.PublicKey = os.Getenv("LANGFUSE_PUBLIC_KEY")
	config.SecretKey = os.Getenv("LANGFUSE_SECRET_KEY")
	config.BaseURL = os.Getenv("LANGFUSE_BASE_URL")
	config.Debug = true

	client, err := langfuse.NewClient(config)
	if err != nil {
		log.Fatalf("Failed to create Langfuse client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	traceID := "5fe14781-e87e-479e-838b-05e18035b3e8"
	sessionID := "8bc2f3c7-1f43-43cc-a35b-71c185dbcdc7"

	fmt.Printf("Trace ID: %s\n", traceID)
	fmt.Printf("Session ID: %s\n\n", sessionID)

	// 1. 根据 sessionID 获取整个 session（包含所有 traces）
	fmt.Println("Fetching session data...")
	session, err := client.GetSession(ctx, langfuse.GetSessionParams{
		SessionID: sessionID,
	})
	if err != nil {
		log.Fatalf("Failed to fetch session: %v", err)
	}
	fmt.Printf("Found session with %d traces\n\n", len(session.Traces))

	// 2. 组装历史上下文
	contextMessages, err := buildContextFromSession(ctx, client, sessionID, traceID)
	if err != nil {
		log.Fatalf("Failed to build context: %v", err)
	}

	fmt.Printf("\n========================================\n")
	fmt.Printf("Context Assembly Complete\n")
	fmt.Printf("========================================\n")
	fmt.Printf("Total context messages: %d\n\n", len(contextMessages))

	// 打印组装好的上下文
	fmt.Printf("========================================\n")
	fmt.Printf("Context Messages:\n")
	fmt.Printf("========================================\n")
	for i, msg := range contextMessages {
		fmt.Printf("Message %d:\n", i+1)

		if role, ok := msg["role"]; ok && role != nil {
			fmt.Printf("  Role: %v\n", role)
		} else {
			fmt.Printf("  Role: (missing)\n")
		}

		if content, ok := msg["content"]; ok {
			if contentStr, ok := content.(string); ok {
				if len(contentStr) > 200 {
					fmt.Printf("  Content: %s... (truncated, total length: %d)\n", contentStr[:200], len(contentStr))
				} else {
					fmt.Printf("  Content: %s\n", contentStr)
				}
			} else {
				fmt.Printf("  Content: %v\n", content)
			}
		}

		if toolCalls, ok := msg["tool_calls"]; ok && toolCalls != nil {
			fmt.Printf("  Tool Calls: %v\n", toolCalls)
		}

		if toolCallID, ok := msg["tool_call_id"]; ok && toolCallID != nil {
			fmt.Printf("  Tool Call ID: %v\n", toolCallID)
		}

		fmt.Println()
	}

	// 3. 发送请求到 replay API
	fmt.Printf("========================================\n")
	fmt.Printf("Sending Request to Replay API\n")
	fmt.Printf("========================================\n")

	templateData, err := os.ReadFile("examples/replay/request_template.json")
	if err != nil {
		log.Fatalf("Failed to read request template: %v", err)
	}

	var requestBody map[string]any
	if err := json.Unmarshal(templateData, &requestBody); err != nil {
		log.Fatalf("Failed to parse request body template: %v", err)
	}

	requestBody["history"] = contextMessages

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		log.Fatalf("Failed to marshal request body: %v", err)
	}

	replayURL := "http://localhost:9001/api/v1/replay"
	fmt.Printf("Sending POST request to: %s\n", replayURL)
	fmt.Printf("Request body size: %d bytes\n", len(requestJSON))

	resp, err := http.Post(replayURL, "application/json", bytes.NewBuffer(requestJSON))
	if err != nil {
		log.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response: %v", err)
	}

	fmt.Printf("\n========================================\n")
	fmt.Printf("Response Status: %s\n", resp.Status)
	fmt.Printf("========================================\n")

	if resp.StatusCode == http.StatusOK {
		var responseData map[string]any
		if err := json.Unmarshal(respBody, &responseData); err != nil {
			fmt.Printf("Response (raw): %s\n", string(respBody))
		} else {
			prettyJSON, err := json.MarshalIndent(responseData, "", "  ")
			if err != nil {
				fmt.Printf("Response: %+v\n", responseData)
			} else {
				fmt.Printf("Response:\n%s\n", string(prettyJSON))
			}
		}
	} else {
		fmt.Printf("Error response: %s\n", string(respBody))
	}
}

// buildContextFromSession 根据 sessionID 和 traceID 动态组装历史上下文
// 从 session 中获取所有 traces，收集从第 0 个到目标 trace 的所有 generations 的 input/output
func buildContextFromSession(ctx context.Context, client *langfuse.Client, sessionID, targetTraceID string) ([]map[string]any, error) {
	contextMessages := []map[string]any{}

	// 获取 session 数据
	session, err := client.GetSession(ctx, langfuse.GetSessionParams{
		SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch session: %w", err)
	}

	// 找到目标 trace 在列表中的位置
	targetIndex := -1
	for i, traceSummary := range session.Traces {
		if traceSummary.ID == targetTraceID {
			targetIndex = i
			break
		}
	}

	if targetIndex == -1 {
		return nil, fmt.Errorf("target trace %s not found in session", targetTraceID)
	}

	fmt.Printf("Target trace found at index %d, will process %d traces in reverse order\n", targetIndex, targetIndex+1)

	// Session 的 traces 是时间逆序的（最新的在前）
	// 需要反向遍历：从目标 trace 往前遍历到第 0 个（时间从旧到新）
	for i := targetIndex; i >= 0; i-- {
		traceSummary := session.Traces[i]
		fmt.Printf("Processing trace %d/%d (ID: %s)...\n", targetIndex-i+1, targetIndex+1, traceSummary.ID)

		// 为每个 trace 单独获取完整数据（包含 observations）
		trace, err := client.GetTrace(ctx, langfuse.GetTraceParams{
			TraceID: traceSummary.ID,
		})
		if err != nil {
			fmt.Printf("  Failed to get trace details: %v, skipping\n", err)
			continue
		}

		// 找到当前 trace 的第一个 generation
		var generation *langfuse.ObservationDetails
		for j := 0; j < len(trace.Observations); j++ {
			if trace.Observations[j].Type == "GENERATION" {
				generation = &trace.Observations[j]
				break
			}
		}

		if generation == nil {
			fmt.Printf("  No generation found in trace %s, skipping\n", trace.ID)
			continue
		}

		fmt.Printf("  Found generation (ID: %s)\n", generation.ID)

		// 添加 generation 的 input
		if generation.Input != nil {
			msgCount := addMessagesToContext(&contextMessages, generation.Input, "input")
			fmt.Printf("  Added %d messages from input\n", msgCount)
		}

		// 添加 generation 的 output
		if generation.Output != nil {
			msgCount := addMessagesToContext(&contextMessages, generation.Output, "output")
			fmt.Printf("  Added %d messages from output\n", msgCount)
		}
	}

	return contextMessages, nil
}

// addMessagesToContext 将 input 或 output 数据添加到上下文消息列表中
// 会过滤掉无效的 tool 消息（前面没有 tool_calls 的 tool 消息）
// 返回添加的消息数量
func addMessagesToContext(contextMessages *[]map[string]any, data any, dataType string) int {
	count := 0

	// 尝试将数据断言为数组
	if dataArray, ok := data.([]any); ok {
		for _, item := range dataArray {
			if msgMap, ok := item.(map[string]any); ok {

				*contextMessages = append(*contextMessages, msgMap)
				count++

			}
		}
	} else if dataMap, ok := data.(map[string]any); ok {
		// 数据是单个对象

		*contextMessages = append(*contextMessages, dataMap)
		count++

	} else {
		// 数据是其他类型，根据类型添加默认角色
		role := "user"
		if dataType == "output" {
			role = "assistant"
		}
		msg := map[string]any{
			"role":    role,
			"content": data,
		}
		*contextMessages = append(*contextMessages, msg)
		count++
	}

	return count
}
