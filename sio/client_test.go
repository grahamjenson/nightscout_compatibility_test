package sio

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDialRootNamespace(t *testing.T) {
	fs := newFakeServer(t)
	c, data, err := Dial(fs.url("/"), nil, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if c.Namespace != "/" {
		t.Errorf("namespace %q", c.Namespace)
	}
	if jsonString(data) != `{"sid":"socket-sid"}` {
		t.Errorf("connect payload %s", jsonString(data))
	}
	conn := fs.nextConn(t)
	if auth := mustReceive(t, conn.auth, "auth"); len(auth) != 0 {
		t.Errorf("expected no auth payload got %s", auth)
	}
}

func TestDialNamespaceWithAuthAndHeaders(t *testing.T) {
	fs := newFakeServer(t)
	gotHeader := make(chan string, 1)
	inner := fs.srv.Config.Handler
	fs.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotHeader <- r.Header.Get("X-Test"):
		default:
		}
		inner.ServeHTTP(w, r)
	})

	headers := http.Header{}
	headers.Set("X-Test", "hello")
	c, _, err := Dial(fs.url("/storage/"), headers, map[string]any{"token": "abc"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if c.Namespace != "/storage" {
		t.Errorf("trailing slash should be trimmed from namespace, got %q", c.Namespace)
	}
	if h := mustReceive(t, gotHeader, "header"); h != "hello" {
		t.Errorf("header %q", h)
	}
	conn := fs.nextConn(t)
	if auth := mustReceive(t, conn.auth, "auth"); string(auth) != `{"token":"abc"}` {
		t.Errorf("auth %s", auth)
	}
}

func TestDialConnectError(t *testing.T) {
	fs := newFakeServer(t)
	fs.onConnect = func(c *fakeConn, ns string, auth []byte) string {
		return "4" + ns + `,{"message":"Invalid namespace"}`
	}
	_, data, err := Dial(fs.url("/nope"), nil, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "connect error") {
		t.Fatalf("expected connect error got %v", err)
	}
	if jsonString(data) != `{"message":"Invalid namespace"}` {
		t.Errorf("connect error payload %s", jsonString(data))
	}
}

func TestDialNoConnectReplyTimesOut(t *testing.T) {
	fs := newFakeServer(t)
	fs.onConnect = func(c *fakeConn, ns string, auth []byte) string { return "-" }
	start := time.Now()
	_, _, err := Dial(fs.url("/"), nil, nil, 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
}

func TestDialBadOpenPacket(t *testing.T) {
	fs := newFakeServer(t)
	fs.openPacket = "4hello"
	_, _, err := Dial(fs.url("/"), nil, nil, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "open packet") {
		t.Fatalf("expected open packet error got %v", err)
	}
}

func TestDialNotSocketIO(t *testing.T) {
	srv := http.NewServeMux()
	fs := newFakeServer(t)
	fs.srv.Config.Handler = srv // every path is a 404
	_, _, err := Dial(fs.url("/"), nil, nil, 2*time.Second)
	if err == nil {
		t.Fatal("expected error dialing a server without socket.io")
	}
}

func TestDialConnectionRefused(t *testing.T) {
	fs := newFakeServer(t)
	url := fs.url("/")
	fs.srv.Close()
	if _, _, err := Dial(url, nil, nil, 2*time.Second); err == nil {
		t.Fatal("expected connection refused")
	}
}

func TestDialBadURL(t *testing.T) {
	for _, u := range []string{"ftp://x/", "://nope", "localhost:1337/"} {
		if _, _, err := Dial(u, nil, nil, time.Second); err == nil {
			t.Errorf("expected error for %q", u)
		}
	}
}

func TestEmitAck(t *testing.T) {
	fs := newFakeServer(t)
	fs.onEvent = func(c *fakeConn, p packet) {
		name, args := eventName(p)
		switch name {
		case "one":
			c.ack(p.Namespace, p.AckID, map[string]any{"got": args})
		case "many":
			c.ack(p.Namespace, p.AckID, 1, "two", nil)
		case "none":
			c.ack(p.Namespace, p.AckID)
		}
	}
	c := dial(t, fs.url("/alarm"))

	res, err := c.Emit("one", []any{"a", 2.0, map[string]any{"b": true}}, true, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if jsonString(res) != `[{"got":["a",2,{"b":true}]}]` {
		t.Errorf("ack %s", jsonString(res))
	}

	res, err = c.Emit("many", nil, true, 2*time.Second)
	if err != nil || jsonString(res) != `[1,"two",null]` {
		t.Errorf("multi arg ack %s %v", jsonString(res), err)
	}

	res, err = c.Emit("none", nil, true, 2*time.Second)
	if err != nil || len(res) != 0 {
		t.Errorf("empty ack %s %v", jsonString(res), err)
	}
}

func TestEmitAckTimeout(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	_, err := c.Emit("ignored", nil, true, 100*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout got %v", err)
	}

	// a late ack for a timed out emit is dropped without blocking the client
	conn := fs.nextConn(t)
	conn.ack("/", 0, "late")
	conn.emit("/", "still-alive")
	if _, err := c.Wait("still-alive", 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestEmitWithoutAck(t *testing.T) {
	fs := newFakeServer(t)
	got := make(chan packet, 1)
	fs.onEvent = func(c *fakeConn, p packet) { got <- p }
	c := dial(t, fs.url("/"))

	res, err := c.Emit("fire", []any{"x"}, false, time.Second)
	if err != nil || res != nil {
		t.Fatalf("emit without ack %v %v", res, err)
	}
	p := mustReceive(t, got, "event")
	if p.AckID != -1 {
		t.Errorf("emit without ack should not send an ack id, got %d", p.AckID)
	}
	if name, args := eventName(p); name != "fire" || jsonString(args) != `["x"]` {
		t.Errorf("event %s %s", name, jsonString(args))
	}
}

func TestConcurrentAcksAreRoutedByID(t *testing.T) {
	fs := newFakeServer(t)
	fs.onEvent = func(c *fakeConn, p packet) {
		_, args := eventName(p)
		// reply out of order
		go func() {
			time.Sleep(time.Duration(10-int(args[0].(float64))) * time.Millisecond)
			c.ack(p.Namespace, p.AckID, args[0])
		}()
	}
	c := dial(t, fs.url("/"))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := c.Emit("echo", []any{i}, true, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			if len(res) != 1 || res[0].(float64) != float64(i) {
				t.Errorf("emit %d got ack %v", i, res)
			}
		}(i)
	}
	wg.Wait()
}

func TestWaitQueuedBeforeAndAfter(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	conn := fs.nextConn(t)

	// queued before the WAIT
	conn.emit("/", "first", 1)
	conn.emit("/", "other")
	conn.emit("/", "first", 2)
	time.Sleep(100 * time.Millisecond)

	e, err := c.Wait("first", time.Second)
	if err != nil || jsonString(e.Args) != `[1]` {
		t.Fatalf("oldest matching event first, got %v %v", e, err)
	}
	e, err = c.Wait("first", time.Second)
	if err != nil || jsonString(e.Args) != `[2]` {
		t.Fatalf("second matching event, got %v %v", e, err)
	}

	// arrives while waiting
	go func() {
		time.Sleep(100 * time.Millisecond)
		conn.emit("/", "later", "a", "b")
	}()
	e, err = c.Wait("later", 2*time.Second)
	if err != nil || jsonString(e.Args) != `["a","b"]` {
		t.Fatalf("event during wait, got %v %v", e, err)
	}

	// the non matching event is still queued
	if e, err := c.Wait("other", time.Second); err != nil || len(e.Args) != 0 {
		t.Fatalf("skipped event should stay queued, got %v %v", e, err)
	}
}

func TestWaitTimeout(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	start := time.Now()
	_, err := c.Wait("never", 100*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout got %v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Errorf("returned before the timeout")
	}
}

func TestWaitZeroTimeoutReturnsQueuedEvent(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	fs.nextConn(t).emit("/", "ready")
	time.Sleep(100 * time.Millisecond)
	if _, err := c.Wait("ready", 0); err != nil {
		t.Fatalf("a queued event should be returned even with no timeout: %v", err)
	}
	if _, err := c.Wait("ready", 0); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected timeout got %v", err)
	}
}

func TestEventsOnOtherNamespacesAreIgnored(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/storage"))
	conn := fs.nextConn(t)
	conn.emit("/", "create", "root")
	conn.emit("/alarm", "create", "alarm")
	conn.emit("/storage", "create", "storage")

	e, err := c.Wait("create", time.Second)
	if err != nil || jsonString(e.Args) != `["storage"]` {
		t.Fatalf("got %v %v", e, err)
	}
	if _, err := c.Wait("create", 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("other namespace events should be dropped, got %v", err)
	}
}

func TestBadPacketsAreIgnored(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	conn := fs.nextConn(t)
	for _, msg := range []string{"", "9", "4", "49", `42{"not":"an array"}`, `42[]`, `42[1,2]`, `42["unterminated`, `451-["bin",{"_placeholder":true,"num":0}]`, "6", "5"} {
		conn.send(msg)
	}
	conn.emit("/", "ok")
	if _, err := c.Wait("ok", 2*time.Second); err != nil {
		t.Fatalf("client should survive bad packets: %v", err)
	}
}

func TestPingIsAnswered(t *testing.T) {
	fs := newFakeServer(t)
	dial(t, fs.url("/"))
	conn := fs.nextConn(t)
	conn.send("2")
	mustReceive(t, conn.pongs, "pong")
	conn.send("2")
	mustReceive(t, conn.pongs, "second pong")
}

func TestServerNamespaceDisconnect(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	conn := fs.nextConn(t)
	conn.send("41")

	e, err := c.Wait("disconnect", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if jsonString(e.Args) != `["io server disconnect"]` {
		t.Errorf("disconnect args %s", jsonString(e.Args))
	}

	start := time.Now()
	if _, err := c.Wait("anything", 5*time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("waiting on a closed connection should fail straight away, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("wait on closed connection took %v", time.Since(start))
	}
	if _, err := c.Emit("x", nil, true, 5*time.Second); err == nil {
		t.Fatal("emit on a closed connection should fail")
	}
	if _, err := c.Emit("x", nil, false, 5*time.Second); err == nil {
		t.Fatal("emit without ack on a closed connection should fail")
	}
}

func TestServerClosesWebsocket(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	conn := fs.nextConn(t)
	conn.emit("/", "before-close")
	conn.ws.CloseNow()

	// events received before the close can still be waited for
	if _, err := c.Wait("before-close", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Wait("after-close", 5*time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("expected closed error got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v to notice the close", time.Since(start))
	}
}

func TestEngineCloseFromServer(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	fs.nextConn(t).send("1")
	if _, err := c.Wait("x", 2*time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("expected closed error got %v", err)
	}
}

func TestPendingAckFailsWhenServerDisconnects(t *testing.T) {
	fs := newFakeServer(t)
	fs.onEvent = func(c *fakeConn, p packet) { c.send("41") }
	c := dial(t, fs.url("/"))
	start := time.Now()
	_, err := c.Emit("kick-me", nil, true, 5*time.Second)
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("expected closed error got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("pending ack took %v to fail", time.Since(start))
	}
}

func TestCollect(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	conn := fs.nextConn(t)

	if events := c.Collect(50 * time.Millisecond); len(events) != 0 {
		t.Errorf("expected no events got %v", events)
	}

	conn.emit("/", "a", 1)
	conn.emit("/", "b")
	conn.emit("/", "a", 2)
	events := c.Collect(200 * time.Millisecond)
	if len(events) != 3 || events[0].Name != "a" || events[1].Name != "b" || events[2].Name != "a" {
		t.Fatalf("collect should keep order, got %v", events)
	}
	if events := c.Collect(10 * time.Millisecond); len(events) != 0 {
		t.Errorf("collect should drain the queue, got %v", events)
	}
}

func TestCollectReturnsEarlyWhenClosed(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	conn := fs.nextConn(t)
	conn.emit("/", "a")
	conn.send("41")

	start := time.Now()
	events := c.Collect(5 * time.Second)
	if time.Since(start) > 2*time.Second {
		t.Errorf("collect on a closed connection took %v", time.Since(start))
	}
	if len(events) != 2 || events[0].Name != "a" || events[1].Name != "disconnect" {
		t.Errorf("got %v", events)
	}
}

func TestLargeEvent(t *testing.T) {
	fs := newFakeServer(t)
	c := dial(t, fs.url("/"))
	big := strings.Repeat("x", 5<<20)
	fs.nextConn(t).emit("/", "dataUpdate", map[string]any{"big": big})
	e, err := c.Wait("dataUpdate", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Args[0].(map[string]any)["big"].(string); len(got) != len(big) {
		t.Errorf("got %d bytes", len(got))
	}
}

func TestCloseSendsDisconnectAndIsIdempotent(t *testing.T) {
	fs := newFakeServer(t)
	c, _, err := Dial(fs.url("/storage"), nil, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn := fs.nextConn(t)
	c.Close()
	if ns := mustReceive(t, conn.disconnects, "disconnect packet"); ns != "/storage" {
		t.Errorf("disconnect namespace %q", ns)
	}
	c.Close()
	if _, err := c.Emit("x", nil, false, time.Second); err == nil {
		t.Error("emit after close should fail")
	}
	if _, err := c.Wait("x", time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Errorf("wait after close should fail straight away, got %v", err)
	}
}

func TestCloseAfterServerClosed(t *testing.T) {
	fs := newFakeServer(t)
	c, _, err := Dial(fs.url("/"), nil, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fs.nextConn(t).ws.CloseNow()
	time.Sleep(100 * time.Millisecond)
	c.Close() // must not panic or hang
}
