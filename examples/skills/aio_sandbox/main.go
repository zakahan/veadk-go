// Command aio_sandbox runs an Agent whose Skills and file/process tools live
// in an AgentKit sandbox bound to the ADK session.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	veagent "github.com/volcengine/veadk-go/v2/agent/llmagent"
	verunner "github.com/volcengine/veadk-go/v2/runner"
	"google.golang.org/adk/v2/agent"
)

func main() {
	paths := flag.String("skills", "", "comma-separated absolute Skill directories inside the sandbox; empty uses the default catalog")
	prompt := flag.String("prompt", "Use bash to print the current working directory.", "user request")
	sessionID := flag.String("session", "aio-example", "ADK session ID")
	flag.Parse()
	var roots []string
	if *paths != "" {
		roots = strings.Split(*paths, ",")
	}
	a, err := veagent.New(&veagent.Config{SkillsMode: "aio_sandbox", Skills: roots})
	if err != nil {
		fail(err)
	}
	r, err := verunner.New(verunner.Config{AppName: "aio_example", Agent: a, AutoCreateSession: true})
	if err != nil {
		fail(err)
	}
	response, err := r.RunText(context.Background(), "example_user", *sessionID, *prompt, agent.RunConfig{})
	if err != nil {
		fail(err)
	}
	fmt.Println(response)
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
