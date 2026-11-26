package jls

import (
	"context"
	"net"

	N "github.com/readeem/mixray/common/net"
	C "github.com/readeem/mixray/constant"
	LC "github.com/readeem/mixray/listener/config"
	"github.com/readeem/mixray/listener/inner"
	"github.com/readeem/mixray/transport/jls"
)

type Builder struct {
	config *jls.ServerConfig
}

func New(config LC.JLSConfig, tunnel C.Tunnel) (*Builder, error) {
	users := make([]jls.User, len(config.Users))
	for i, user := range config.Users {
		users[i] = jls.User{Username: user.Username, Password: user.Password}
	}
	serverConfig, err := jls.NewServerConfig(
		config.SNI,
		config.Dest,
		users,
		config.ALPN,
		config.RateLimit,
		func(ctx context.Context, network, address string) (net.Conn, error) {
			return inner.HandleTcp(tunnel, address, config.Proxy)
		},
	)
	if err != nil {
		return nil, err
	}
	return &Builder{config: serverConfig}, nil
}

func (b Builder) NewListener(listener net.Listener) net.Listener {
	return N.NewHandleContextListener(context.Background(), listener, func(ctx context.Context, conn net.Conn) (net.Conn, error) {
		return jls.Server(ctx, conn, b.config)
	}, nil)
}

func UserFromConn(conn net.Conn) (string, bool) {
	return jls.UserFromConn(conn)
}
