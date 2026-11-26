// Package xray supplies Xray-core outbound transports for mixray's routing engine.
package xray

import (
	"context"
	cryptotls "crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/readeem/mixray/internal/xray-core/common"
	"github.com/readeem/mixray/internal/xray-core/common/buf"
	xnet "github.com/readeem/mixray/internal/xray-core/common/net"
	"github.com/readeem/mixray/internal/xray-core/common/session"
	xcore "github.com/readeem/mixray/internal/xray-core/core"
	"github.com/readeem/mixray/internal/xray-core/features/outbound"
	"github.com/readeem/mixray/internal/xray-core/infra/conf"
	"github.com/readeem/mixray/internal/xray-core/transport"
	"github.com/readeem/mixray/internal/xray-core/transport/internet"
	"github.com/readeem/mixray/internal/xray-core/transport/internet/stat"

	_ "github.com/readeem/mixray/internal/xray-core/app/dispatcher"
	_ "github.com/readeem/mixray/internal/xray-core/app/log"
	_ "github.com/readeem/mixray/internal/xray-core/app/proxyman/inbound"
	proxyman "github.com/readeem/mixray/internal/xray-core/app/proxyman/outbound"
	_ "github.com/readeem/mixray/internal/xray-core/proxy/hysteria"
	_ "github.com/readeem/mixray/internal/xray-core/proxy/shadowsocks"
	_ "github.com/readeem/mixray/internal/xray-core/proxy/trojan"
	_ "github.com/readeem/mixray/internal/xray-core/proxy/vless/outbound"
	_ "github.com/readeem/mixray/internal/xray-core/proxy/vmess/outbound"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/grpc"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/hysteria"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/reality"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/splithttp"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/tcp"
	"github.com/readeem/mixray/internal/xray-core/transport/internet/tls"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/udp"
	_ "github.com/readeem/mixray/internal/xray-core/transport/internet/websocket"
)

type SocketOptions struct {
	IPVersion string
	TFO       bool
	MPTCP     bool
	Bootstrap bool
}

type Network interface {
	DialContext(ctx context.Context, address string, options SocketOptions) (net.Conn, error)
	ListenPacket(ctx context.Context, address string, options SocketOptions) (net.PacketConn, *net.UDPAddr, error)
}

type Outbounds struct {
	instance *xcore.Instance
	ctx      context.Context
	cancel   context.CancelFunc
	byName   map[string]*Outbound
	keys     []string
}

type Outbound struct {
	ctx       context.Context
	handler   *proxyman.Handler
	bootstrap *proxyman.Handler
}

type socketPolicy struct {
	ctx     context.Context
	network Network
	options SocketOptions
}

const socketPrefix = "mixray/"

var (
	nextRuntime    atomic.Uint64
	policies       sync.Map
	errUnreachable = errors.New("could not connect through the Xray outbound")
)

type ownerContextConfig struct{}

func init() {
	internet.UseAlternativeSystemDialer(&systemDialer{})
	common.Must(common.RegisterConfig(ownerContextConfig{}, func(ctx context.Context, _ any) (any, error) {
		return ctx, nil
	}))
}

