//go:build !js

package relaytest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

// CloseError is a connection the relay closed, with its code.
type CloseError struct{ Code int }

func (e *CloseError) Error() string {
	return fmt.Sprintf("the relay closed the connection (%d)", e.Code)
}

// RefusedError is a connection the relay did not let in.
type RefusedError struct{ Status int }

func (e *RefusedError) Error() string {
	return fmt.Sprintf("the relay refused the connection (%d)", e.Status)
}

// Dialer connects to a relay at an address such as "http://127.0.0.1:8787" as a browser would, for
// tests and tools that play a device.
func Dialer(address string) device.Dialer {
	return func(ctx context.Context, path string, token []byte) (wire.Conn, error) {
		target := "ws" + strings.TrimPrefix(address, "http") + path
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ws, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{
			Subprotocols: []string{device.Subprotocol, base64.RawURLEncoding.EncodeToString(token)},
			HTTPHeader:   http.Header{"Origin": {address}},
		})
		if err != nil {
			if resp != nil {
				return nil, &RefusedError{Status: resp.StatusCode}
			}
			return nil, err
		}
		ws.SetReadLimit(1 << 17)
		return &wsConn{ws: ws}, nil
	}
}

type wsConn struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (c *wsConn) Read() ([]byte, error) {
	typ, b, err := c.ws.Read(context.Background())
	if err != nil {
		if code := websocket.CloseStatus(err); code > 0 {
			return nil, &CloseError{Code: int(code)}
		}
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, errors.New("a text frame")
	}
	return b, nil
}

func (c *wsConn) Write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageBinary, b)
}

func (c *wsConn) Close() error { return c.ws.CloseNow() }
