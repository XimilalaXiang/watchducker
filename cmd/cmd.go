package cmd

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"watchducker/internal/core"
	"watchducker/internal/store"
	"watchducker/internal/types"
	"watchducker/internal/web"
	"watchducker/pkg/config"
	"watchducker/pkg/logger"
	"watchducker/pkg/notify"
	"watchducker/pkg/utils"

	"github.com/robfig/cron/v3"
)

// checkContainersByName 根据容器名称检查镜像更新
func checkContainersByName(ctx context.Context) {
	cfg := config.Get()
	RunChecker(ctx, func(checker *core.Checker) (*types.BatchCheckResult, error) {
		return checker.CheckByName(ctx, utils.UniqueDifference(cfg.ContainerNames(), cfg.DisabledContainers()))
	})
}

// checkContainersByLabel 根据标签检查镜像更新
func checkContainersByLabel(ctx context.Context) {
	labelKey, labelValue := "watchducker.update", "true"
	cfg := config.Get()

	RunChecker(ctx, func(checker *core.Checker) (*types.BatchCheckResult, error) {
		return checker.CheckByLabel(ctx, labelKey, labelValue, cfg.DisabledContainers())
	})
}

// checkAllContainers 检查所有容器的镜像更新
func checkAllContainers(ctx context.Context) {
	cfg := config.Get()

	RunChecker(ctx, func(checker *core.Checker) (*types.BatchCheckResult, error) {
		return checker.CheckAll(ctx, cfg.DisabledContainers())
	})
}

// checkContainersByLabelReversed 检查没有传入标签的容器
func checkContainersByLabelReversed(ctx context.Context) {
	labelKey, labelValue := "watchducker.update", "true"
	cfg := config.Get()

	RunChecker(ctx, func(checker *core.Checker) (*types.BatchCheckResult, error) {
		return checker.CheckByLabelReversed(ctx, labelKey, labelValue, cfg.DisabledContainers())
	})
}

// RunOnce 单次执行模式
func RunOnce(ctx context.Context) {
	cfg := config.Get()

	if len(cfg.ContainerNames()) > 0 {
		checkContainersByName(ctx)
	} else if cfg.CheckAll() {
		checkAllContainers(ctx)
	} else if cfg.CheckLabelReversed() {
		checkContainersByLabelReversed(ctx)
	} else if cfg.CheckLabel() {
		checkContainersByLabel(ctx)
	} else {
		config.PrintUsage()
	}
}

// RunCronScheduler 运行定时调度器（原有 CLI 模式，保持兼容）
func RunCronScheduler(ctx context.Context) {
	cfg := config.Get()

	c := cron.New()

	_, err := c.AddFunc(cfg.CronExpression(), func() {
		logger.Info("定时任务开始执行")
		RunOnce(ctx)
		logger.Info("定时任务执行完成")
	})

	if err != nil {
		logger.Fatal("无效的 cron 表达式 '%s': %v", cfg.CronExpression(), err)
	}

	logger.Info("定时任务已启动，cron 表达式: %s", cfg.CronExpression())
	logger.Info("按 Ctrl+C 停止定时任务")

	c.Start()
	select {}
}

// RunWithWebUI 启动带 Web UI 的模式（cron + HTTP 服务器）
func RunWithWebUI(ctx context.Context) {
	cfg := config.Get()

	dataDir := cfg.DataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		logger.Fatal("创建数据目录失败: %v", err)
	}

	dbPath := filepath.Join(dataDir, "watchducker.db")
	dataStore, err := store.New(dbPath)
	if err != nil {
		logger.Fatal("初始化数据库失败: %v", err)
	}
	defer dataStore.Close()

	srv := web.NewServer(dataStore, cfg.WebPort())
	srv.SetCronExpr(cfg.CronExpression())

	hasCheckMode := len(cfg.ContainerNames()) > 0 || cfg.CheckAll() || cfg.CheckLabel() || cfg.CheckLabelReversed()

	if hasCheckMode {
		c := cron.New()
		entryID, cronErr := c.AddFunc(cfg.CronExpression(), func() {
			logger.Info("定时任务开始执行")
			srv.RunScheduledCheck(ctx)
			logger.Info("定时任务执行完成")
			entries := c.Entries()
			for _, e := range entries {
				srv.SetNextRun(e.Next)
				break
			}
		})
		if cronErr != nil {
			logger.Fatal("无效的 cron 表达式 '%s': %v", cfg.CronExpression(), cronErr)
		}

		c.Start()
		logger.Info("定时任务已启动，cron 表达式: %s", cfg.CronExpression())

		entries := c.Entries()
		for _, e := range entries {
			if e.ID == entryID {
				srv.SetNextRun(e.Next)
				break
			}
		}

		go func() {
			time.Sleep(2 * time.Second)
			srv.RunScheduledCheck(ctx)
		}()
	}

	if err := srv.Start(); err != nil {
		logger.Fatal("Web 服务器启动失败: %v", err)
	}
}

// RunChecker 创建并运行检查器的通用函数
func RunChecker(ctx context.Context, checkFunc func(*core.Checker) (*types.BatchCheckResult, error)) {
	utils.PrintWelcome()

	cfg := config.Get()

	// 创建检查器
	checker, err := core.NewChecker(cfg.IncludeStopped())
	if err != nil {
		logger.Fatal("创建检查器失败: %v", err)
	}
	defer checker.Close()

	// 使用回调函数实时输出结果
	result, err := checkFunc(checker)
	if err != nil {
		logger.Error("容器检查过程中出现错误: %v", err)
	}

	if result == nil {
		return
	}

	if !cfg.NoRestart() && result.Summary.Updated > 0 {
		// 创建操作器
		operator, err := core.NewOperator()
		if err != nil {
			logger.Fatal("创建操作器失败: %v", err)
		}
		defer operator.Close()

		// 更新有镜像更新的容器
		err = operator.UpdateContainersByBatchCheckResult(ctx, result)
		if err != nil {
			logger.Error("容器更新过程中出现错误: %v", err)
		}

		// 如果启用了清理功能，清理悬空镜像
		if cfg.CleanUp() {
			if err := operator.CleanDanglingImages(ctx); err != nil {
				logger.Error("清理悬空镜像失败: %v", err)
			}
		}

		notify.Send("WatchDucker 镜像更新", utils.GetUpdateSummary(result))
	}

	// 输出最终结果
	utils.PrintContainerList(result.Containers)
	utils.PrintBatchSummary(result)
}
