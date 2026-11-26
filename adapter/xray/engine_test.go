package xray

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/readeem/mixray/adapter"
	"github.com/readeem/mixray/common/convert"
	"github.com/readeem/mixray/component/ca"
	C "github.com/readeem/mixray/constant"
)

func TestBatchPreservesTLSFromShareLinksWithMihomoRoots(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	setDefaultTrust(t)
	restoreEngine(t, C.EngineXray)

	cert, roots := testCertificate(t)
	const id = "92d696db-0166-464e-91bb-b65f72b3d665"
	link := "vless://" + id + "@" + listener.Addr().String() + "?security=tls&sni=proxy.test&alpn=http%2F1.1#VLESS"
	configs, err := convert.ConvertsV2Ray([]byte(link))
	if err != nil || len(configs) != 1 {
		t.Fatalf("convert share link: %d configs, %v", len(configs), err)
	}
	mapping := configs[0]
	mapping["tls"] = 1
	proxy := prepareProxy(t, mapping, prepareBatches(false, func() *x509.CertPool { return roots }))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	hello := make(chan *tls.ClientHelloInfo, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		server := tls.Server(conn, &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
			GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
				hello <- info
				return nil, nil
			},
		})
		result <- server.HandshakeContext(ctx)
	}()
	conn, err := proxy.DialContext(ctx, &C.Metadata{Host: "example.test", DstPort: 80})
	if err != nil {
		cancel()
		listener.Close()
		<-result
		t.Fatal(err)
	}
	defer conn.Close()
	info := <-hello
	if err := <-result; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	if info.ServerName != "proxy.test" || !slices.Contains(info.SupportedProtos, "http/1.1") {
		t.Fatalf("ClientHello SNI = %q, ALPN = %v", info.ServerName, info.SupportedProtos)
	}
}

func TestBatchKeepsUnrecognizedVlessSNIOnMihomo(t *testing.T) {
	setDefaultTrust(t)
	mapping := map[string]any{
		"name": "VLESS", "type": "vless", "server": "127.0.0.1", "port": 443,
		"uuid": "92d696db-0166-464e-91bb-b65f72b3d665", "tls": true, "sni": "ignored.test",
	}
	proxy, err := adapter.ParseProxy(mapping)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.Close() })
	parsed := proxy.(*adapter.Proxy)
	native := parsed.ProxyAdapter
	PrepareBatches(false)([]ParsedProxy{{Proxy: proxy, Mapping: mapping}})
	if parsed.ProxyAdapter != native {
		t.Fatal("Xray took over a VLESS proxy with an unrecognized TLS name")
	}
}

func TestBatchKeepsWebSocketEarlyDataOnMihomo(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	restoreEngine(t, C.EngineXray)
	mapping := map[string]any{
		"name": "early data", "type": "vless", "server": "127.0.0.1", "port": listener.Addr().(*net.TCPAddr).Port,
		"uuid": "92d696db-0166-464e-91bb-b65f72b3d665", "network": "ws",
		"ws-opts": map[string]any{"max-early-data": "2048"},
	}
	proxy := prepareProxy(t, mapping, PrepareBatches(false))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	conn, err := proxy.DialContext(ctx, &C.Metadata{Host: "example.test", DstPort: 80})
	if err != nil {
		t.Fatalf("early data handshake did not wait for a first write: %v", err)
	}
	conn.Close()
}

func TestEngineSwitchUsesMihomoWithoutStartingXray(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	restoreEngine(t, C.EngineMihomo)

	mapping := map[string]any{
		"name": "plain", "type": "vless", "server": "127.0.0.1", "port": listener.Addr().(*net.TCPAddr).Port,
		"uuid": "92d696db-0166-464e-91bb-b65f72b3d665",
	}
	proxy := prepareProxy(t, mapping, PrepareBatches(false))
	bo, ok := proxy.(*adapter.Proxy).ProxyAdapter.(*batchedOutbound)
	if !ok {
		t.Fatal("proxy was not wrapped in a batchedOutbound")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := proxy.DialContext(ctx, &C.Metadata{Host: "example.test", DstPort: 80})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if bo.batch.engine != nil {
		t.Fatal("mihomo engine mode started an Xray instance")
	}
}

func prepareProxy(t *testing.T, mapping map[string]any, prepare PrepareProxies) C.Proxy {
	t.Helper()
	proxy, err := adapter.ParseProxy(mapping)
	if err != nil {
		t.Fatal(err)
	}
	prepare([]ParsedProxy{{Proxy: proxy, Mapping: mapping}})
	t.Cleanup(func() {
		RetireProxy(proxy)
		proxy.Close()
	})
	return proxy
}

func setDefaultTrust(t *testing.T) {
	t.Helper()
	system, embedded := ca.DisableSystemCa, ca.DisableEmbedCa
	ca.DisableSystemCa, ca.DisableEmbedCa = false, false
	t.Cleanup(func() { ca.DisableSystemCa, ca.DisableEmbedCa = system, embedded })
}

func restoreEngine(t *testing.T, engine C.Engine) {
	t.Helper()
	previous := Engine()
	SetEngine(engine)
	t.Cleanup(func() { SetEngine(previous) })
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"proxy.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}, roots
}
