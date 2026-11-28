// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package rem

import (
	"reflect"
	"testing"
)

// TestSyncLoadGenericChainArgFields: the batched form of a loadGenericChain
// call carries exactly the call's fields, so a field added to one and not
// the other fails here rather than going missing from batched calls.
func TestSyncLoadGenericChainArgFields(t *testing.T) {
	fields := func(v any) map[string]reflect.Type {
		ret := make(map[string]reflect.Type)
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			ret[typ.Field(i).Name] = typ.Field(i).Type
		}
		return ret
	}
	if a, b := fields(LoadGenericChainArg{}), fields(SyncLoadGenericChainArg{}); !reflect.DeepEqual(a, b) {
		t.Fatalf("fields differ: %v vs %v", a, b)
	}
}
