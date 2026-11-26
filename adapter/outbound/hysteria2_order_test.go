package outbound_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/readeem/mixray/adapter/xray"
	C "github.com/readeem/mixray/constant"
	"github.com/readeem/mixray/hub/executor"

	"github.com/metacubex/http"
	"github.com/metacubex/quic-go"
	"github.com/metacubex/quic-go/http3"
	"github.com/metacubex/quic-go/quicvarint"
	"github.com/metacubex/tls"
)

func TestHysteriaReceivesDatagramsInPeerOrder(t *testing.T) {
	previous := goruntime.GOMAXPROCS(1)
	t.Cleanup(func() { goruntime.GOMAXPROCS(previous) })
	for _, engine := range []C.Engine{C.EngineMihomo, C.EngineXray} {
		t.Run(string(engine), func(t *testing.T) { testSelfSignedHysteria(t, engine) })
	}
}

func testSelfSignedHysteria(t *testing.T, engine C.Engine) {
	t.Helper()
	originalEngine := xray.Engine()
	xray.SetEngine(engine)
	t.Cleanup(func() { xray.SetEngine(originalEngine) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: cert.Certificate, PrivateKey: cert.PrivateKey}},
		NextProtos:   []string{"h3"},
	}, &quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const batchSize, batches = 16, 16
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return err
			}
			defer conn.CloseWithError(0, "")
			server := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/auth" || r.Header.Get("Hysteria-Auth") != "test" {
					w.WriteHeader(403)
					return
				}
				w.Header().Set("Hysteria-UDP", "true")
				w.Header().Set("Hysteria-CC-RX", "0")
				w.WriteHeader(233)
			})}
			server.StreamDispatcher = func(frame http3.FrameType, stream *quic.Stream, err error) (bool, error) {
				if err != nil || frame != 0x401 {
					return false, err
				}
				defer stream.Close()
				deadline, _ := ctx.Deadline()
				stream.SetDeadline(deadline)
				return true, echoHysteriaStream(stream)
			}
			served := make(chan error, 1)
			go func() { served <- server.ServeQUICConn(conn) }()
			defer func() { conn.CloseWithError(0, ""); <-served }()
			for batch := range batches {
				request, err := conn.ReceiveDatagram(ctx)
				if err != nil {
					return err
				}
				if len(request) < 9 {
					return fmt.Errorf("short Hysteria datagram: %d bytes", len(request))
				}
				for offset := range batchSize {
					response := append([]byte(nil), request[:len(request)-4]...)
					response = binary.BigEndian.AppendUint32(response, uint32(batch*batchSize+offset))
					if err := conn.SendDatagram(response); err != nil {
						return err
					}
				}
			}
			_, err = conn.ReceiveDatagram(ctx)
			return err
		}()
	}()
	t.Cleanup(func() { cancel(); listener.Close(); <-done })

	yamlCfg := fmt.Sprintf(`
engine: %s
proxies:
  - name: Ordered
    type: hysteria2
    server: 127.0.0.1
    port: %d
    password: test
    skip-cert-verify: true
`, engine, listener.Addr().(*net.UDPAddr).Port)
	cfg, err := executor.ParseWithBytes([]byte(yamlCfg))
	if err != nil {
		t.Fatal(err)
	}
	proxy := cfg.Proxies["Ordered"]
	t.Cleanup(func() {
		xray.RetireProxy(proxy)
		proxy.Close()
	})

	stream, err := proxy.DialContext(ctx, &C.Metadata{
		NetWork: C.TCP, DstIP: netip.MustParseAddr("192.0.2.1"), DstPort: 7777,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	deadline, _ := ctx.Deadline()
	stream.SetDeadline(deadline)
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var echo [4]byte
	if _, err := io.ReadFull(stream, echo[:]); err != nil || string(echo[:]) != "ping" {
		t.Fatalf("TCP echo %q: %v", echo, err)
	}
	packets, err := proxy.ListenPacketContext(ctx, &C.Metadata{
		NetWork: C.UDP, DstIP: netip.MustParseAddr("192.0.2.1"), DstPort: 7777,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer packets.Close()
	packets.SetReadDeadline(deadline)
	destination := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 7777}
	buffer := make([]byte, 4)
	for batch := range batches {
		if _, err := packets.WriteTo(buffer, destination); err != nil {
			t.Fatal(err)
		}
		for offset := range batchSize {
			n, _, err := packets.ReadFrom(buffer)
			if err != nil {
				t.Fatal(err)
			}
			want := uint32(batch*batchSize + offset)
			if n != 4 || binary.BigEndian.Uint32(buffer) != want {
				t.Fatalf("received datagram %d = %x; want sequence %d", want, buffer[:n], want)
			}
		}
	}
}

func echoHysteriaStream(stream *quic.Stream) error {
	header := quicvarint.NewReader(stream)
	if _, err := quicvarint.Read(header); err != nil {
		return err
	}
	for range 2 {
		size, err := quicvarint.Read(header)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, stream, int64(size)); err != nil {
			return err
		}
	}
	if _, err := stream.Write([]byte{0, 0, 0}); err != nil {
		return err
	}
	_, err := io.CopyN(stream, stream, 4)
	return err
}
