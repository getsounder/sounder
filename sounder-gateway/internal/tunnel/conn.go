package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/getsounder/sounder-gateway/internal/protocol"
)

var ErrClosed = errors.New("tunnel closed")

type EventHandler func(workerID, sessionID string, ev json.RawMessage)

type Conn struct {
	ws       *websocket.Conn
	workerID string

	writeMu sync.Mutex
	pending sync.Map // id -> chan protocol.Frame
	closed  atomic.Bool

	onEvent EventHandler
	onClose func(workerID string)
}

func New(ws *websocket.Conn, onEvent EventHandler, onClose func(string)) *Conn {
	return &Conn{ws: ws, onEvent: onEvent, onClose: onClose}
}

func (c *Conn) WorkerID() string { return c.workerID }

func (c *Conn) SetWorkerID(id string) { c.workerID = id }

func (c *Conn) Write(f protocol.Frame) error {
	if c.closed.Load() {
		return ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(f)
}

// Call 发送 RPC 并等待结果。超时由 ctx 控制。
func (c *Conn) Call(ctx context.Context, f protocol.Frame) (protocol.Frame, error) {
	if f.ID == "" {
		return protocol.Frame{}, errors.New("rpc frame requires id")
	}
	ch := make(chan protocol.Frame, 1)
	c.pending.Store(f.ID, ch)
	defer c.pending.Delete(f.ID)

	if err := c.Write(f); err != nil {
		return protocol.Frame{}, err
	}

	select {
	case resp := <-ch:
		if resp.Kind == protocol.KindError {
			return resp, errors.New(resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

// ReadLoop 必须在独立 goroutine 里跑。
func (c *Conn) ReadLoop() {
	defer func() {
		c.closed.Store(true)
		_ = c.ws.Close()
		if c.onClose != nil {
			c.onClose(c.workerID)
		}
	}()

	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		var f protocol.Frame
		if err := c.ws.ReadJSON(&f); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("tunnel %s read: %v", c.workerID, err)
			}
			return
		}

		switch f.Kind {
		case protocol.KindRPCResult, protocol.KindAck, protocol.KindError:
			if ch, ok := c.pending.Load(f.ID); ok {
				select {
				case ch.(chan protocol.Frame) <- f:
				default:
				}
			}
		case protocol.KindEvent:
			if c.onEvent != nil {
				c.onEvent(c.workerID, f.SessionID, f.Event)
			}
		case protocol.KindHeartbeat:
			// 心跳只用于刷新注册表，由 registry 定时清理
		default:
			log.Printf("tunnel %s unknown frame: %s", c.workerID, f.Kind)
		}
	}
}
