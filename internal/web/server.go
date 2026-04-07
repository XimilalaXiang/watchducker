package web

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"watchducker/internal/core"
	"watchducker/internal/store"
	"watchducker/internal/types"
	"watchducker/pkg/config"
	"watchducker/pkg/logger"
	"watchducker/pkg/notify"
	"watchducker/pkg/utils"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

//go:embed all:static
var staticFS embed.FS

type Server struct {
	store    *store.Store
	hub      *WSHub
	router   *gin.Engine
	port     int
	checking sync.Mutex
	cronExpr string
	nextRun  time.Time
	auth     *authManager
}

func NewServer(dataStore *store.Store, port int) *Server {
	gin.SetMode(gin.ReleaseMode)

	s := &Server{
		store:  dataStore,
		hub:    NewWSHub(),
		router: gin.New(),
		port:   port,
		auth:   newAuthManager(),
	}

	s.router.Use(gin.Recovery())
	s.setupRoutes()
	return s
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) setupRoutes() {
	s.router.POST("/api/login", s.handleLogin)
	s.router.POST("/api/logout", s.handleLogout)
	s.router.GET("/api/auth-status", s.handleAuthStatus)

	api := s.router.Group("/api")
	api.Use(authMiddleware(s.auth))
	{
		api.GET("/status", s.handleStatus)
		api.GET("/containers", s.handleContainers)
		api.POST("/check", s.handleCheck)
		api.POST("/containers/:name/check", s.handleCheckOne)
		api.POST("/containers/:name/update", s.handleUpdateOne)
		api.POST("/update", s.handleUpdateAll)
		api.POST("/clean", s.handleClean)
		api.GET("/history", s.handleHistory)
		api.GET("/history/:id", s.handleHistoryDetail)
		api.GET("/settings", s.handleGetSettings)
	}

	s.router.GET("/ws", s.handleWebSocket)

	distFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		logger.Error("Failed to load static files: %v", err)
		return
	}

	s.router.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		f, err := http.FS(distFS).Open(path)
		if err != nil {
			// SPA fallback: serve index.html for unmatched routes
			c.FileFromFS("index.html", http.FS(distFS))
			return
		}
		f.Close()
		c.FileFromFS(path, http.FS(distFS))
	})
}

func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	logger.Info("Web UI 已启动: http://0.0.0.0%s", addr)
	return s.router.Run(addr)
}

func (s *Server) SetNextRun(t time.Time) {
	s.nextRun = t
}

func (s *Server) SetCronExpr(expr string) {
	s.cronExpr = expr
}

// --- Auth Handlers ---

func (s *Server) handleAuthStatus(c *gin.Context) {
	cfg := config.Get()
	if !cfg.AuthEnabled() {
		c.JSON(http.StatusOK, gin.H{"auth_required": false, "logged_in": true})
		return
	}

	token, err := c.Cookie(cookieName)
	loggedIn := err == nil && s.auth.validateToken(token)
	c.JSON(http.StatusOK, gin.H{"auth_required": true, "logged_in": loggedIn})
}

func (s *Server) handleLogin(c *gin.Context) {
	cfg := config.Get()
	if !cfg.AuthEnabled() {
		c.JSON(http.StatusOK, gin.H{"message": "认证未启用"})
		return
	}

	if s.auth.isLocked() {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录尝试过多，请稍后再试"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	if !s.auth.checkCredentials(req.Username, req.Password) {
		locked := s.auth.recordFail()
		msg := "用户名或密码错误"
		if locked {
			msg = "登录尝试过多，账户已锁定15分钟"
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": msg})
		return
	}

	s.auth.resetFails()
	token, err := s.auth.createSession()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建会话失败"})
		return
	}

	c.SetCookie(cookieName, token, int(tokenExpiry.Seconds()), "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"message": "登录成功", "token": token})
}

func (s *Server) handleLogout(c *gin.Context) {
	token, err := c.Cookie(cookieName)
	if err == nil {
		s.auth.revokeToken(token)
	}
	c.SetCookie(cookieName, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"message": "已登出"})
}

// --- Handlers ---

func (s *Server) handleStatus(c *gin.Context) {
	last := s.store.GetLastCheckResult()
	states := s.store.GetContainerStates()

	totalContainers := len(states)
	updateAvailable := 0
	failed := 0
	localImage := 0
	upToDate := 0
	for _, st := range states {
		switch st.Status {
		case "update_available":
			updateAvailable++
		case "failed":
			failed++
		case "local_image":
			localImage++
		case "up_to_date":
			upToDate++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total_containers":  totalContainers,
		"update_available":  updateAvailable,
		"failed":            failed,
		"local_image":       localImage,
		"up_to_date":        upToDate,
		"cron_expression":   s.cronExpr,
		"next_run":          s.nextRun,
		"last_check":        last,
	})
}

func (s *Server) handleContainers(c *gin.Context) {
	states := s.store.GetContainerStates()
	c.JSON(http.StatusOK, gin.H{"containers": states})
}

