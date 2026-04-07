package web

import (
	"sync"

	"github.com/gorilla/websocket"
)

type WSHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
}

func NewWSHub() *WSHub {
	return &WSHub{
		clients: make(map[*websocket.Conn]struct{}),
	}
}

func (h *WSHub) Register(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
}

func (h *WSHub) Unregister(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
	conn.Close()
}

func (h *WSHub) Broadcast(msg interface{}) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn := range h.clients {
		if err := conn.WriteJSON(msg); err != nil {
			go h.Unregister(conn)
		}
	}
}
