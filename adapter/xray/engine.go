package xray

import (
	"context"
	"crypto/x509"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/readeem/mixray/adapter"
	"github.com/readeem/mixray/adapter/outbound"
	"github.com/readeem/mixray/common/structure"
	"github.com/readeem/mixray/component/ca"
	"github.com/readeem/mixray/component/dialer"
	"github.com/readeem/mixray/component/resolver"
	C "github.com/readeem/mixray/constant"
	"github.com/readeem/mixray/log"
)

// activeEngine stores 0 for xray (default) and 1 for mihomo.
var activeEngine atomic.Int32

func Engine() C.Engine {
	if activeEngine.Load() == 1 {
		return C.EngineMihomo
	}
	return C.EngineXray
}

func SetEngine(engine C.Engine) {
	if engine == C.EngineMihomo {
		activeEngine.Store(1)
	} else {
		activeEngine.Store(0)
	}
}

type ProxySpec struct {
	Name    string
	Type    string
	Options map[string]any
}

type ParsedProxy struct {
	Proxy   C.Proxy
	Mapping map[string]any
}

type PrepareProxies func([]ParsedProxy)

// PrepareBatches binds trust policy to a parsed config and its providers.
// Preparation leaves sockets and Xray engines unopened until the first dial.
func PrepareBatches(customTrust bool) PrepareProxies {
	return prepareBatches(customTrust, sync.OnceValue(ca.NewCertPool))
}

func prepareBatches(customTrust bool, roots func() *x509.CertPool) PrepareProxies {
	keepNativeTrust := customTrust || ca.DisableSystemCa || ca.DisableEmbedCa
	return func(proxies []ParsedProxy) {
		batch := &outboundBatch{}
		decoder := structure.NewDecoder(structure.Option{WeaklyTypedInput: true})
		for _, entry := range proxies {
			proxy := entry.Proxy.(*adapter.Proxy)
			kind, _ := entry.Mapping["type"].(string)
			switch kind {
			case "vless", "vmess", "trojan", "hysteria2", "ss":
			default:
				continue
			}
			options := maps.Clone(entry.Mapping)
			delete(options, "name")
			delete(options, "type")
			if !normalizeOptions(decoder, options) || !usesNativeTLSName(kind, options) {
				continue
			}
			if usesCertificateAuthorities(kind, options) {
				if keepNativeTrust {
					continue
				}
				batch.roots = roots
			}

			batch.proxies = append(batch.proxies, ProxySpec{Name: proxy.Name(), Type: kind, Options: options})
			proxy.ProxyAdapter = &batchedOutbound{ProxyAdapter: proxy.ProxyAdapter.(outbound.ProxyAdapter), batch: batch}
		}
	}
}

func RetireProxy(proxy C.Proxy) {
	if p, ok := proxy.(*adapter.Proxy); ok {
		if owned, ok := p.ProxyAdapter.(interface{ Retire() }); ok {
			owned.Retire()
		}
	}
}

func Wrap(base outbound.ProxyAdapter, native *Outbound) outbound.ProxyAdapter {
	return &preferredOutbound{ProxyAdapter: base, native: native}
}

type preferredOutbound struct {
	outbound.ProxyAdapter
	native *Outbound
}

func (p *preferredOutbound) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	host := metadata.Host
	if host == "" {
		host = metadata.DstIP.String()
	}
	conn, err := p.native.DialContext(ctx, host, metadata.DstPort)
	if err != nil {
		return nil, err
	}
	return outbound.NewConn(conn, p), nil
}

func (p *preferredOutbound) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	target := metadata.Clone()
	if err := p.ResolveUDP(ctx, target); err != nil {
		return nil, err
	}
	conn, err := p.native.ListenPacket(ctx, target.UDPAddr())
	if err != nil {
		return nil, err
	}
	return outbound.NewPacketConn(conn, p), nil
}

type MihomoNetwork struct{}

var _ Network = MihomoNetwork{}

func (MihomoNetwork) DialContext(ctx context.Context, address string, options SocketOptions) (net.Conn, error) {
	if options.Bootstrap {
		ctx = WithBootstrapLookup(ctx)
	}
	opts := []dialer.Option{
		dialer.WithResolver(resolver.ProxyServerHostResolver),
		dialer.WithTFO(options.TFO), dialer.WithMPTCP(options.MPTCP),
	}
	switch options.IPVersion {
	case "ipv4":
		opts = append(opts, dialer.WithOnlySingleStack(true))
	case "ipv6":
		opts = append(opts, dialer.WithOnlySingleStack(false))
	case "ipv4-prefer":
		opts = append(opts, dialer.WithPreferIPv4())
	case "ipv6-prefer":
		opts = append(opts, dialer.WithPreferIPv6())
	}
	return dialer.DialContext(ctx, "tcp", address, opts...)
}

