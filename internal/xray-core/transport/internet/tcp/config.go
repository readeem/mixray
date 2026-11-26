package tcp

import (
	"github.com/readeem/mixray/internal/xray-core/common"
	"github.com/readeem/mixray/internal/xray-core/transport/internet"
)

func init() {
	common.Must(internet.RegisterProtocolConfigCreator(protocolName, func() interface{} {
		return new(Config)
	}))
}