// New uses mixray's socket policy for every outbound, including connections
// opened by a pooled Xray transport after the original dial returns.
func New(configs []ProxySpec, network Network, roots *x509.CertPool) (*Outbounds, error) {
	ctx, cancel := context.WithCancel(context.Background())
	o := &Outbounds{ctx: ctx, cancel: cancel, byName: make(map[string]*Outbound)}
	prefix := socketPrefix + strconv.FormatUint(nextRuntime.Add(1), 10) + "/"
	supported := make(map[string]ProxySpec)
	var handlers []*xcore.OutboundHandlerConfig
	for i, p := range configs {
		tag := proxyTag(i)
		key := prefix + tag
		config, reason := proxyOutbound(tag, key, p)
		if reason != "" {
			continue
		}
		opts := fields(p.Options)
		options := SocketOptions{
			IPVersion: opts.string("ip-version"), TFO: opts["tfo"] == true, MPTCP: opts["mptcp"] == true,
		}
		policies.Store(key, socketPolicy{ctx, network, options})
		o.keys = append(o.keys, key)
		supported[tag] = p
		handlers = append(handlers, config)
		// Pooled transports can dial with a context unrelated to the request
		// that needs DNS. Keep recursive bootstrap on a separate pool.
		if needsBootstrapPool(p) {
			bootstrapTag, bootstrapKey := tag+"-bootstrap", key+"-bootstrap"
			bootstrap, reason := proxyOutbound(bootstrapTag, bootstrapKey, p)
			if reason != "" {
				o.Close()
				return nil, errors.New("could not configure the Xray bootstrap outbound")
			}
			options.Bootstrap = true
			policies.Store(bootstrapKey, socketPolicy{ctx, network, options})
			o.keys = append(o.keys, bootstrapKey)
			handlers = append(handlers, bootstrap)
		}
	}
	if len(supported) == 0 {
		return o, nil
	}
	cfg, err := (&conf.Config{LogConfig: &conf.LogConfig{LogLevel: "none"}}).Build()
	if err != nil {
		o.Close()
		return nil, err
	}
	cfg.Outbound = handlers
	o.instance, err = xcore.NewWithContext(tls.WithRootCAs(ctx, roots), cfg)
	if err == nil {
		err = o.instance.Start()
	}
	if err != nil {
		o.Close()
		// Xray config and start errors can contain credentials.
		return nil, errors.New("could not start the Xray outbounds")
	}
	manager := o.instance.GetFeature(outbound.ManagerType()).(outbound.Manager)
	owner, err := xcore.CreateObject(o.instance, ownerContextConfig{})
	if err != nil {
		o.Close()
		return nil, errors.New("could not bind Xray outbounds to their owner")
	}
	for tag, proxy := range supported {
		bootstrap, _ := manager.GetHandler(tag + "-bootstrap").(*proxyman.Handler)
		o.byName[proxy.Name] = &Outbound{ctx: owner.(context.Context), handler: manager.GetHandler(tag).(*proxyman.Handler), bootstrap: bootstrap}
	}
	return o, nil
}

func (o *Outbounds) Get(name string) *Outbound { return o.byName[name] }

func (o *Outbounds) Close() error {
	o.cancel()
	for _, key := range o.keys {
		policies.Delete(key)
	}
	if o.instance != nil {
		return o.instance.Close()
	}
	return nil
}

func needsBootstrapPool(p ProxySpec) bool {
	opts := fields(p.Options)
	if !isDomain(opts.string("server")) {
		return false
	}
	switch opts.string("network") {
	case "grpc", "xhttp", "splithttp":
		return true
	default:
		return p.Type == "hysteria2"
	}
}

type bootstrapLookup struct{}

func WithBootstrapLookup(ctx context.Context) context.Context {
	return context.WithValue(ctx, bootstrapLookup{}, true)
}

func IsBootstrapLookup(ctx context.Context) bool {
	return ctx.Value(bootstrapLookup{}) == true
}

type sessionContext struct {
	context.Context
	owner context.Context
}

func (c sessionContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.owner.Value(key)
}

func (o *Outbound) session(ctx context.Context, dest xnet.Destination) (context.Context, context.CancelFunc, *proxyman.Handler) {
	handler := o.handler
	if o.bootstrap != nil && IsBootstrapLookup(ctx) {
		handler = o.bootstrap
	}
	run, cancel := context.WithCancel(sessionContext{context.WithoutCancel(ctx), o.ctx})
	stop := context.AfterFunc(o.ctx, cancel)
	run = session.ContextWithInbound(run, &session.Inbound{})
	run = session.ContextWithOutbounds(run, []*session.Outbound{{
		Tag: handler.Tag(), Target: dest, OriginalTarget: dest,
	}})
	return run, func() { stop(); cancel() }, handler
}

