package encoding

import (
	"context"
	"io"
	"sync"

	"github.com/readeem/mixray/internal/xray-core/common/buf"
	"github.com/readeem/mixray/internal/xray-core/common/errors"
	"github.com/readeem/mixray/internal/xray-core/common/net"
	"github.com/readeem/mixray/internal/xray-core/common/net/cnc"
	"github.com/readeem/mixray/internal/xray-core/common/signal/done"
)

type HunkConn interface {
	Context() context.Context
	Send(*Hunk) error
	Recv() (*Hunk, error)
	SendMsg(m interface{}) error
	RecvMsg(m interface{}) error
}

type StreamCloser interface {
	CloseSend() error
}

type HunkReaderWriter struct {
	hc     HunkConn
	cancel context.CancelFunc
	done   *done.Instance
	// gRPC does not allow Send and CloseSend to overlap.
	sendMu      sync.Mutex
	writeClosed bool

	buf   []byte
	index int
}

func NewHunkReadWriter(hc HunkConn, cancel context.CancelFunc) *HunkReaderWriter {
	return &HunkReaderWriter{hc: hc, cancel: cancel, done: done.New()}
}

func NewHunkConn(hc HunkConn, cancel context.CancelFunc, trustedXForwardedFor []string) net.Conn {
	rAddr := remoteAddrFromContext(hc.Context(), trustedXForwardedFor)
	lAddr := localAddrFromContext(hc.Context())
	wrc := NewHunkReadWriter(hc, cancel)
	return cnc.NewConnection(
		cnc.ConnectionInput(wrc),
		cnc.ConnectionOutput(wrc),
		cnc.ConnectionOnClose(wrc),
		cnc.ConnectionRemoteAddr(rAddr),
		cnc.ConnectionLocalAddr(lAddr),
	)
}

func (h *HunkReaderWriter) forceFetch() error {
	hunk, err := h.hc.Recv()
	if err != nil {
		if err == io.EOF {
			return err
		}

		return errors.New("failed to fetch hunk from gRPC tunnel").Base(err)
	}

	h.buf = hunk.Data
	h.index = 0

	return nil
}

func (h *HunkReaderWriter) Read(buf []byte) (int, error) {
	if h.done.Done() {
		return 0, io.EOF
	}

	if h.index >= len(h.buf) {
		if err := h.forceFetch(); err != nil {
			return 0, err
		}
	}
	n := copy(buf, h.buf[h.index:])
	h.index += n

	return n, nil
}

func (h *HunkReaderWriter) ReadMultiBuffer() (buf.MultiBuffer, error) {
	if h.done.Done() {
		return nil, io.EOF
	}
	if h.index >= len(h.buf) {
		if err := h.forceFetch(); err != nil {
			return nil, err
		}
	}

	if cap(h.buf) >= buf.Size {
		b := h.buf
		h.index = len(h.buf)
		return buf.MultiBuffer{buf.NewExisted(b)}, nil
	}

	b := buf.New()
	_, err := b.ReadFrom(h)
	if err != nil {
		return nil, err
	}
	return buf.MultiBuffer{b}, nil
}

func (h *HunkReaderWriter) Write(buf []byte) (int, error) {
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	if h.writeClosed || h.done.Done() {
		return 0, io.ErrClosedPipe
	}

	err := h.hc.Send(&Hunk{Data: buf[:]})
	if err != nil {
		return 0, errors.New("failed to send data over gRPC tunnel").Base(err)
	}
	return len(buf), nil
}

func (h *HunkReaderWriter) CloseWrite() error {
	sc, ok := h.hc.(StreamCloser)
	if !ok {
		return nil
	}
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	if h.writeClosed || h.done.Done() {
		return nil
	}
	h.writeClosed = true
	return sc.CloseSend()
}

func (h *HunkReaderWriter) Close() error {
	if h.cancel != nil {
		h.cancel()
	}
	return h.done.Close()
}
