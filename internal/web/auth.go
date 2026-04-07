package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"watchducker/pkg/config"

	"github.com/gin-gonic/gin"
)

const (
	tokenExpiry   = 24 * time.Hour
	cookieName    = "wd_token"
	maxLoginFails = 10
	lockDuration  = 15 * time.Minute
)

type authSession struct {
	token     string
	expiresAt time.Time
}

type authManager struct {
	mu          sync.RWMutex
	sessions    map[string]*authSession
	failCount   int
	lockedUntil time.Time
}

func newAuthManager() *authManager {
	return &authManager{
		sessions: make(map[string]*authSession),
	}
}

func (a *authManager) generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (a *authManager) createSession() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	token, err := a.generateToken()
	if err != nil {
		return "", err
	}

	a.sessions[token] = &authSession{
		token:     token,
		expiresAt: time.Now().Add(tokenExpiry),
	}

	a.cleanExpired()
	return token, nil
}

func (a *authManager) validateToken(token string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	session, ok := a.sessions[token]
	if !ok {
		return false
	}
	return time.Now().Before(session.expiresAt)
}

func (a *authManager) revokeToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *authManager) cleanExpired() {
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.expiresAt) {
			delete(a.sessions, k)
		}
	}
}

func (a *authManager) isLocked() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().Before(a.lockedUntil)
}

func (a *authManager) recordFail() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failCount++
	if a.failCount >= maxLoginFails {
		a.lockedUntil = time.Now().Add(lockDuration)
		a.failCount = 0
		return true
	}
	return false
}

func (a *authManager) resetFails() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failCount = 0
}

func (a *authManager) checkCredentials(username, password string) bool {
	cfg := config.Get()
	return subtle.ConstantTimeCompare([]byte(username), []byte(cfg.AuthUsername())) == 1 &&
		subtle.ConstantTimeCompare([]byte(password), []byte(cfg.AuthPassword())) == 1
}

func authMiddleware(am *authManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !config.Get().AuthEnabled() {
			c.Next()
			return
		}

		token, err := c.Cookie(cookieName)
		if err == nil && am.validateToken(token) {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			if am.validateToken(authHeader[7:]) {
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
	}
}
