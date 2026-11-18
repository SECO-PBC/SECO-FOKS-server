// Auto-generated to Go types and interfaces using snowpc 0.0.4 (https://github.com/foks-proj/go-snowpack-compiler)
//  Input file:../../proto-src/lib/analytics.snowp

package lib

import (
	"github.com/foks-proj/go-snowpack-rpc/rpc"
)

type ClientLatencyMetric int

const (
	ClientLatencyMetric_SendAck     ClientLatencyMetric = 1
	ClientLatencyMetric_RecvRender  ClientLatencyMetric = 2
	ClientLatencyMetric_DaemonReply ClientLatencyMetric = 3
	ClientLatencyMetric_ColdStart   ClientLatencyMetric = 4
	ClientLatencyMetric_ChannelOpen ClientLatencyMetric = 5
)

var ClientLatencyMetricMap = map[string]ClientLatencyMetric{
	"SendAck":     1,
	"RecvRender":  2,
	"DaemonReply": 3,
	"ColdStart":   4,
	"ChannelOpen": 5,
}
var ClientLatencyMetricRevMap = map[ClientLatencyMetric]string{
	1: "SendAck",
	2: "RecvRender",
	3: "DaemonReply",
	4: "ColdStart",
	5: "ChannelOpen",
}

type ClientLatencyMetricInternal__ ClientLatencyMetric

func (c ClientLatencyMetricInternal__) Import() ClientLatencyMetric {
	return ClientLatencyMetric(c)
}
func (c ClientLatencyMetric) Export() *ClientLatencyMetricInternal__ {
	return ((*ClientLatencyMetricInternal__)(&c))
}

type ClientPlatform int

const (
	ClientPlatform_Ios     ClientPlatform = 1
	ClientPlatform_Android ClientPlatform = 2
	ClientPlatform_Desktop ClientPlatform = 3
)

var ClientPlatformMap = map[string]ClientPlatform{
	"Ios":     1,
	"Android": 2,
	"Desktop": 3,
}
var ClientPlatformRevMap = map[ClientPlatform]string{
	1: "Ios",
	2: "Android",
	3: "Desktop",
}

type ClientPlatformInternal__ ClientPlatform

func (c ClientPlatformInternal__) Import() ClientPlatform {
	return ClientPlatform(c)
}
func (c ClientPlatform) Export() *ClientPlatformInternal__ {
	return ((*ClientPlatformInternal__)(&c))
}

type ClientLatencySample struct {
	Metric ClientLatencyMetric
	Ms     uint64
}
type ClientLatencySampleInternal__ struct {
	_struct struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Metric  *ClientLatencyMetricInternal__
	Ms      *uint64
}

func (c ClientLatencySampleInternal__) Import() ClientLatencySample {
	return ClientLatencySample{
		Metric: (func(x *ClientLatencyMetricInternal__) (ret ClientLatencyMetric) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(c.Metric),
		Ms: (func(x *uint64) (ret uint64) {
			if x == nil {
				return ret
			}
			return *x
		})(c.Ms),
	}
}
func (c ClientLatencySample) Export() *ClientLatencySampleInternal__ {
	return &ClientLatencySampleInternal__{
		Metric: c.Metric.Export(),
		Ms:     &c.Ms,
	}
}
func (c *ClientLatencySample) Encode(enc rpc.Encoder) error {
	return enc.Encode(c.Export())
}

func (c *ClientLatencySample) Decode(dec rpc.Decoder) error {
	var tmp ClientLatencySampleInternal__
	err := dec.Decode(&tmp)
	if err != nil {
		return err
	}
	*c = tmp.Import()
	return nil
}

func (c *ClientLatencySample) Bytes() []byte { return nil }

type ClientAnalyticsBatch struct {
	Platform  ClientPlatform
	Latencies []ClientLatencySample
}
type ClientAnalyticsBatchInternal__ struct {
	_struct   struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Platform  *ClientPlatformInternal__
	Latencies *[](*ClientLatencySampleInternal__)
}

func (c ClientAnalyticsBatchInternal__) Import() ClientAnalyticsBatch {
	return ClientAnalyticsBatch{
		Platform: (func(x *ClientPlatformInternal__) (ret ClientPlatform) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(c.Platform),
		Latencies: (func(x *[](*ClientLatencySampleInternal__)) (ret []ClientLatencySample) {
			if x == nil || len(*x) == 0 {
				return nil
			}
			ret = make([]ClientLatencySample, len(*x))
			for k, v := range *x {
				if v == nil {
					continue
				}
				ret[k] = (func(x *ClientLatencySampleInternal__) (ret ClientLatencySample) {
					if x == nil {
						return ret
					}
					return x.Import()
				})(v)
			}
			return ret
		})(c.Latencies),
	}
}
func (c ClientAnalyticsBatch) Export() *ClientAnalyticsBatchInternal__ {
	return &ClientAnalyticsBatchInternal__{
		Platform: c.Platform.Export(),
		Latencies: (func(x []ClientLatencySample) *[](*ClientLatencySampleInternal__) {
			if len(x) == 0 {
				return nil
			}
			ret := make([](*ClientLatencySampleInternal__), len(x))
			for k, v := range x {
				ret[k] = v.Export()
			}
			return &ret
		})(c.Latencies),
	}
}
func (c *ClientAnalyticsBatch) Encode(enc rpc.Encoder) error {
	return enc.Encode(c.Export())
}

func (c *ClientAnalyticsBatch) Decode(dec rpc.Decoder) error {
	var tmp ClientAnalyticsBatchInternal__
	err := dec.Decode(&tmp)
	if err != nil {
		return err
	}
	*c = tmp.Import()
	return nil
}

func (c *ClientAnalyticsBatch) Bytes() []byte { return nil }
