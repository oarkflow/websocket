// +build !js

package websocket_test

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/duration"
	"golang.org/x/xerrors"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/internal/test/assert"
	"nhooyr.io/websocket/internal/test/wstest"
	"nhooyr.io/websocket/internal/test/xrand"
	"nhooyr.io/websocket/internal/xsync"
	"nhooyr.io/websocket/wsjson"
	"nhooyr.io/websocket/wspb"
)

func TestConn(t *testing.T) {
	t.Parallel()

	t.Run("fuzzData", func(t *testing.T) {
		t.Parallel()

		copts := func() *websocket.CompressionOptions {
			return &websocket.CompressionOptions{
				Mode:      websocket.CompressionMode(xrand.Int(int(websocket.CompressionDisabled) + 1)),
				Threshold: xrand.Int(9999),
			}
		}

		for i := 0; i < 5; i++ {
			t.Run("", func(t *testing.T) {
				tt, c1, c2 := newConnTest(t, &websocket.DialOptions{
					CompressionOptions: copts(),
				}, &websocket.AcceptOptions{
					CompressionOptions: copts(),
				})
				defer tt.done()

				tt.goEchoLoop(c2)

				c1.SetReadLimit(131072)

				for i := 0; i < 5; i++ {
					err := wstest.Echo(tt.ctx, c1, 131072)
					assert.Success(t, err)
				}

				err := c1.Close(websocket.StatusNormalClosure, "")
				assert.Success(t, err)
			})
		}
	})

	t.Run("badClose", func(t *testing.T) {
		tt, c1, _ := newConnTest(t, nil, nil)
		defer tt.done()

		err := c1.Close(-1, "")
		assert.Contains(t, err, "failed to marshal close frame: status code StatusCode(-1) cannot be set")
	})

	t.Run("ping", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		c1.CloseRead(tt.ctx)
		c2.CloseRead(tt.ctx)

		for i := 0; i < 10; i++ {
			err := c1.Ping(tt.ctx)
			assert.Success(t, err)
		}

		err := c1.Close(websocket.StatusNormalClosure, "")
		assert.Success(t, err)
	})

	t.Run("badPing", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		c2.CloseRead(tt.ctx)

		ctx, cancel := context.WithTimeout(tt.ctx, time.Millisecond*100)
		defer cancel()

		err := c1.Ping(ctx)
		assert.Contains(t, err, "failed to wait for pong")
	})

	t.Run("concurrentWrite", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		tt.goDiscardLoop(c2)

		msg := xrand.Bytes(xrand.Int(9999))
		const count = 100
		errs := make(chan error, count)

		for i := 0; i < count; i++ {
			go func() {
				errs <- c1.Write(tt.ctx, websocket.MessageBinary, msg)
			}()
		}

		for i := 0; i < count; i++ {
			err := <-errs
			assert.Success(t, err)
		}

		err := c1.Close(websocket.StatusNormalClosure, "")
		assert.Success(t, err)
	})

	t.Run("concurrentWriteError", func(t *testing.T) {
		tt, c1, _ := newConnTest(t, nil, nil)
		defer tt.done()

		_, err := c1.Writer(tt.ctx, websocket.MessageText)
		assert.Success(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
		defer cancel()

		err = c1.Write(ctx, websocket.MessageText, []byte("x"))
		assert.Equal(t, "write error", context.DeadlineExceeded, err)
	})

	t.Run("netConn", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		n1 := websocket.NetConn(tt.ctx, c1, websocket.MessageBinary)
		n2 := websocket.NetConn(tt.ctx, c2, websocket.MessageBinary)

		// Does not give any confidence but at least ensures no crashes.
		d, _ := tt.ctx.Deadline()
		n1.SetDeadline(d)
		n1.SetDeadline(time.Time{})

		assert.Equal(t, "remote addr", n1.RemoteAddr(), n1.LocalAddr())
		assert.Equal(t, "remote addr string", "websocket/unknown-addr", n1.RemoteAddr().String())
		assert.Equal(t, "remote addr network", "websocket", n1.RemoteAddr().Network())

		errs := xsync.Go(func() error {
			_, err := n2.Write([]byte("hello"))
			if err != nil {
				return err
			}
			return n2.Close()
		})

		b, err := ioutil.ReadAll(n1)
		assert.Success(t, err)

		_, err = n1.Read(nil)
		assert.Equal(t, "read error", err, io.EOF)

		err = <-errs
		assert.Success(t, err)

		assert.Equal(t, "read msg", []byte("hello"), b)
	})

	t.Run("netConn/BadMsg", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		n1 := websocket.NetConn(tt.ctx, c1, websocket.MessageBinary)
		n2 := websocket.NetConn(tt.ctx, c2, websocket.MessageText)

		errs := xsync.Go(func() error {
			_, err := n2.Write([]byte("hello"))
			if err != nil {
				return err
			}
			return nil
		})

		_, err := ioutil.ReadAll(n1)
		assert.Contains(t, err, `unexpected frame type read (expected MessageBinary): MessageText`)

		err = <-errs
		assert.Success(t, err)
	})

	t.Run("wsjson", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		tt.goEchoLoop(c2)

		c1.SetReadLimit(1 << 30)

		exp := xrand.String(xrand.Int(131072))

		werr := xsync.Go(func() error {
			return wsjson.Write(tt.ctx, c1, exp)
		})

		var act interface{}
		err := wsjson.Read(tt.ctx, c1, &act)
		assert.Success(t, err)
		assert.Equal(t, "read msg", exp, act)

		err = <-werr
		assert.Success(t, err)

		err = c1.Close(websocket.StatusNormalClosure, "")
		assert.Success(t, err)
	})

	t.Run("wspb", func(t *testing.T) {
		tt, c1, c2 := newConnTest(t, nil, nil)
		defer tt.done()

		tt.goEchoLoop(c2)

		exp := ptypes.DurationProto(100)
		err := wspb.Write(tt.ctx, c1, exp)
		assert.Success(t, err)

		act := &duration.Duration{}
		err = wspb.Read(tt.ctx, c1, act)
		assert.Success(t, err)
		assert.Equal(t, "read msg", exp, act)

		err = c1.Close(websocket.StatusNormalClosure, "")
		assert.Success(t, err)
	})
}

