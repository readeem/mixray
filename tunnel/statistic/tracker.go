package statistic

import (
	"io"
	"net"
	"sync"
	syncatomic "sync/atomic"
	"time"

	"github.com/readeem/mixray/common/atomic"
	"github.com/readeem/mixray/common/buf"
	N "github.com/readeem/mixray/common/net"
	"github.com/readeem/mixray/common/utils"
	C "github.com/readeem/mixray/constant"

	"github.com/gofrs/uuid/v5"
)

type Tracker interface {
	ID() string
	Close() error
	Info() *TrackerInfo
	C.Connection
}

// SetConnectionObserver installs the session capture callback. Capture it
// before dialing so late completions stay with the session that accepted them.
func SetConnectionObserver(observer func() func(*TrackerInfo) func()) {
	connectionObserver.Store(&observer)
}

var connectionObserver syncatomic.Pointer[func() func(*TrackerInfo) func()]

func CaptureConnection() func(*TrackerInfo) func() {
	if observer := connectionObserver.Load(); observer != nil {
		return (*observer)()
	}
	return nil
}

type TrackerInfo struct {
	UUID                 uuid.UUID     `json:"id"`
	Metadata             *C.Metadata   `json:"metadata"`
	UploadTotal          atomic.Int64  `json:"upload"`
	DownloadTotal        atomic.Int64  `json:"download"`
	Start                time.Time     `json:"start"`
	Chain                C.Chain       `json:"chains"`
	ProviderChain        C.Chain       `json:"providerChains"`
	Session              atomic.Uint64 `json:"-"`
	MultipleDestinations atomic.Bool   `json:"-"`
	Rule                 string        `json:"rule"`
	RulePayload          string        `json:"rulePayload"`
}

// Internal dialers can close a connection while a read or write is
// returning. Drain those calls before recording the final byte counts.
type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

type observedIO struct {
	mu     sync.RWMutex
	finish func()
}

func (o *observedIO) closed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.finish()
}

type tcpTracker struct {
	observed *observedIO
	C.Conn   `json:"-"`
	*TrackerInfo
	manager *Manager

	pushToManager bool `json:"-"`
}

func (tt *tcpTracker) ID() string {
	return tt.UUID.String()
}

func (tt *tcpTracker) Info() *TrackerInfo {
	return tt.TrackerInfo
}

func (tt *tcpTracker) Read(b []byte) (int, error) {
	if tt.observed != nil {
		tt.observed.mu.RLock()
		defer tt.observed.mu.RUnlock()
	}
	n, err := tt.Conn.Read(b)
	download := int64(n)
	if tt.pushToManager {
		tt.manager.PushDownloaded(download)
	}
	tt.DownloadTotal.Add(download)
	return n, err
}

func (tt *tcpTracker) ReadBuffer(buffer *buf.Buffer) (err error) {
	if tt.observed != nil {
		tt.observed.mu.RLock()
		defer tt.observed.mu.RUnlock()
	}
	err = tt.Conn.ReadBuffer(buffer)
	download := int64(buffer.Len())
	if tt.pushToManager {
		tt.manager.PushDownloaded(download)
	}
	tt.DownloadTotal.Add(download)
	return
}

func (tt *tcpTracker) UnwrapReader() (io.Reader, []N.CountFunc) {
	if tt.observed != nil {
		return readerOnly{tt}, nil
	}
	return tt.Conn, []N.CountFunc{func(download int64) {
		if tt.pushToManager {
			tt.manager.PushDownloaded(download)
		}
		tt.DownloadTotal.Add(download)
	}}
}

func (tt *tcpTracker) Write(b []byte) (int, error) {
	if tt.observed != nil {
		tt.observed.mu.RLock()
		defer tt.observed.mu.RUnlock()
	}
	n, err := tt.Conn.Write(b)
	upload := int64(n)
	if tt.pushToManager {
		tt.manager.PushUploaded(upload)
	}
	tt.UploadTotal.Add(upload)
	return n, err
}

func (tt *tcpTracker) WriteBuffer(buffer *buf.Buffer) (err error) {
	if tt.observed != nil {
		tt.observed.mu.RLock()
		defer tt.observed.mu.RUnlock()
	}
	upload := int64(buffer.Len())
	err = tt.Conn.WriteBuffer(buffer)
	if err != nil {
		return err
	}
	if tt.pushToManager {
		tt.manager.PushUploaded(upload)
	}
	tt.UploadTotal.Add(upload)
	return
}

func (tt *tcpTracker) UnwrapWriter() (io.Writer, []N.CountFunc) {
	if tt.observed != nil {
		return writerOnly{tt}, nil
	}
	return tt.Conn, []N.CountFunc{func(upload int64) {
		if tt.pushToManager {
			tt.manager.PushUploaded(upload)
		}
		tt.UploadTotal.Add(upload)
	}}
}

func (tt *tcpTracker) Close() error {
	tt.manager.Leave(tt)
	err := tt.Conn.Close()
	if tt.observed != nil {
		tt.observed.closed()
	}
	return err
}

func (tt *tcpTracker) Upstream() any {
	return tt.Conn
}

