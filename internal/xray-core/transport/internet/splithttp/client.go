package splithttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"

	"github.com/apernet/quic-go/http3"
	"github.com/readeem/mixray/internal/xray-core/common"
	"github.com/readeem/mixray/internal/xray-core/common/buf"
	"github.com/readeem/mixray/internal/xray-core/common/errors"
	"github.com/readeem/mixray/internal/xray-core/common/net"
	"github.com/readeem/mixray/internal/xray-core/common/signal/done"
)

// interface to abstract between use of browser dialer, vs net/http
type DialerClient interface {
	IsClosed() bool

	// ctx, url, sessionId, body, uploadOnly
	OpenStream(context.Context, string, string, io.Reader, bool) (io.ReadCloser, net.Addr, net.Addr, error)

	// ctx, url, sessionId, seqStr, body, contentLength
	PostPacket(context.Context, string, string, string, buf.MultiBuffer) error
}

// implements splithttp.DialerClient in terms of direct network connections
type DefaultDialerClient struct {
	transportConfig *Config
	client          *http.Client
	closed          atomic.Bool
	retired         atomic.Bool
	httpVersion     string
	uploadMu        sync.Mutex
	idleUploads     []*H1Conn
	dialUploadConn  func(ctxInner context.Context) (net.Conn, error)
}

func (c *DefaultDialerClient) IsClosed() bool {
	return c.closed.Load()
}

func (c *DefaultDialerClient) OpenStream(ctx context.Context, url string, sessionId string, body io.Reader, uploadOnly bool) (io.ReadCloser, net.Addr, net.Addr, error) {
	type dialResult struct {
		remoteAddr net.Addr
		localAddr  net.Addr
		err        error
	}
	result := make(chan dialResult, 1)
	var ready sync.Once
	if c.retired.Load() {
		return nil, nil, nil, errors.New("XHTTP client is closed")
	}
	report := func(r dialResult) { ready.Do(func() { result <- r }) }
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			report(dialResult{remoteAddr: info.Conn.RemoteAddr(), localAddr: info.Conn.LocalAddr()})
		},
	})

	method := "GET" // stream-down
	if body != nil {
		method = c.transportConfig.GetNormalizedUplinkHTTPMethod() // stream-up/one
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		errors.LogInfoInner(ctx, err, "failed to create HTTP request for "+url)
		return nil, nil, nil, err
	}
	c.transportConfig.FillStreamRequest(req, sessionId, "")

	wrc := &WaitReadCloser{wait: done.New()}
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			if !uploadOnly && ctx.Err() == nil {
				c.closed.Store(true)
				errors.LogInfoInner(ctx, err, "failed to "+method+" "+url)
			}
			report(dialResult{err: err})
			common.Close(body)
			wrc.Close()
			return
		}
		report(dialResult{})
		if resp.StatusCode != 200 && !uploadOnly {
			errors.LogInfo(ctx, "unexpected status ", resp.StatusCode)
		}
		if resp.StatusCode != 200 || uploadOnly {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			common.Close(body)
			wrc.Close()
			return
		}
		wrc.Set(resp.Body)
	}()

	select {
	case r := <-result:
		if r.err != nil {
			return nil, nil, nil, r.err
		}
		return wrc, r.remoteAddr, r.localAddr, nil
	case <-ctx.Done():
		wrc.Close()
		return nil, nil, nil, ctx.Err()
	}
}

func (c *DefaultDialerClient) PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload buf.MultiBuffer) error {
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	c.transportConfig.FillPacketRequest(req, sessionId, seqStr, payload)

	if c.retired.Load() {
		return errors.New("XHTTP client is closed")
	}
	if c.httpVersion == "1.1" {
		return c.postPacketH1(ctx, req)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.closed.Store(true)
		}
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return errors.New("bad status code:", resp.Status)
	}
	return nil
}

func (c *DefaultDialerClient) postPacketH1(ctx context.Context, req *http.Request) error {
	request := new(bytes.Buffer)
	request.Grow(512 + int(req.ContentLength))
	if err := req.Write(request); err != nil {
		return err
	}
	for ctx.Err() == nil {
		conn := c.takeUpload()
		reused := conn != nil
		if !reused {
			if c.retired.Load() {
				return errors.New("XHTTP client is closed")
			}
			raw, err := c.dialUploadConn(ctx)
			if err != nil {
				return err
			}
			conn = NewH1Conn(raw)
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		n, err := conn.Write(request.Bytes())
		if err != nil {
			stop()
			conn.Close()
			if reused && n == 0 {
				continue
			}
			return err
		}
		if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		reusable, err := readUploadResponse(conn, req)
		if stop() && ctx.Err() == nil && reusable && err == nil {
			c.putUpload(conn)
		} else {
			conn.Close()
		}
		return err
	}
	return ctx.Err()
}

func readUploadResponse(conn *H1Conn, req *http.Request) (bool, error) {
	resp, err := http.ReadResponse(conn.RespBufReader, req)
	if err != nil {
		return false, fmt.Errorf("read upload response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected upload status: %s", resp.Status)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return !resp.Close, err
}

func (c *DefaultDialerClient) takeUpload() *H1Conn {
	c.uploadMu.Lock()
	defer c.uploadMu.Unlock()
	if len(c.idleUploads) == 0 || c.retired.Load() {
		return nil
	}
	last := len(c.idleUploads) - 1
	conn := c.idleUploads[last]
	c.idleUploads[last] = nil
	c.idleUploads = c.idleUploads[:last]
	return conn
}

func (c *DefaultDialerClient) putUpload(conn *H1Conn) {
	const maxIdleUploads = 8
	c.uploadMu.Lock()
	if len(c.idleUploads) < maxIdleUploads && !c.retired.Load() {
		c.idleUploads = append(c.idleUploads, conn)
		c.uploadMu.Unlock()
		return
	}
	c.uploadMu.Unlock()
	conn.Close()
}

func (c *DefaultDialerClient) Close() error {
	if c.retired.Swap(true) {
		return nil
	}
	c.closed.Store(true)
	c.uploadMu.Lock()
	idle := c.idleUploads
	c.idleUploads = nil
	c.uploadMu.Unlock()
	for _, conn := range idle {
		conn.Close()
	}
	c.client.CloseIdleConnections()
	if h3Transport, ok := c.client.Transport.(*http3.Transport); ok {
		return h3Transport.Close()
	}
	return nil
}

type WaitReadCloser struct {
	wait   *done.Instance
	reader atomic.Pointer[io.ReadCloser]
}

func (w *WaitReadCloser) Set(rc io.ReadCloser) {
	w.reader.Store(&rc)
	if w.wait.Done() {
		if p := w.reader.Swap(nil); p != nil {
			(*p).Close()
		}
	}
	w.wait.Close()
}

func (w *WaitReadCloser) Read(b []byte) (int, error) {
	rc := w.reader.Load()
	if rc == nil {
		<-w.wait.Wait()
		if rc = w.reader.Load(); rc == nil {
			return 0, io.ErrClosedPipe
		}
	}
	return (*rc).Read(b)
}

func (w *WaitReadCloser) Close() error {
	w.wait.Close()
	if p := w.reader.Swap(nil); p != nil {
		return (*p).Close()
	}
	return nil
}