func (s *Server) handleCheck(c *gin.Context) {
	if !s.checking.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "检查正在进行中"})
		return
	}

	go func() {
		defer s.checking.Unlock()
		s.runCheck(context.Background(), "manual")
	}()

	c.JSON(http.StatusAccepted, gin.H{"message": "检查已开始"})
}

func (s *Server) handleCheckOne(c *gin.Context) {
	name := c.Param("name")
	if !s.checking.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "检查正在进行中"})
		return
	}

	go func() {
		defer s.checking.Unlock()
		s.runCheckByName(context.Background(), name)
	}()

	c.JSON(http.StatusAccepted, gin.H{"message": fmt.Sprintf("容器 %s 检查已开始", name)})
}

func (s *Server) handleUpdateOne(c *gin.Context) {
	name := c.Param("name")
	if !s.checking.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "操作正在进行中"})
		return
	}

	go func() {
		defer s.checking.Unlock()
		s.runUpdateByName(context.Background(), name)
	}()

	c.JSON(http.StatusAccepted, gin.H{"message": fmt.Sprintf("容器 %s 更新已开始", name)})
}

func (s *Server) handleUpdateAll(c *gin.Context) {
	if !s.checking.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "操作正在进行中"})
		return
	}

	go func() {
		defer s.checking.Unlock()
		s.runUpdateAll(context.Background())
	}()

	c.JSON(http.StatusAccepted, gin.H{"message": "批量更新已开始"})
}

func (s *Server) handleClean(c *gin.Context) {
	go func() {
		operator, err := core.NewOperator()
		if err != nil {
			logger.Error("创建操作器失败: %v", err)
			return
		}
		defer operator.Close()
		if err := operator.CleanDanglingImages(context.Background()); err != nil {
			logger.Error("清理悬空镜像失败: %v", err)
		}
		s.hub.Broadcast(gin.H{"type": "clean_done"})
	}()
	c.JSON(http.StatusAccepted, gin.H{"message": "清理已开始"})
}

func (s *Server) handleHistory(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	history, err := s.store.GetCheckHistory(limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	total, _ := s.store.GetHistoryCount()
	c.JSON(http.StatusOK, gin.H{"history": history, "total": total})
}

func (s *Server) handleHistoryDetail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	h, err := s.store.GetCheckHistoryByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, h)
}

func (s *Server) handleGetSettings(c *gin.Context) {
	cfg := config.Get()
	c.JSON(http.StatusOK, gin.H{
		"cron_expression":    cfg.CronExpression(),
		"check_all":          cfg.CheckAll(),
		"check_label":        cfg.CheckLabel(),
		"check_label_reversed": cfg.CheckLabelReversed(),
		"no_restart":         cfg.NoRestart(),
		"clean_up":           cfg.CleanUp(),
		"include_stopped":    cfg.IncludeStopped(),
	})
}

func (s *Server) handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Error("WebSocket 升级失败: %v", err)
		return
	}

	s.hub.Register(conn)

	go func() {
		defer s.hub.Unregister(conn)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}()
}

// --- Core operations ---

func (s *Server) runCheck(ctx context.Context, checkType string) {
	startTime := time.Now()
	s.hub.Broadcast(gin.H{"type": "check_start"})

	cfg := config.Get()
	checker, err := core.NewChecker(cfg.IncludeStopped())
	if err != nil {
		logger.Error("创建检查器失败: %v", err)
		s.hub.Broadcast(gin.H{"type": "check_error", "error": err.Error()})
		return
	}
	defer checker.Close()

	var result *types.BatchCheckResult
	if cfg.CheckAll() {
		result, err = checker.CheckAll(ctx, cfg.DisabledContainers())
	} else if cfg.CheckLabelReversed() {
		result, err = checker.CheckByLabelReversed(ctx, "watchducker.update", "true", cfg.DisabledContainers())
	} else if cfg.CheckLabel() {
		result, err = checker.CheckByLabel(ctx, "watchducker.update", "true", cfg.DisabledContainers())
	} else if len(cfg.ContainerNames()) > 0 {
		result, err = checker.CheckByName(ctx, cfg.ContainerNames())
	} else {
		result, err = checker.CheckAll(ctx, cfg.DisabledContainers())
	}

	if err != nil {
		logger.Error("检查过程中出现错误: %v", err)
	}

	s.processCheckResult(result, checkType, startTime)
}

func (s *Server) runCheckByName(ctx context.Context, name string) {
	startTime := time.Now()
	s.hub.Broadcast(gin.H{"type": "check_start", "container": name})

	checker, err := core.NewChecker(config.Get().IncludeStopped())
	if err != nil {
		logger.Error("创建检查器失败: %v", err)
		return
	}
	defer checker.Close()

	result, err := checker.CheckByName(ctx, []string{name})
	if err != nil {
		logger.Error("检查容器 %s 失败: %v", name, err)
	}

	s.processCheckResult(result, "manual", startTime)
}

