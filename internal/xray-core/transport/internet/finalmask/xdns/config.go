package xdns

import (
	"github.com/readeem/mixray/internal/xray-core/common/net"
	"github.com/readeem/mixray/internal/xray-core/transport/internet/finalmask"
)

func (c *Config) WrapPacketConnClient(conn net.PacketConn, dest *net.Destination, dialer *finalmask.Dialer) (net.PacketConn, error) {
	return NewClient(c, dialer)
}

func (c *Config) WrapPacketConnServer(conn net.PacketConn, addr net.Addr, lc *finalmask.ListenConfig) (net.PacketConn, error) {
	return NewServer(c, conn)
}
