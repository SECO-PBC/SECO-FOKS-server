// Auto-generated to Go types and interfaces using snowpc 0.0.4 (https://github.com/foks-proj/go-snowpack-compiler)
//  Input file:../../proto-src/lib/api.snowp

package lib

import (
	"github.com/foks-proj/go-snowpack-rpc/rpc"
)

type ApiToken [32]byte
type ApiTokenInternal__ [32]byte

func (a ApiToken) Export() *ApiTokenInternal__ {
	tmp := (([32]byte)(a))
	return ((*ApiTokenInternal__)(&tmp))
}
func (a ApiTokenInternal__) Import() ApiToken {
	tmp := ([32]byte)(a)
	return ApiToken((func(x *[32]byte) (ret [32]byte) {
		if x == nil {
			return ret
		}
		return *x
	})(&tmp))
}

func (a *ApiToken) Encode(enc rpc.Encoder) error {
	return enc.Encode(a.Export())
}

func (a *ApiToken) Decode(dec rpc.Decoder) error {
	var tmp ApiTokenInternal__
	err := dec.Decode(&tmp)
	if err != nil {
		return err
	}
	*a = tmp.Import()
	return nil
}

func (a ApiToken) Bytes() []byte {
	return (a)[:]
}

type ApiScope int

const (
	ApiScope_Ping            ApiScope = 0
	ApiScope_ClientAnalytics ApiScope = 1
)

var ApiScopeMap = map[string]ApiScope{
	"Ping":            0,
	"ClientAnalytics": 1,
}
var ApiScopeRevMap = map[ApiScope]string{
	0: "Ping",
	1: "ClientAnalytics",
}

type ApiScopeInternal__ ApiScope

func (a ApiScopeInternal__) Import() ApiScope {
	return ApiScope(a)
}
func (a ApiScope) Export() *ApiScopeInternal__ {
	return ((*ApiScopeInternal__)(&a))
}

type ApiSession struct {
	Token   ApiToken
	Scope   ApiScope
	BaseURL URLString
	Expires Time
}
type ApiSessionInternal__ struct {
	_struct struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Token   *ApiTokenInternal__
	Scope   *ApiScopeInternal__
	BaseURL *URLStringInternal__
	Expires *TimeInternal__
}

func (a ApiSessionInternal__) Import() ApiSession {
	return ApiSession{
		Token: (func(x *ApiTokenInternal__) (ret ApiToken) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.Token),
		Scope: (func(x *ApiScopeInternal__) (ret ApiScope) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.Scope),
		BaseURL: (func(x *URLStringInternal__) (ret URLString) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.BaseURL),
		Expires: (func(x *TimeInternal__) (ret Time) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.Expires),
	}
}
func (a ApiSession) Export() *ApiSessionInternal__ {
	return &ApiSessionInternal__{
		Token:   a.Token.Export(),
		Scope:   a.Scope.Export(),
		BaseURL: a.BaseURL.Export(),
		Expires: a.Expires.Export(),
	}
}
func (a *ApiSession) Encode(enc rpc.Encoder) error {
	return enc.Encode(a.Export())
}

func (a *ApiSession) Decode(dec rpc.Decoder) error {
	var tmp ApiSessionInternal__
	err := dec.Decode(&tmp)
	if err != nil {
		return err
	}
	*a = tmp.Import()
	return nil
}

func (a *ApiSession) Bytes() []byte { return nil }
