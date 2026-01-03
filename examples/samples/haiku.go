// -------------------------------------------------
// Package samples
// Author: hanzhi
// Date: 2026/1/3
// -------------------------------------------------

package main

import (
	"context"
	"fmt"
	veagent "github.com/volcengine/veadk-go/agent/llmagent"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
	"log"
)

func main() {
	ctx := context.Background()

	agentDescription := `一个专业的俳句大师，能用5-7-5俳句来回答任何问题，非常有逼格`
	agentInstruction := `你是一个专业的俳句大师，你能够用5-7-5的中文样式的俳句来回答任何用户的问题
		不管用户问什么，你都能回答的非常合理。`

	haikuAgentConfigArr := &veagent.Config{
		Config: llmagent.Config{
			Name:        "haiku_agent",
			Description: agentDescription,
			Instruction: agentInstruction,
		},
	}
	haikuAgent, err := veagent.New(
		haikuAgentConfigArr,
	)
	if err != nil {
		fmt.Printf("NewLlmAgent myagent failed: %v\n", err)
		return
	}
	session_service := session.InMemoryService()
	_, err = session_service.Create(
		ctx, &session.CreateRequest{
			AppName:   "my_app",
			UserID:    "user_id",
			SessionID: "session_id",
		},
	)
	if err != nil {
		fmt.Printf("Create session failed: %v\n", err)
	}
	r, err := runner.New(runner.Config{
		AppName:        "my_app",
		Agent:          haikuAgent,
		SessionService: session_service,
	})
	if err != nil {
		fmt.Printf("NewLlmAgent myagent failed: %v\n", err)
	}
	msg := genai.NewContentFromText("今天晚上咱们吃啥", genai.RoleUser)
	stream := r.Run(ctx, "user_id", "session_id", msg, agent.RunConfig{})

	// 处理响应
	// 这是开启thinking的，所以很正常
	for event, err := range stream {
		if err != nil {
			log.Printf("Error: %v", err)
			continue
		}
		fmt.Printf("%v", event.Content.Parts[1].Text)
	}

}