func NewTCPTracker(conn C.Conn, manager *Manager, metadata *C.Metadata, rule C.Rule, uploadTotal int64, downloadTotal int64, pushToManager bool, observe ...func(*TrackerInfo) func()) *tcpTracker {
	metadata.RemoteDst = conn.RemoteDestination()

	t := &tcpTracker{
		Conn:    conn,
		manager: manager,
		TrackerInfo: &TrackerInfo{
			UUID:          utils.NewUUIDV4(),
			Start:         time.Now(),
			Metadata:      metadata,
			Chain:         conn.Chains(),
			ProviderChain: conn.ProviderChains(),
			Rule:          "",
			UploadTotal:   atomic.NewInt64(uploadTotal),
			DownloadTotal: atomic.NewInt64(downloadTotal),
		},
		pushToManager: pushToManager,
	}

	if pushToManager {
		if uploadTotal > 0 {
			manager.PushUploaded(uploadTotal)
		}
		if downloadTotal > 0 {
			manager.PushDownloaded(downloadTotal)
		}
	}

	if rule != nil {
		t.TrackerInfo.Rule = rule.RuleType().String()
		t.TrackerInfo.RulePayload = rule.Payload()
	}

	if len(observe) > 0 && observe[0] != nil {
		if finish := observe[0](t.Info()); finish != nil {
			t.observed = &observedIO{finish: finish}
		}
	}
	manager.Join(t)
	return t
}

type udpTracker struct {
	observed         *observedIO
	C.PacketConn     `json:"-"`
	firstDestination string
	*TrackerInfo
	manager *Manager

	pushToManager bool `json:"-"`
}

func (ut *udpTracker) ID() string {
	return ut.UUID.String()
}

func (ut *udpTracker) Info() *TrackerInfo {
	return ut.TrackerInfo
}

func (ut *udpTracker) ReadFrom(b []byte) (int, net.Addr, error) {
	if ut.observed != nil {
		ut.observed.mu.RLock()
		defer ut.observed.mu.RUnlock()
	}
	n, addr, err := ut.PacketConn.ReadFrom(b)
	download := int64(n)
	if ut.pushToManager {
		ut.manager.PushDownloaded(download)
	}
	ut.DownloadTotal.Add(download)
	return n, addr, err
}

func (ut *udpTracker) WaitReadFrom() (data []byte, put func(), addr net.Addr, err error) {
	if ut.observed != nil {
		ut.observed.mu.RLock()
		defer ut.observed.mu.RUnlock()
	}
	data, put, addr, err = ut.PacketConn.WaitReadFrom()
	download := int64(len(data))
	if ut.pushToManager {
		ut.manager.PushDownloaded(download)
	}
	ut.DownloadTotal.Add(download)
	return
}

// ObserveDestination tracks the addresses sharing this NAT association.
// The packet sender calls it from its single writer before sending a packet.
func (ut *udpTracker) ObserveDestination(destination string) {
	if ut.firstDestination == "" {
		ut.firstDestination = destination
	} else if destination != ut.firstDestination {
		ut.MultipleDestinations.Store(true)
	}
}

func (ut *udpTracker) WriteTo(b []byte, addr net.Addr) (int, error) {
	if ut.observed != nil {
		ut.observed.mu.RLock()
		defer ut.observed.mu.RUnlock()
	}
	n, err := ut.PacketConn.WriteTo(b, addr)
	upload := int64(n)
	if ut.pushToManager {
		ut.manager.PushUploaded(upload)
	}
	ut.UploadTotal.Add(upload)
	return n, err
}

func (ut *udpTracker) Close() error {
	ut.manager.Leave(ut)
	err := ut.PacketConn.Close()
	if ut.observed != nil {
		ut.observed.closed()
	}
	return err
}

func (ut *udpTracker) Upstream() any {
	return ut.PacketConn
}

func NewUDPTracker(conn C.PacketConn, manager *Manager, metadata *C.Metadata, rule C.Rule, uploadTotal int64, downloadTotal int64, pushToManager bool, observe ...func(*TrackerInfo) func()) *udpTracker {
	metadata.RemoteDst = conn.RemoteDestination()

	ut := &udpTracker{
		PacketConn: conn,
		manager:    manager,
		TrackerInfo: &TrackerInfo{
			UUID:          utils.NewUUIDV4(),
			Start:         time.Now(),
			Metadata:      metadata,
			Chain:         conn.Chains(),
			ProviderChain: conn.ProviderChains(),
			Rule:          "",
			UploadTotal:   atomic.NewInt64(uploadTotal),
			DownloadTotal: atomic.NewInt64(downloadTotal),
		},
		pushToManager: pushToManager,
	}

	if pushToManager {
		if uploadTotal > 0 {
			manager.PushUploaded(uploadTotal)
		}
		if downloadTotal > 0 {
			manager.PushDownloaded(downloadTotal)
		}
	}

	if rule != nil {
		ut.TrackerInfo.Rule = rule.RuleType().String()
		ut.TrackerInfo.RulePayload = rule.Payload()
	}

	if len(observe) > 0 && observe[0] != nil {
		if finish := observe[0](ut.Info()); finish != nil {
			ut.observed = &observedIO{finish: finish}
		}
	}
	manager.Join(ut)
	return ut
}