func TestWasm(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:       []string{"echo"},
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Errorf("echo server failed: %v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "")

		err = wstest.EchoLoop(r.Context(), c)

		err = assertCloseStatus(websocket.StatusNormalClosure, err)
		if err != nil {
			t.Errorf("echo server failed: %v", err)
			return
		}
	}))
	defer wg.Wait()
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test", "-exec=wasmbrowsertest", "./...")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", fmt.Sprintf("WS_ECHO_SERVER_URL=%v", wstest.URL(s)))

	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wasm test binary failed: %v:\n%s", err, b)
	}
}

func assertCloseStatus(exp websocket.StatusCode, err error) error {
	if websocket.CloseStatus(err) == -1 {
		return xerrors.Errorf("expected websocket.CloseError: %T %v", err, err)
	}
	if websocket.CloseStatus(err) != exp {
		return xerrors.Errorf("expected close status %v but got ", exp, err)
	}
	return nil
}

type connTest struct {
	t   *testing.T
	ctx context.Context

	doneFuncs []func()
}

func newConnTest(t *testing.T, dialOpts *websocket.DialOptions, acceptOpts *websocket.AcceptOptions) (tt *connTest, c1, c2 *websocket.Conn) {
	t.Parallel()
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	tt = &connTest{t: t, ctx: ctx}
	tt.appendDone(cancel)

	c1, c2, err := wstest.Pipe(dialOpts, acceptOpts)
	assert.Success(tt.t, err)
	tt.appendDone(func() {
		c2.Close(websocket.StatusInternalError, "")
		c1.Close(websocket.StatusInternalError, "")
	})

	return tt, c1, c2
}

func (tt *connTest) appendDone(f func()) {
	tt.doneFuncs = append(tt.doneFuncs, f)
}

func (tt *connTest) done() {
	for i := len(tt.doneFuncs) - 1; i >= 0; i-- {
		tt.doneFuncs[i]()
	}
}

func (tt *connTest) goEchoLoop(c *websocket.Conn) {
	ctx, cancel := context.WithCancel(tt.ctx)

	echoLoopErr := xsync.Go(func() error {
		err := wstest.EchoLoop(ctx, c)
		return assertCloseStatus(websocket.StatusNormalClosure, err)
	})
	tt.appendDone(func() {
		cancel()
		err := <-echoLoopErr
		if err != nil {
			tt.t.Errorf("echo loop error: %v", err)
		}
	})
}

func (tt *connTest) goDiscardLoop(c *websocket.Conn) {
	ctx, cancel := context.WithCancel(tt.ctx)

	discardLoopErr := xsync.Go(func() error {
		defer c.Close(websocket.StatusInternalError, "")

		for {
			_, _, err := c.Read(ctx)
			if err != nil {
				return assertCloseStatus(websocket.StatusNormalClosure, err)
			}
		}
	})
	tt.appendDone(func() {
		cancel()
		err := <-discardLoopErr
		if err != nil {
			tt.t.Errorf("discard loop error: %v", err)
		}
	})
}
