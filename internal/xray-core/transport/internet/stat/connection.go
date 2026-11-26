package stat

import (
	"net"

	"github.com/readeem/mixray/internal/xray-core/features/stats"
)

type Connection interface {
	net.Conn
}

type CounterConnection struct {
	Connection
	ReadCounter  stats.Counter
	WriteCounter stats.Counter
}

func (c *CounterConnection) Read(b []byte) (int, error) {
	nBytes, err := c.Connection.Read(b)
	if c.ReadCounter != nil {
		c.ReadCounter.Add(int64(nBytes))
	}

	return nBytes, err
}

func (c *CounterConnection) Write(b []byte) (int, error) {
	nBytes, err := c.Connection.Write(b)
	if c.WriteCounter != nil {
		c.WriteCounter.Add(int64(nBytes))
	}
	return nBytes, err
}

func TryUnwrapStatsConn(conn net.Conn) net.Conn {
	if conn == nil {
		return conn
	}
	if conn, ok := conn.(*CounterConnection); ok {
		return conn.Connection
	}
	return conn
}

func CloseWrite(conn net.Conn) error {
	if c, ok := TryUnwrapStatsConn(conn).(interface{ CloseWrite() error }); ok {
		return c.CloseWrite()
	}
	return nil
}
