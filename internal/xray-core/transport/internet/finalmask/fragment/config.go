package fragment

import (
	"github.com/readeem/mixray/internal/xray-core/common/net"
	"github.com/readeem/mixray/internal/xray-core/transport/internet/finalmask"
)

func (c *Config) WrapConnClient(conn net.Conn, dest *net.Destination, dialer *finalmask.Dialer) (net.Conn, error) {
	return NewConnClient(c, conn, false)
}

func (c *Config) WrapConnServer(conn net.Conn) (net.Conn, error) {
	return NewConnServer(c, conn, true)
}
