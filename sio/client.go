package sio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Client is a minimal Socket.IO client (protocol v5, used by socket.io 3.x/4.x)
// over the Engine.IO v4 websocket transport. It connects to a single namespace,
// queues every event the server sends, and supports emitting with acks.
//
// Binary events/acks are not supported.

// Engine.IO packet types
const (
	eioOpen    = '0'
	eioClose   = '1'
	eioPing    = '2'
	eioPong    = '3'
	eioMessage = '4'
	eioNoop    = '6'
)

// Socket.IO packet types
const (
	sioConnect      = 0
	sioDisconnect   = 1
	sioEvent        = 2
	sioAck          = 3
	sioConnectError = 4
	sioBinaryEvent  = 5
	sioBinaryAck    = 6
)

var ErrTimeout = errors.New("timed out")

type Event struct {
	Name string
	Args []any
}

type packet struct {
	Type      int
	Namespace string
	AckID     int // -1 when there is no ack id
	Data      json.RawMessage
}

type Client struct {
	URL       string // the URL that was used to connect, e.g. http://localhost:1337/storage
	Namespace string

	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	writeMu sync.Mutex

	mu        sync.Mutex
	events    []Event
	newEvent  chan struct{} // closed and replaced every time an event is queued
	acks      map[int]chan []any
	nextAckID int
	closed    bool
	closeErr  error

	connected chan connectResult
}

type connectResult struct {
	data any
	err  error
}

// SocketURL converts a URL like http://host/storage?x=y into the Engine.IO
// websocket URL ws://host/socket.io/?EIO=4&transport=websocket&x=y and the namespace /storage
func SocketURL(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}

	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	namespace := strings.TrimSuffix(u.Path, "/")
	if namespace == "" {
		namespace = "/"
	}

	q := u.Query()
	q.Set("EIO", "4")
	q.Set("transport", "websocket")
	u.Path = "/socket.io/"
	u.RawQuery = q.Encode()

	return u.String(), namespace, nil
}

// Dial opens the websocket, joins the namespace and returns the client and the
// server's connect payload (e.g. {"sid": "..."}). auth is sent as the namespace
// connect payload if it is not nil.
func Dial(rawURL string, headers http.Header, auth any, timeout time.Duration) (*Client, any, error) {
	wsURL, namespace, err := SocketURL(rawURL)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		URL:       rawURL,
		Namespace: namespace,
		ctx:       ctx,
		cancel:    cancel,
		newEvent:  make(chan struct{}),
		acks:      map[int]chan []any{},
		connected: make(chan connectResult, 1),
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	defer dialCancel()

	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("websocket dial %s: %w", wsURL, err)
	}
	// dataUpdate messages can be many MB
	conn.SetReadLimit(256 << 20)
	c.conn = conn

	// The first message is the Engine.IO open packet: 0{"sid":"...","pingInterval":...}
	_, msg, err := conn.Read(dialCtx)
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("reading engine.io open packet: %w", err)
	}
	if len(msg) == 0 || msg[0] != eioOpen {
		c.Close()
		return nil, nil, fmt.Errorf("expected engine.io open packet, got %q", msg)
	}

	go c.readLoop()

	var authData json.RawMessage
	if auth != nil {
		authData, err = json.Marshal(auth)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
	}
	if err := c.send(packet{Type: sioConnect, Namespace: namespace, AckID: -1, Data: authData}); err != nil {
		c.Close()
		return nil, nil, err
	}

	select {
	case res := <-c.connected:
		if res.err != nil {
			c.Close()
			return nil, res.data, res.err
		}
		return c, res.data, nil
	case <-time.After(timeout):
		c.Close()
		return nil, nil, fmt.Errorf("namespace %s connect: %w", namespace, ErrTimeout)
	}
}

// Emit sends an event. If ack is true it waits for the server to call the ack
// callback and returns the arguments it was called with.
func (c *Client) Emit(event string, args []any, ack bool, timeout time.Duration) ([]any, error) {
	data, err := json.Marshal(append([]any{event}, args...))
	if err != nil {
		return nil, err
	}

	p := packet{Type: sioEvent, Namespace: c.Namespace, AckID: -1, Data: data}

	var ackCh chan []any
	if ack {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, c.closedError()
		}
		p.AckID = c.nextAckID
		c.nextAckID++
		ackCh = make(chan []any, 1)
		c.acks[p.AckID] = ackCh
		c.mu.Unlock()

		defer func() {
			c.mu.Lock()
			delete(c.acks, p.AckID)
			c.mu.Unlock()
		}()
	}

	if err := c.send(p); err != nil {
		return nil, err
	}

	if !ack {
		return nil, nil
	}

	select {
	case res, ok := <-ackCh:
		if !ok {
			return nil, c.closedError()
		}
		return res, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("waiting for ack of %q: %w", event, ErrTimeout)
	}
}

// Wait removes and returns the oldest queued event called name, waiting up to
// timeout for one to arrive.
func (c *Client) Wait(name string, timeout time.Duration) (Event, error) {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for i, e := range c.events {
			if e.Name == name {
				c.events = append(c.events[:i], c.events[i+1:]...)
				c.mu.Unlock()
				return e, nil
			}
		}
		if c.closed {
			c.mu.Unlock()
			return Event{}, fmt.Errorf("waiting for %q: %w", name, c.closedError())
		}
		signal := c.newEvent
		c.mu.Unlock()

		select {
		case <-signal:
		case <-deadline:
			return Event{}, fmt.Errorf("waiting for %q: %w", name, ErrTimeout)
		}
	}
}

