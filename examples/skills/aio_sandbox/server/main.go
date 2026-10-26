// Command server exposes ADK session routes with AIO sandbox lifecycle.
// VEADK_AIO_IDLE_TIMEOUT and VEADK_AIO_PAUSED_RETENTION configure cleanup.
package main

import (
	"context"
	"log"
	"os"
	"strings"

	veagent "github.com/volcengine/veadk-go/v2/agent/llmagent"
	"github.com/volcengine/veadk-go/v2/apps"
	"github.com/volcengine/veadk-go/v2/apps/agentkit_server_app"
	"github.com/volcengine/veadk-go/v2/integrations/agentkit"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

func main() {
	var spaces []string
	if value := strings.TrimSpace(os.Getenv("SKILL_SPACE_ID")); value != "" {
		spaces = strings.Split(value, ",")
	}
	root, err := veagent.New(&veagent.Config{SkillsMode: "aio_sandbox", Skills: spaces})
	if err != nil {
		log.Fatal(err)
	}
	store := session.InMemoryService()
	lifecycle, err := agentkit.NewAIOLifecycle(agentkit.AIOLifecycleConfig{
		SessionService: store,
		SkillSpaceIDs:  spaces,
		// Optional: IdleTimeout and PausedRetention override the environment.
	})
	if err != nil {
		log.Fatal(err)
	}
	app := agentkit_server_app.NewAgentkitServerApp(
		apps.DefaultApiConfig(), agentkit_server_app.WithAIOLifecycle(lifecycle),
	)
	if err := app.Run(context.Background(), &apps.RunConfig{
		AgentLoader: agent.NewSingleLoader(root), SessionService: store,
	}); err != nil {
		log.Fatal(err)
	}
}
