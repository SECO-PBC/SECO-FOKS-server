// Auto-generated to Go types and interfaces using snowpc 0.0.4 (https://github.com/foks-proj/go-snowpack-compiler)
//  Input file:../../proto-src/lib/api.snowp

package lib

import (
	"github.com/foks-proj/go-snowpack-rpc/rpc"
)

type ApiToken string
type ApiTokenInternal__ string

func (a ApiToken) Export() *ApiTokenInternal__ {
	tmp := ((string)(a))
	return ((*ApiTokenInternal__)(&tmp))
}
func (a ApiTokenInternal__) Import() ApiToken {
	tmp := (string)(a)
	return ApiToken((func(x *string) (ret string) {
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
	return nil
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
	Token    ApiToken
	BaseURL  URLString
	Lifetime DurationMilli
}
type ApiSessionInternal__ struct {
	_struct  struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Token    *ApiTokenInternal__
	BaseURL  *URLStringInternal__
	Lifetime *DurationMilliInternal__
}

func (a ApiSessionInternal__) Import() ApiSession {
	return ApiSession{
		Token: (func(x *ApiTokenInternal__) (ret ApiToken) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.Token),
		BaseURL: (func(x *URLStringInternal__) (ret URLString) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.BaseURL),
		Lifetime: (func(x *DurationMilliInternal__) (ret DurationMilli) {
			if x == nil {
				return ret
			}
			return x.Import()
		})(a.Lifetime),
	}
}
func (a ApiSession) Export() *ApiSessionInternal__ {
	return &ApiSessionInternal__{
		Token:    a.Token.Export(),
		BaseURL:  a.BaseURL.Export(),
		Lifetime: a.Lifetime.Export(),
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
