package dns

import (
	"github.com/readeem/mixray/internal/xray-core/common/net"
	"github.com/readeem/mixray/internal/xray-core/features"
)

type FakeDNSEngine interface {
	features.Feature
	GetFakeIPForDomain(domain string) []net.Address
	GetDomainFromFakeDNS(ip net.Address) string
}

var (
	FakeIPv4Pool = "198.18.0.0/15"
	FakeIPv6Pool = "2001:2::/48"
)

type FakeDNSEngineRev0 interface {
	FakeDNSEngine
	IsIPInIPPool(ip net.Address) bool
	GetFakeIPForDomain3(domain string, IPv4, IPv6 bool) []net.Address
}