func (MihomoNetwork) ListenPacket(ctx context.Context, address string, options SocketOptions) (net.PacketConn, *net.UDPAddr, error) {
	if options.Bootstrap {
		ctx = WithBootstrapLookup(ctx)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, err
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		r := resolver.ProxyServerHostResolver
		switch options.IPVersion {
		case "ipv4":
			ip, err = resolver.ResolveIPv4WithResolver(ctx, host, r)
		case "ipv6":
			ip, err = resolver.ResolveIPv6WithResolver(ctx, host, r)
		case "ipv4-prefer":
			ip, err = resolver.ResolveIPv4WithResolver(ctx, host, r)
			if err != nil {
				ip, err = resolver.ResolveIPv6WithResolver(ctx, host, r)
			}
		case "ipv6-prefer":
			ip, err = resolver.ResolveIPv6WithResolver(ctx, host, r)
			if err != nil {
				ip, err = resolver.ResolveIPv4WithResolver(ctx, host, r)
			}
		default:
			ip, err = resolver.ResolveIPWithResolver(ctx, host, r)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	remote := netip.AddrPortFrom(ip.Unmap(), uint16(n))
	network := "udp4"
	if remote.Addr().Is6() {
		network = "udp6"
	}
	conn, err := dialer.ListenPacket(ctx, network, "", remote)
	return conn, net.UDPAddrFromAddrPort(remote), err
}

func usesNativeTLSName(kind string, options map[string]any) bool {
	_, sni := options["sni"]
	_, serverName := options["servername"]
	switch kind {
	case "vless", "vmess":
		return !sni
	case "trojan", "hysteria2":
		return !serverName
	default:
		return !sni && !serverName && options["tls"] != true && options["reality-opts"] == nil
	}
}

func usesCertificateAuthorities(kind string, options map[string]any) bool {
	reality, _ := options["reality-opts"].(map[string]any)
	return kind == "hysteria2" || len(reality) == 0 && (kind == "trojan" || options["tls"] == true)
}

// Keep unsupported keys for Xray's allowlist and preserve the provider's original mapping.
func normalizeOptions(decoder *structure.Decoder, options map[string]any) bool {
	for key, value := range options {
		if value == nil {
			return false
		}
		var err error
		switch key {
		case "port", "alterId", "max-early-data":
			options[key], err = decodeOption[int](decoder, value)
		case "tls", "udp", "tfo", "mptcp", "xudp", "skip-cert-verify", "no-grpc-header", "support-x25519mlkem768":
			options[key], err = decodeOption[bool](decoder, value)
		case "alpn":
			var protocols []string
			protocols, err = decodeOption[[]string](decoder, value)
			alpn := make([]any, len(protocols))
			for i, protocol := range protocols {
				alpn[i] = protocol
			}
			options[key] = alpn
		case "ws-opts", "grpc-opts", "xhttp-opts", "reality-opts":
			nested, ok := value.(map[string]any)
			if !ok {
				return false
			}
			nested = maps.Clone(nested)
			if !normalizeOptions(decoder, nested) {
				return false
			}
			options[key] = nested
		case "headers":
			var headers map[string]string
			headers, err = decodeOption[map[string]string](decoder, value)
			fields := make(map[string]any, len(headers))
			for name, header := range headers {
				fields[name] = header
			}
			options[key] = fields
		default:
			if _, ok := value.(string); !ok {
				return false
			}
		}
		if err != nil {
			return false
		}
	}
	return true
}

func decodeOption[T any](decoder *structure.Decoder, value any) (T, error) {
	var field struct{ Value T }
	err := decoder.Decode(map[string]any{"Value": value}, &field)
	return field.Value, err
}

type outboundBatch struct {
	mu      sync.Mutex
	proxies []ProxySpec
	roots   func() *x509.CertPool
	engine  *Outbounds
	retired bool
}

func (b *outboundBatch) get(name string) (*Outbound, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retired {
		return nil, net.ErrClosed
	}
	// Validation and rejected updates never start engines.
	if len(b.proxies) != 0 {
		var roots *x509.CertPool
		if b.roots != nil {
			roots = b.roots()
		}
		engine, err := New(b.proxies, MihomoNetwork{}, roots)
		b.proxies = nil
		b.roots = nil
		if err != nil {
			log.Warnln("Xray outbounds unavailable: %s", err)
		}
		b.engine = engine
	}
	if b.engine == nil {
		return nil, nil
	}
	return b.engine.Get(name), nil
}

func (b *outboundBatch) retire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.retired = true
	b.proxies = nil
	b.roots = nil
	if b.engine != nil {
		if err := b.engine.Close(); err != nil {
			log.Warnln("Closing Xray outbounds: %s", err)
		}
		b.engine = nil
	}
}

type batchedOutbound struct {
	outbound.ProxyAdapter
	batch *outboundBatch
}

func (p *batchedOutbound) active() (outbound.ProxyAdapter, error) {
	if Engine() != C.EngineXray {
		return p.ProxyAdapter, nil
	}
	native, err := p.batch.get(p.Name())
	if err != nil || native == nil {
		return p.ProxyAdapter, err
	}
	return Wrap(p.ProxyAdapter, native), nil
}

func (p *batchedOutbound) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	proxy, err := p.active()
	if err != nil {
		return nil, err
	}
	return proxy.DialContext(ctx, metadata)
}

func (p *batchedOutbound) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	proxy, err := p.active()
	if err != nil {
		return nil, err
	}
	return proxy.ListenPacketContext(ctx, metadata)
}

func (p *batchedOutbound) Retire() {
	p.batch.retire()
}
