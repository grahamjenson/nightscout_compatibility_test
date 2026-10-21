package sio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeServer speaks just enough Engine.IO v4 / Socket.IO v5 to test the client
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	// openPacket replaces the engine.io open packet when set
	openPacket string
	// onConnect decides the reply to a namespace connect, nil means accept
	onConnect func(c *fakeConn, ns string, auth []byte) (reply string)
	// onEvent is called for every event the client emits
	onEvent func(c *fakeConn, p packet)

	conns chan *fakeConn
}

type fakeConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	pongs       chan struct{}
	disconnects chan string
	auth        chan json.RawMessage
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, conns: make(chan *fakeConn, 16)}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	t.Cleanup(fs.srv.Close)
	return fs
}

// url returns the http URL of a namespace on the server, e.g. url("/storage")
func (fs *fakeServer) url(ns string) string {
	return fs.srv.URL + ns
}

func (fs *fakeServer) host() string {
	return strings.TrimPrefix(fs.srv.URL, "http://")
}

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/socket.io/" || r.URL.Query().Get("EIO") != "4" || r.URL.Query().Get("transport") != "websocket" {
		http.NotFound(w, r)
		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(-1)
	c := &fakeConn{
		ws:          ws,
		pongs:       make(chan struct{}, 16),
		disconnects: make(chan string, 16),
		auth:        make(chan json.RawMessage, 16),
	}
	fs.conns <- c

	open := fs.openPacket
	if open == "" {
		open = `0{"sid":"engine-sid","upgrades":[],"pingInterval":25000,"pingTimeout":20000,"maxPayload":1000000}`
	}
	c.send(open)

	for {
		_, msg, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		s := string(msg)
		switch {
		case s == "3":
			c.pongs <- struct{}{}
		case s == "1":
			ws.Close(websocket.StatusNormalClosure, "")
			return
		case strings.HasPrefix(s, "4"):
			p, err := decodePacket(s[1:])
			if err != nil {
				fs.t.Errorf("fake server got bad packet %q: %v", s, err)
				continue
			}
			switch p.Type {
			case sioConnect:
				c.auth <- p.Data
				reply := ""
				if fs.onConnect != nil {
					reply = fs.onConnect(c, p.Namespace, p.Data)
				}
				if reply == "" {
					reply = encodePacket(packet{Type: sioConnect, Namespace: p.Namespace, AckID: -1, Data: []byte(`{"sid":"socket-sid"}`)})
				}
				if reply != "-" {
					c.send("4" + reply)
				}
			case sioDisconnect:
				c.disconnects <- p.Namespace
			case sioEvent:
				if fs.onEvent != nil {
					fs.onEvent(c, p)
				}
			}
		}
	}
}

func (c *fakeConn) send(msg string) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.Write(context.Background(), websocket.MessageText, []byte(msg))
}

func (c *fakeConn) emit(ns, event string, args ...any) {
	data, _ := json.Marshal(append([]any{event}, args...))
	c.send("4" + encodePacket(packet{Type: sioEvent, Namespace: ns, AckID: -1, Data: data}))
}

func (c *fakeConn) ack(ns string, id int, args ...any) {
	data, _ := json.Marshal(args)
	c.send("4" + encodePacket(packet{Type: sioAck, Namespace: ns, AckID: id, Data: data}))
}

// eventName returns the name and arguments of an event packet
func eventName(p packet) (string, []any) {
	var arr []any
	_ = json.Unmarshal(p.Data, &arr)
	if len(arr) == 0 {
		return "", nil
	}
	name, _ := arr[0].(string)
	return name, arr[1:]
}

func (fs *fakeServer) nextConn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-fs.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection to fake server")
		return nil
	}
}

func dial(t *testing.T, url string) *Client {
	t.Helper()
	c, _, err := Dial(url, nil, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(c.Close)
	return c
}

func mustReceive[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
