//go:build !js

package device

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

	"github.com/tyler-builds/solo/internal/remote/wire"
)

// RefusedError is a connection the relay did not let in.
type RefusedError struct{ Status int }

func (e *RefusedError) Error() string {
	return fmt.Sprintf("the relay refused the connection (%d)", e.Status)
}

// WebSocket connects to the relay at an address such as "https://solo-x.name.workers.dev" as a browser
// would, for a device that isn't a page: another Solo linked to this one, or a test. client is nil for
// http.DefaultClient.
func WebSocket(address string, client *http.Client) Dialer {
	if client == nil {
		client = http.DefaultClient
	}
	host := strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://")
	return func(ctx context.Context, path string, token []byte) (wire.Conn, error) {
		target := "ws" + strings.TrimPrefix(address, "http") + path
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ws, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{
			HTTPClient:   client,
			Subprotocols: []string{Subprotocol, base64.RawURLEncoding.EncodeToString(token)},
			// A page served by the relay says so, with the origin the relay knows itself by.
			HTTPHeader: http.Header{"Origin": {Origin(host)}},
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