func (s *Server) runUpdateByName(ctx context.Context, name string) {
	s.hub.Broadcast(gin.H{"type": "update_start", "container": name})

	checker, err := core.NewChecker(config.Get().IncludeStopped())
	if err != nil {
		logger.Error("创建检查器失败: %v", err)
		return
	}
	defer checker.Close()

	result, err := checker.CheckByName(ctx, []string{name})
	if err != nil || result == nil || result.Summary.Updated == 0 {
		s.hub.Broadcast(gin.H{"type": "update_done", "container": name, "updated": false})
		return
	}

	operator, err := core.NewOperator()
	if err != nil {
		logger.Error("创建操作器失败: %v", err)
		return
	}
	defer operator.Close()

	if err := operator.UpdateContainersByBatchCheckResult(ctx, result); err != nil {
		logger.Error("更新容器 %s 失败: %v", name, err)
		s.hub.Broadcast(gin.H{"type": "update_error", "container": name, "error": err.Error()})
		return
	}

	s.hub.Broadcast(gin.H{"type": "update_done", "container": name, "updated": true})
}

func (s *Server) runUpdateAll(ctx context.Context) {
	s.runCheck(ctx, "manual")

	states := s.store.GetContainerStates()
	var toUpdate []string
	for _, st := range states {
		if st.Status == "update_available" {
			toUpdate = append(toUpdate, st.Name)
		}
	}

	if len(toUpdate) == 0 {
		s.hub.Broadcast(gin.H{"type": "update_done", "updated": 0})
		return
	}

	checker, err := core.NewChecker(config.Get().IncludeStopped())
	if err != nil {
		return
	}
	defer checker.Close()

	result, err := checker.CheckByName(ctx, toUpdate)
	if err != nil || result == nil {
		return
	}

	operator, err := core.NewOperator()
	if err != nil {
		return
	}
	defer operator.Close()

	if err := operator.UpdateContainersByBatchCheckResult(ctx, result); err != nil {
		logger.Error("批量更新失败: %v", err)
	}

	if config.Get().CleanUp() {
		operator.CleanDanglingImages(ctx)
	}

	notify.Send("WatchDucker 镜像更新", utils.GetUpdateSummary(result))
	s.hub.Broadcast(gin.H{"type": "update_done", "updated": len(toUpdate)})
}

func isLocalImageError(errMsg string) bool {
	localPatterns := []string{
		"pull access denied",
		"repository does not exist",
		"未关联任何标签或摘要",
		"无法解析",
	}
	lower := strings.ToLower(errMsg)
	for _, p := range localPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func (s *Server) processCheckResult(result *types.BatchCheckResult, checkType string, startTime time.Time) {
	if result == nil {
		return
	}

	duration := time.Since(startTime)

	var containerStates []*store.ContainerState
	imageStatusMap := make(map[string]*types.ImageCheckResult)
	for _, img := range result.Images {
		imageStatusMap[img.Name] = img
	}

	for _, container := range result.Containers {
		state := &store.ContainerState{
			Name:      container.Name,
			Image:     container.Image,
			LastCheck: time.Now(),
			Status:    "unknown",
		}

		if img, ok := imageStatusMap[container.Image]; ok {
			state.LocalHash = img.LocalHash
			state.RemoteHash = img.RemoteHash
			if img.Error != "" {
				if isLocalImageError(img.Error) {
					state.Status = "local_image"
				} else {
					state.Status = "failed"
				}
				state.Error = img.Error
			} else if img.IsUpdated {
				state.Status = "update_available"
			} else {
				state.Status = "up_to_date"
			}
		}

		containerStates = append(containerStates, state)
	}

	s.store.UpdateContainerStates(containerStates)

	history := &store.CheckHistory{
		Type:       checkType,
		StartedAt:  startTime,
		DurationMs: duration.Milliseconds(),
		Total:      result.Summary.TotalContainers,
		Updated:    result.Summary.Updated,
		Failed:     result.Summary.Failed,
		UpToDate:   result.Summary.UpToDate,
		Details:    store.MarshalDetails(result.Images),
	}

	if err := s.store.SaveCheckHistory(history); err != nil {
		logger.Error("保存检查历史失败: %v", err)
	}

	s.hub.Broadcast(gin.H{
		"type":    "check_done",
		"total":   result.Summary.TotalContainers,
		"updated": result.Summary.Updated,
		"failed":  result.Summary.Failed,
		"duration_ms": duration.Milliseconds(),
	})
}

// RunScheduledCheck is called by the cron scheduler
func (s *Server) RunScheduledCheck(ctx context.Context) {
	if !s.checking.TryLock() {
		logger.Warn("定时检查跳过：上一次检查仍在进行中")
		return
	}
	defer s.checking.Unlock()

	s.runCheck(ctx, "scheduled")

	cfg := config.Get()
	if !cfg.NoRestart() {
		states := s.store.GetContainerStates()
		hasUpdates := false
		for _, st := range states {
			if st.Status == "update_available" {
				hasUpdates = true
				break
			}
		}
		if hasUpdates {
			s.runUpdateAll(ctx)
		}
	}
}