// Collect waits for d (or until the connection closes) then removes and
// returns every queued event.
func (c *Client) Collect(d time.Duration) []Event {
	select {
	case <-time.After(d):
	case <-c.ctx.Done():
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	events := c.events
	c.events = nil
	return events
}

// Close leaves the namespace and closes the websocket
func (c *Client) Close() {
	c.mu.Lock()
	alreadyClosed := c.closed
	c.mu.Unlock()

	if !alreadyClosed {
		_ = c.send(packet{Type: sioDisconnect, Namespace: c.Namespace, AckID: -1})
		_ = c.writeRaw(string(eioClose))
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	c.shutdown(errors.New("closed by client"))
}

func (c *Client) readLoop() {
	for {
		_, msg, err := c.conn.Read(c.ctx)
		if err != nil {
			c.shutdown(fmt.Errorf("connection closed: %w", err))
			return
		}
		if len(msg) == 0 {
			continue
		}

		switch msg[0] {
		case eioPing:
			c.writeRaw(string(eioPong))
		case eioClose:
			c.shutdown(errors.New("connection closed by server"))
			return
		case eioNoop, eioPong:
		case eioMessage:
			p, err := decodePacket(string(msg[1:]))
			if err != nil {
				fmt.Fprintf(os.Stderr, "socket.io: ignoring bad packet %q: %v\n", msg, err)
				continue
			}
			if p.Namespace == c.Namespace {
				c.handlePacket(p)
			}
		default:
			fmt.Fprintf(os.Stderr, "socket.io: ignoring engine.io packet %q\n", msg)
		}
	}
}

func (c *Client) handlePacket(p packet) {
	switch p.Type {
	case sioConnect:
		c.signalConnected(connectResult{data: decodeData(p.Data)})

	case sioConnectError:
		data := decodeData(p.Data)
		c.signalConnected(connectResult{data: data, err: fmt.Errorf("namespace %s connect error: %v", c.Namespace, data)})

	case sioEvent:
		var arr []any
		if err := json.Unmarshal(p.Data, &arr); err != nil || len(arr) == 0 {
			fmt.Fprintf(os.Stderr, "socket.io: ignoring bad event %s\n", p.Data)
			return
		}
		name, ok := arr[0].(string)
		if !ok {
			fmt.Fprintf(os.Stderr, "socket.io: ignoring event without a name %s\n", p.Data)
			return
		}
		c.queue(Event{Name: name, Args: arr[1:]})

	case sioAck:
		var args []any
		if err := json.Unmarshal(p.Data, &args); err != nil {
			fmt.Fprintf(os.Stderr, "socket.io: ignoring bad ack %s\n", p.Data)
			return
		}
		c.mu.Lock()
		if ch, ok := c.acks[p.AckID]; ok {
			select {
			case ch <- args:
			default:
			}
		}
		c.mu.Unlock()

	case sioDisconnect:
		// The server kicked us out of the namespace, e.g. after a failed authorize.
		// Queue it like the socket.io client's "disconnect" event so scripts can WAIT for it.
		c.queue(Event{Name: "disconnect", Args: []any{"io server disconnect"}})
		c.shutdown(errors.New("disconnected by server"))

	case sioBinaryEvent, sioBinaryAck:
		fmt.Fprintf(os.Stderr, "socket.io: binary packets are not supported, ignoring\n")
	}
}

func (c *Client) queue(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	close(c.newEvent)
	c.newEvent = make(chan struct{})
}

func (c *Client) shutdown(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err
	for id, ch := range c.acks {
		close(ch)
		delete(c.acks, id)
	}
	close(c.newEvent)
	c.newEvent = make(chan struct{})
	c.signalConnected(connectResult{err: err})
	c.cancel()
}

// signalConnected reports the namespace connect result to Dial, if it is still waiting
func (c *Client) signalConnected(res connectResult) {
	select {
	case c.connected <- res:
	default:
	}
}

func (c *Client) closedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

func (c *Client) send(p packet) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.closedError()
	}
	return c.writeRaw(string(eioMessage) + encodePacket(p))
}

func (c *Client) writeRaw(msg string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.conn.Write(ctx, websocket.MessageText, []byte(msg))
}

// encodePacket encodes <type>[<namespace>,][<ack id>][<json data>]
func encodePacket(p packet) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(p.Type))
	if p.Namespace != "" && p.Namespace != "/" {
		b.WriteString(p.Namespace)
		b.WriteString(",")
	}
	if p.AckID >= 0 {
		b.WriteString(strconv.Itoa(p.AckID))
	}
	b.Write(p.Data)
	return b.String()
}

func decodePacket(s string) (packet, error) {
	p := packet{Namespace: "/", AckID: -1}
	if len(s) == 0 || s[0] < '0' || s[0] > '6' {
		return p, fmt.Errorf("bad packet type")
	}
	p.Type = int(s[0] - '0')
	s = s[1:]

	if p.Type == sioBinaryEvent || p.Type == sioBinaryAck {
		i := strings.IndexByte(s, '-')
		if i < 0 {
			return p, fmt.Errorf("binary packet missing attachment count")
		}
		s = s[i+1:]
	}

	if strings.HasPrefix(s, "/") {
		i := strings.IndexByte(s, ',')
		if i < 0 {
			p.Namespace, s = s, ""
		} else {
			p.Namespace, s = s[:i], s[i+1:]
		}
	}

	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 {
		id, err := strconv.Atoi(s[:i])
		if err != nil {
			return p, err
		}
		p.AckID = id
	}

	if rest := s[i:]; rest != "" {
		if !json.Valid([]byte(rest)) {
			return p, fmt.Errorf("invalid json payload")
		}
		p.Data = json.RawMessage(rest)
	}
	return p, nil
}

func decodeData(data json.RawMessage) any {
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}
