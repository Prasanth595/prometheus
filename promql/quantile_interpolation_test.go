// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promql_test

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/promql/promqltest"
	storage2 "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/teststorage"
)

func TestQuantileInterpolationPreservesExactValues(t *testing.T) {
	smallestSubnormal := math.SmallestNonzeroFloat64
	largestSubnormal := math.Nextafter(math.Ldexp(1, -1022), 0)
	smallestNormal := math.Ldexp(1, -1022)
	negativeZero := math.Copysign(0, -1)
	maxFloat := math.MaxFloat64
	previousMaxFloat := math.Nextafter(maxFloat, 0)

	testCases := []struct {
		name     string
		values   []float64
		want     float64
		constant bool
	}{
		{name: "smallest positive subnormal", values: []float64{smallestSubnormal, smallestSubnormal}, want: smallestSubnormal, constant: true},
		{name: "three positive subnormal units", values: []float64{math.Float64frombits(3), math.Float64frombits(3)}, want: math.Float64frombits(3), constant: true},
		{name: "smallest negative subnormal", values: []float64{-smallestSubnormal, -smallestSubnormal}, want: -smallestSubnormal, constant: true},
		{name: "three negative subnormal units", values: []float64{-math.Float64frombits(3), -math.Float64frombits(3)}, want: -math.Float64frombits(3), constant: true},
		{name: "largest positive subnormal", values: []float64{largestSubnormal, largestSubnormal}, want: largestSubnormal, constant: true},
		{name: "largest negative subnormal", values: []float64{-largestSubnormal, -largestSubnormal}, want: -largestSubnormal, constant: true},
		{name: "smallest positive normal", values: []float64{smallestNormal, smallestNormal}, want: smallestNormal, constant: true},
		{name: "smallest negative normal", values: []float64{-smallestNormal, -smallestNormal}, want: -smallestNormal, constant: true},
		{name: "positive zero", values: []float64{0, 0}, want: 0, constant: true},
		{name: "negative zero", values: []float64{negativeZero, negativeZero}, want: negativeZero, constant: true},
		{name: "mixed signed zero", values: []float64{negativeZero, 0}, want: 0},
		{name: "equal positive infinity", values: []float64{math.Inf(1), math.Inf(1)}, want: math.Inf(1)},
		{name: "equal negative infinity", values: []float64{math.Inf(-1), math.Inf(-1)}, want: math.Inf(-1)},
		{name: "mixed infinities", values: []float64{math.Inf(-1), math.Inf(1)}, want: math.NaN()},
		{name: "equal NaNs", values: []float64{math.NaN(), math.NaN()}, want: math.NaN()},
		{name: "NaN and finite value", values: []float64{math.NaN(), 1}, want: math.NaN()},
		{name: "opposite finite extremes", values: []float64{-maxFloat, maxFloat}, want: 0},
		{name: "adjacent finite values near maximum", values: []float64{previousMaxFloat, maxFloat}, want: previousMaxFloat},
	}

	stor := teststorage.New(t)
	app := stor.AppenderV2(t.Context())
	for _, tc := range testCases {
		scope := "controls"
		if tc.constant {
			scope = "constants"
		}
		rangeLabels := labels.FromStrings("__name__", "quantile_range", "case", tc.name, "scope", scope)
		for i, value := range tc.values {
			_, err := app.Append(0, rangeLabels, 0, int64(i*1000), value, nil, nil, storage2.AppendV2Options{})
			require.NoError(t, err)

			aggregationLabels := labels.FromStrings("__name__", "quantile_aggregation", "case", tc.name, "scope", scope, "replica", strconv.Itoa(i))
			_, err = app.Append(0, aggregationLabels, 0, 1000, value, nil, nil, storage2.AppendV2Options{})
			require.NoError(t, err)
		}
	}
	require.NoError(t, app.Commit())

	engine := promqltest.NewTestEngine(t, false, 0, promqltest.DefaultMaxSamplesPerQuery)
	checkQuery := func(expr string, expected map[string]float64, warning bool) {
		t.Helper()
		query, err := engine.NewInstantQuery(t.Context(), stor, nil, expr, timestamp.Time(1000))
		require.NoError(t, err)
		defer query.Close()

		result := query.Exec(t.Context())
		require.NoError(t, result.Err)
		if warning {
			require.NotEmpty(t, result.Warnings)
		} else {
			require.Empty(t, result.Warnings)
		}
		vector, err := result.Vector()
		require.NoError(t, err)
		require.Len(t, vector, len(expected))
		seen := make(map[string]struct{}, len(expected))
		for _, sample := range vector {
			name := sample.Metric.Get("case")
			want, ok := expected[name]
			require.True(t, ok, "unexpected result for case %q", name)
			_, duplicate := seen[name]
			require.False(t, duplicate, "duplicate result for case %q", name)
			seen[name] = struct{}{}
			if math.IsNaN(want) {
				require.True(t, math.IsNaN(sample.F), "case %q: expected NaN, got %v", name, sample.F)
				continue
			}
			require.Equal(t, math.Float64bits(want), math.Float64bits(sample.F), "case %q", name)
		}
		require.Len(t, seen, len(expected))
	}

	constantValues := make(map[string]float64)
	allValues := make(map[string]float64)
	for _, tc := range testCases {
		allValues[tc.name] = tc.want
		if tc.constant {
			constantValues[tc.name] = tc.want
		}
	}
	for _, q := range []string{"0.25", "0.5", "0.75"} {
		checkQuery("quantile_over_time("+q+", quantile_range{scope=\"constants\"}[2s])", constantValues, false)
		checkQuery("quantile by (case) ("+q+", quantile_aggregation{scope=\"constants\"})", constantValues, false)
	}
	checkQuery("quantile_over_time(0.5, quantile_range[2s])", allValues, false)
	checkQuery("quantile by (case) (0.5, quantile_aggregation)", allValues, false)

	for _, tc := range []struct {
		q    string
		want float64
	}{
		{q: "NaN", want: math.NaN()},
		{q: "-1", want: math.Inf(-1)},
		{q: "2", want: math.Inf(1)},
	} {
		expected := map[string]float64{"smallest positive subnormal": tc.want}
		checkQuery("quantile_over_time("+tc.q+", quantile_range{case=\"smallest positive subnormal\"}[2s])", expected, true)
		checkQuery("quantile by (case) ("+tc.q+", quantile_aggregation{case=\"smallest positive subnormal\"})", expected, true)
	}
}

func TestMADOverTimeUsesExactSubnormalMedian(t *testing.T) {
	stor := teststorage.New(t)
	app := stor.AppenderV2(t.Context())
	metric := labels.FromStrings("__name__", "metric")
	for i, value := range []float64{0, math.Float64frombits(3), math.Float64frombits(3), math.Float64frombits(6)} {
		_, err := app.Append(0, metric, 0, int64(i*1000), value, nil, nil, storage2.AppendV2Options{})
		require.NoError(t, err)
	}
	require.NoError(t, app.Commit())

	engine := promqltest.NewTestEngine(t, false, 0, promqltest.DefaultMaxSamplesPerQuery)
	query, err := engine.NewInstantQuery(t.Context(), stor, nil, "mad_over_time(metric[4s])", timestamp.Time(3000))
	require.NoError(t, err)
	defer query.Close()

	result := query.Exec(t.Context())
	require.NoError(t, result.Err)
	require.Empty(t, result.Warnings)
	vector, err := result.Vector()
	require.NoError(t, err)
	require.Len(t, vector, 1)
	require.Equal(t, math.Float64bits(math.Float64frombits(2)), math.Float64bits(vector[0].F))
}
