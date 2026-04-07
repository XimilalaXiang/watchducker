package main

import (
	"context"
	"watchducker/cmd"
	"watchducker/pkg/config"
	"watchducker/pkg/logger"
)

func main() {
	if err := config.Load(); err != nil {
		logger.Fatal("初始化失败: %v", err)
	}

	ctx := context.Background()

	if config.Get().RunOnce() {
		cmd.RunOnce(ctx)
		return
	}

	cmd.RunWithWebUI(ctx)
}
