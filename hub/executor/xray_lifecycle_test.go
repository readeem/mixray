package executor_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/readeem/mixray/adapter"
	"github.com/readeem/mixray/adapter/xray"
	"github.com/readeem/mixray/component/ca"
	"github.com/readeem/mixray/config"
	C "github.com/readeem/mixray/constant"
	"github.com/readeem/mixray/hub/executor"
)

func TestReloadAndProviderRefreshRetireActiveOutbounds(t *testing.T) {
	home := t.TempDir()
	originalHome := C.Path.HomeDir()
	originalEngine := xray.Engine()
	C.SetHomeDir(home)
	xray.SetEngine(C.EngineXray)
	t.Cleanup(func() {
		C.SetHomeDir(originalHome)
		xray.SetEngine(originalEngine)
	})

	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	payload := func(name string) []byte {
		return fmt.Appendf(nil, "proxies:\n  - {name: %s, type: vless, server: 127.0.0.1, port: %d, uuid: 92d696db-0166-464e-91bb-b65f72b3d665}\n", name, port)
	}
	parse := func(t *testing.T, data []byte) *config.Config {
		t.Helper()
		cfg, err := executor.ParseWithBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("config reload", func(t *testing.T) {
		first := parse(t, payload("First"))
		executor.ApplyConfig(first, true)
		t.Cleanup(executor.Shutdown)
		old := first.Proxies["First"]
		wire := holdOutbound(t, listener, old)

		if _, err := executor.ParseWithBytes(append(payload("Rejected"), "rules: ['MATCH,Missing']\n"...)); err == nil {
			t.Fatal("invalid config passed validation")
		}
		holdOutbound(t, listener, old)

		second := parse(t, payload("Second"))
		executor.ApplyConfig(second, true)
		requireRetired(t, old, wire)
		holdOutbound(t, listener, second.Proxies["Second"])
	})

	t.Run("unchanged proxies stay open across reload", func(t *testing.T) {
		first := parse(t, append(payload("Same"), "rules: ['DOMAIN,a.example,Same']\n"...))
		executor.ApplyConfig(first, true)
		t.Cleanup(executor.Shutdown)
		wire := holdOutbound(t, listener, first.Proxies["Same"])

		second := parse(t, append(payload("Same"), "rules: ['DOMAIN,b.example,Same']\n"...))
		executor.ApplyConfig(second, true)
		if err := wire.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := wire.Read(b[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("reload with identical proxies closed active connection: %v", err)
		}
		wire.SetReadDeadline(time.Time{})

		third := parse(t, payload("Other"))
		executor.ApplyConfig(third, true)
		requireRetired(t, second.Proxies["Same"], wire)
	})

	t.Run("switching to mihomo retires active Xray batch", func(t *testing.T) {
		first := parse(t, append([]byte("engine: xray\n"), payload("Same")...))
		executor.ApplyConfig(first, true)
		t.Cleanup(executor.Shutdown)
		wire := holdOutbound(t, listener, first.Proxies["Same"])

		second := parse(t, append([]byte("engine: mihomo\n"), payload("Same")...))
		executor.ApplyConfig(second, true)
		if err := wire.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, wire); err != nil {
			t.Fatalf("switching to mihomo did not close active Xray socket: %v", err)
		}

		third := parse(t, append([]byte("engine: xray\n"), payload("Same")...))
		executor.ApplyConfig(third, true)
		if err := xray.Start(third.Proxies); err != nil {
			t.Fatalf("xray.Start after switching back: %v", err)
		}
		holdOutbound(t, listener, third.Proxies["Same"])
	})

	t.Run("wrapped proxy retires", func(t *testing.T) {
		type wrapped struct{ C.Proxy }
		cfg := parse(t, payload("Wrapped"))
		cfg.Proxies["Wrapped"] = wrapped{Proxy: cfg.Proxies["Wrapped"]}
		executor.ApplyConfig(cfg, true)
		t.Cleanup(executor.Shutdown)
		wire := holdOutbound(t, listener, cfg.Proxies["Wrapped"])

		next := parse(t, payload("Replacement"))
		executor.ApplyConfig(next, true)
		requireRetired(t, cfg.Proxies["Wrapped"], wire)
	})

	t.Run("provider refresh", func(t *testing.T) {
		cfg := parse(t, []byte("proxy-providers:\n  file:\n    type: file\n    path: ./provider.yaml\n"))
		provider := cfg.Providers["file"]
		t.Cleanup(func() { provider.(io.Closer).Close() })
		refresh := func(name string) C.Proxy {
			t.Helper()
			if err := os.WriteFile(filepath.Join(home, "provider.yaml"), payload(name), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := provider.Update(); err != nil {
				t.Fatal(err)
			}
			return provider.Proxies()[0]
		}

		old := refresh("First")
		wire := holdOutbound(t, listener, old)
		next := refresh("Second")
		requireRetired(t, old, wire)
		holdOutbound(t, listener, next)
	})
}

func TestPreservesTLSRootsAcrossReloadAndProviderRefresh(t *testing.T) {
	home := t.TempDir()
	originalHome := C.Path.HomeDir()
	system, embedded := ca.DisableSystemCa, ca.DisableEmbedCa
	t.Cleanup(func() {
		C.SetHomeDir(originalHome)
		ca.DisableSystemCa, ca.DisableEmbedCa = system, embedded
	})
	C.SetHomeDir(home)
	ca.DisableSystemCa, ca.DisableEmbedCa = false, false

	const entry = "{name: %s, type: vless, server: 127.0.0.1, port: 443, uuid: 92d696db-0166-464e-91bb-b65f72b3d665, tls: %t}"
	parse := func(trust string) *config.Config {
		t.Helper()
		cfg, err := executor.ParseWithBytes([]byte(trust + fmt.Sprintf("proxies:\n  - "+entry+"\n  - "+entry+"\n", "TLS", true, "Plain", false) +
			"proxy-providers:\n  refresh:\n    type: file\n    path: ./provider.yaml\n"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cfg.Proxies["TLS"].Close()
			cfg.Proxies["Plain"].Close()
			cfg.Providers["refresh"].(io.Closer).Close()
		})
		return cfg
	}
	isXrayWrapped := func(p C.Proxy) bool {
		_, ok := p.(*adapter.Proxy).ProxyAdapter.(interface{ Retire() })
		return ok
	}
	check := func(p C.Proxy, wantXray bool) {
		t.Helper()
		if got := isXrayWrapped(p); got != wantXray {
			t.Errorf("%s: selected Xray = %t, want %t", p.Name(), got, wantXray)
		}
	}
	refresh := func(cfg *config.Config, name string) C.Proxy {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "provider.yaml"), []byte(fmt.Sprintf("proxies:\n  - "+entry+"\n", name, true)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Providers["refresh"].Update(); err != nil {
			t.Fatal(err)
		}
		p := cfg.Providers["refresh"].Proxies()[0]
		t.Cleanup(func() { p.Close() })
		return p
	}

	custom := parse("tls:\n  custom-certifactes: [test-root]\n")
	check(custom.Proxies["TLS"], false)
	check(custom.Proxies["Plain"], true)

	defaults := parse("")
	check(defaults.Proxies["TLS"], true)
	check(refresh(custom, "Custom provider"), false)
	check(refresh(defaults, "Default provider"), true)

	ca.DisableSystemCa = true
	noSys := parse("")
	check(noSys.Proxies["TLS"], false)
	check(noSys.Proxies["Plain"], true)

	ca.DisableSystemCa, ca.DisableEmbedCa = false, true
	noEmbed := parse("")
	check(noEmbed.Proxies["TLS"], false)
}

func TestConfigEngineParsing(t *testing.T) {
	original := xray.Engine()
	t.Cleanup(func() { xray.SetEngine(original) })

	cfg, err := executor.ParseWithBytes([]byte("mode: rule\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.Engine != C.EngineXray {
		t.Fatalf("default engine = %q, want %q", cfg.General.Engine, C.EngineXray)
	}

	cfg, err = executor.ParseWithBytes([]byte("engine: mihomo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.Engine != C.EngineMihomo {
		t.Fatalf("explicit engine = %q, want %q", cfg.General.Engine, C.EngineMihomo)
	}
	executor.ApplyConfig(cfg, true)
	t.Cleanup(executor.Shutdown)
	if got := executor.GetGeneral().Engine; got != C.EngineMihomo {
		t.Fatalf("GetGeneral().Engine = %q, want %q", got, C.EngineMihomo)
	}

	if _, err := executor.ParseWithBytes([]byte("engine: invalid\n")); err == nil {
		t.Fatal("invalid engine should fail config parse")
	}
}

func TestRealityClientHelloOffersHybridKeyExchange(t *testing.T) {
	originalEngine := xray.Engine()
	xray.SetEngine(C.EngineXray)
	t.Cleanup(func() { xray.SetEngine(originalEngine) })

	const xhttpFallback = `
    network: xhttp
    xhttp-opts: {mode: packet-up, sc-max-each-post-bytes: '65536'}`

	for _, tc := range []struct {
		name, fingerprint, transport string
		hybrid                       *bool
		wantHybrid                   bool
	}{
		{name: "chrome default", fingerprint: "chrome", wantHybrid: true},
		{name: "firefox default", fingerprint: "firefox", wantHybrid: true},
		{name: "explicit hybrid", fingerprint: "chrome", hybrid: new(true), wantHybrid: true},
		{name: "explicit legacy", fingerprint: "chrome", hybrid: new(false)},
		{name: "legacy fingerprint preserved", fingerprint: "firefox120", hybrid: new(true)},
		{name: "XHTTP fallback default", fingerprint: "chrome", transport: xhttpFallback, wantHybrid: true},
		{name: "XHTTP fallback explicit hybrid", fingerprint: "chrome", transport: xhttpFallback, hybrid: new(true), wantHybrid: true},
		{name: "XHTTP fallback explicit legacy", fingerprint: "chrome", transport: xhttpFallback, hybrid: new(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hellos := make(chan bool, 1)
			srv := httptest.NewUnstartedServer(http.NotFoundHandler())
			srv.Config.ErrorLog = log.New(io.Discard, "", 0)
			srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				select {
				case hellos <- slices.Contains(hello.SupportedCurves, tls.X25519MLKEM768):
				default:
				}
				return nil, errors.New("client hello captured")
			}}
			srv.StartTLS()
			t.Cleanup(srv.Close)
			host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatal(err)
			}
			hybridLine := ""
			if tc.hybrid != nil {
				hybridLine = fmt.Sprintf(", support-x25519mlkem768: %t", *tc.hybrid)
			}
			yamlCfg := fmt.Sprintf(`
proxies:
  - name: Reality
    type: vless
    server: %s
    port: %d
    uuid: 00000000-0000-4000-8000-000000000001
    tls: true
    servername: donor.example
    client-fingerprint: %s%s
    reality-opts: {public-key: %s, short-id: 0123456789abcdef%s}
`, host, port, tc.fingerprint, tc.transport, base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), hybridLine)

			cfg, err := executor.ParseWithBytes([]byte(yamlCfg))
			if err != nil {
				t.Fatal(err)
			}
			proxy := cfg.Proxies["Reality"]
			t.Cleanup(func() {
				xray.RetireProxy(proxy)
				proxy.Close()
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			dialDone := make(chan struct{})
			go func() {
				defer close(dialDone)
				if conn, err := proxy.DialContext(ctx, &C.Metadata{Host: "192.0.2.1", DstPort: 80}); err == nil {
					conn.Close()
				}
			}()
			select {
			case got := <-hellos:
				if got != tc.wantHybrid {
					t.Errorf("hybrid key exchange offered = %v, want %v", got, tc.wantHybrid)
				}
			case <-ctx.Done():
				t.Fatal("the REALITY client did not send a TLS ClientHello")
			}
			cancel()
			<-dialDone
		})
	}
}

func holdOutbound(t *testing.T, listener *net.TCPListener, proxy C.Proxy) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	metadata := &C.Metadata{Host: "example.test", DstPort: 80}
	conn, err := proxy.DialContext(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	wire, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wire.Close() })
	return wire
}

func requireRetired(t *testing.T, proxy C.Proxy, wire net.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if conn, err := proxy.DialContext(ctx, &C.Metadata{Host: "example.test", DstPort: 80}); !errors.Is(err, net.ErrClosed) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("dial through retired outbound returned %v", err)
	}
	if err := wire.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, wire); err != nil {
		t.Fatalf("retired outbound did not close its socket: %v", err)
	}
}
