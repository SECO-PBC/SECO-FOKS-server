// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// methodSampler picks the sampling ratio for a root span by its name. Root
// spans are RPCs, named "<Protocol>.<method>" (see InstrumentProtocol), so
// this gives per-method trace sampling. Wrap it in ParentBased so a trace's
// child spans follow its root's decision. Unsampled spans are non-recording,
// so childSpan and the pgx tracer skip their children entirely.
type methodSampler struct {
	def       sdktrace.Sampler
	overrides map[string]sdktrace.Sampler
}

var rpcMethodNameRE = regexp.MustCompile(`^[A-Za-z0-9_]+\.[A-Za-z0-9_]+$`)

// newMethodSampler builds a sampler with ratio def for every root span, except
// those named in overrides. Entries that aren't "<Protocol>.<method>" or whose
// ratio is outside [0,1] are skipped and returned as problems for the caller
// to log; a typo shouldn't take telemetry down.
func newMethodSampler(def float64, overrides map[string]float64) (methodSampler, []string) {
	s := methodSampler{
		def:       sdktrace.TraceIDRatioBased(def),
		overrides: make(map[string]sdktrace.Sampler, len(overrides)),
	}
	var problems []string
	for name, r := range overrides {
		switch {
		case !rpcMethodNameRE.MatchString(name):
			problems = append(problems, fmt.Sprintf("%q: want <Protocol>.<method>", name))
		case r < 0 || r > 1:
			problems = append(problems, fmt.Sprintf("%q: ratio %v not in [0,1]", name, r))
		default:
			s.overrides[name] = sdktrace.TraceIDRatioBased(r)
		}
	}
	sort.Strings(problems)
	return s, problems
}

func (s methodSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if o, ok := s.overrides[p.Name]; ok {
		return o.ShouldSample(p)
	}
	return s.def.ShouldSample(p)
}

func (s methodSampler) Description() string {
	names := make([]string, 0, len(s.overrides))
	for n, o := range s.overrides {
		names = append(names, n+"="+o.Description())
	}
	sort.Strings(names)
	return fmt.Sprintf("FoksMethodSampler{default=%s,overrides=[%s]}", s.def.Description(), strings.Join(names, ","))
}