func (o *Outbound) DialContext(ctx context.Context, host string, port uint16) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run, cancel, handler := o.session(ctx, tcpDestination(host, port))
	client, relay := streamPipePair()
	disarm := context.AfterFunc(run, func() { relay.Close() })
	ready := make(chan struct{})
	deadline, _ := ctx.Deadline()
	d := &tcpAttempt{Dialer: handler, ready: ready, deadline: deadline}
	run = context.WithValue(run, attemptKey{}, d)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		defer disarm()
		defer relay.Close()
		reader := &buf.TimeoutWrapperReader{Reader: buf.NewReader(relay)}
		writer := relayWriter{buf.NewWriter(relay), relay}
		handler.GetOutbound().Process(run, &transport.Link{Reader: reader, Writer: writer}, d)
	}()
	select {
	case <-ready:
		return &sessionConn{streamPipe: client, cancel: cancel}, nil
	case <-done:
	case <-ctx.Done():
	case <-o.ctx.Done():
	}
	client.Close()
	cancel()
	if d.failed.Load() && !d.opened.Load() {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errUnreachable}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreachable, err)
	}
	return nil, errUnreachable
}

type attemptKey struct{}

type sessionConn struct {
	*streamPipe
	cancel context.CancelFunc
}

func (c *sessionConn) Close() error {
	c.cancel()
	return c.streamPipe.Close()
}

type tcpAttempt struct {
	internet.Dialer
	ready    chan struct{}
	once     sync.Once
	deadline time.Time
	opened   atomic.Bool
	failed   atomic.Bool
}

func (d *tcpAttempt) connected() { d.once.Do(func() { close(d.ready) }) }

func (d *tcpAttempt) Dial(ctx context.Context, dest xnet.Destination) (stat.Connection, error) {
	conn, err := d.Dialer.Dial(ctx, dest)
	if err == nil {
		d.connected()
	}
	return conn, err
}

type systemDialer struct{ internet.DefaultSystemDialer }

func (d *systemDialer) Dial(ctx context.Context, src xnet.Address, dest xnet.Destination, opts *internet.SocketConfig) (net.Conn, error) {
	if opts == nil || !strings.HasPrefix(opts.Interface, socketPrefix) {
		return d.DefaultSystemDialer.Dial(ctx, src, dest, opts)
	}
	slot, ok := policies.Load(opts.Interface)
	if !ok {
		return nil, net.ErrClosed
	}
	policy := slot.(socketPolicy)
	timeout := 5 * time.Second
	attempt, _ := ctx.Value(attemptKey{}).(*tcpAttempt)
	if attempt != nil && !attempt.deadline.IsZero() {
		timeout = min(timeout, time.Until(attempt.deadline))
	}
	dial, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(policy.ctx, cancel)
	defer stop()
	if policy.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if dest.Network == xnet.Network_UDP {
		packet, address, err := policy.network.ListenPacket(dial, dest.NetAddr(), policy.options)
		if err != nil {
			return nil, err
		}
		return &xnet.PacketConnWrapper{
			PacketConn: &trackedPacket{PacketConn: packet, stop: context.AfterFunc(policy.ctx, func() { packet.Close() })},
			Dest:       address,
		}, nil
	}
	conn, err := policy.network.DialContext(dial, dest.NetAddr(), policy.options)
	if attempt != nil {
		if err == nil {
			attempt.opened.Store(true)
		} else if isRecoverableDialError(err) {
			attempt.failed.Store(true)
		}
	}
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: conn, stop: context.AfterFunc(policy.ctx, func() { conn.Close() })}, nil
}

func isRecoverableDialError(err error) bool {
	for nested := err; nested != nil; nested = errors.Unwrap(nested) {
		if joined, ok := nested.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				if !isRecoverableDialError(cause) {
					return false
				}
			}
			return len(joined.Unwrap()) > 0
		}
	}

	var (
		verify    *cryptotls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		record    cryptotls.RecordHeaderError
		alert     cryptotls.AlertError
	)
	if errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &record) || errors.As(err, &alert) {
		return false
	}
	var (
		op     *net.OpError
		lookup *net.DNSError
	)
	if errors.As(err, &lookup) && lookup.IsNotFound {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &op) || errors.As(err, &lookup)
}

type trackedConn struct {
	net.Conn
	stop func() bool
}

func (c *trackedConn) CloseWrite() error { return stat.CloseWrite(c.Conn) }

func (c *trackedConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

type trackedPacket struct {
	net.PacketConn
	stop func() bool
}

func (c *trackedPacket) Close() error {
	c.stop()
	return c.PacketConn.Close()
}
