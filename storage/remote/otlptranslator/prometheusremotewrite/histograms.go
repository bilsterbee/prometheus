// Copyright 2024 The Prometheus Authors
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
// Provenance-includes-location: https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/95e8f8fdc2a9dc87230406c9a3cf02be4fd68bea/pkg/translator/prometheusremotewrite/histograms.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: Copyright The OpenTelemetry Authors.

package prometheusremotewrite

import (
	"context"
	"fmt"
	"math"

	"github.com/prometheus/common/model"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/util/annotations"
)

const defaultZeroThreshold = 1e-128

// addExponentialHistogramDataPoints adds OTel exponential histogram data points to the corresponding time series
// as native histogram samples.
func (c *PrometheusConverter) addExponentialHistogramDataPoints(ctx context.Context, dataPoints pmetric.ExponentialHistogramDataPointSlice,
	resource pcommon.Resource, settings Settings, promName string) (annotations.Annotations, error) {
	var annots annotations.Annotations
	for x := 0; x < dataPoints.Len(); x++ {
		if err := c.everyN.checkContext(ctx); err != nil {
			return annots, err
		}

		pt := dataPoints.At(x)

		histogram, ws, err := exponentialToNativeHistogram(pt)
		annots.Merge(ws)
		if err != nil {
			return annots, err
		}

		lbls := createAttributes(
			resource,
			pt.Attributes(),
			settings,
			nil,
			true,
			model.MetricNameLabel,
			promName,
		)
		ts, _ := c.getOrCreateTimeSeries(lbls)
		ts.Histograms = append(ts.Histograms, histogram)

		exemplars, err := getPromExemplars[pmetric.ExponentialHistogramDataPoint](ctx, &c.everyN, pt)
		if err != nil {
			return annots, err
		}
		ts.Exemplars = append(ts.Exemplars, exemplars...)
	}

	return annots, nil
}

// exponentialToNativeHistogram translates an OTel Exponential Histogram data point
// to a Prometheus Native Histogram.
func exponentialToNativeHistogram(p pmetric.ExponentialHistogramDataPoint) (prompb.Histogram, annotations.Annotations, error) {
	var annots annotations.Annotations
	scale := p.Scale()
	if scale < -4 {
		return prompb.Histogram{}, annots,
			fmt.Errorf("cannot convert exponential to native histogram."+
				" Scale must be >= -4, was %d", scale)
	}

	var scaleDown int32
	if scale > 8 {
		scaleDown = scale - 8
		scale = 8
	}

	pSpans, pDeltas := convertBucketsLayout(p.Positive(), scaleDown)
	nSpans, nDeltas := convertBucketsLayout(p.Negative(), scaleDown)

	h := prompb.Histogram{
		// The counter reset detection must be compatible with Prometheus to
		// safely set ResetHint to NO. This is not ensured currently.
		// Sending a sample that triggers counter reset but with ResetHint==NO
		// would lead to Prometheus panic as it does not double check the hint.
		// Thus we're explicitly saying UNKNOWN here, which is always safe.
		// TODO: using created time stamp should be accurate, but we
		// need to know here if it was used for the detection.
		// Ref: https://github.com/open-telemetry/opentelemetry-collector-contrib/pull/28663#issuecomment-1810577303
		// Counter reset detection in Prometheus: https://github.com/prometheus/prometheus/blob/f997c72f294c0f18ca13fa06d51889af04135195/tsdb/chunkenc/histogram.go#L232
		ResetHint: prompb.Histogram_UNKNOWN,
		Schema:    scale,

		ZeroCount: &prompb.Histogram_ZeroCountInt{ZeroCountInt: p.ZeroCount()},
		// TODO use zero_threshold, if set, see
		// https://github.com/open-telemetry/opentelemetry-proto/pull/441
		ZeroThreshold: defaultZeroThreshold,

		PositiveSpans:  pSpans,
		PositiveDeltas: pDeltas,
		NegativeSpans:  nSpans,
		NegativeDeltas: nDeltas,

		Timestamp: convertTimeStamp(p.Timestamp()),
	}

	if p.Flags().NoRecordedValue() {
		h.Sum = math.Float64frombits(value.StaleNaN)
		h.Count = &prompb.Histogram_CountInt{CountInt: value.StaleNaN}
	} else {
		if p.HasSum() {
			h.Sum = p.Sum()
		}
		h.Count = &prompb.Histogram_CountInt{CountInt: p.Count()}
		if p.Count() == 0 && h.Sum != 0 {
			annots.Add(fmt.Errorf("exponential histogram data point has zero count, but non-zero sum: %f", h.Sum))
		}
	}
	return h, annots, nil
}

// convertBucketsLayout translates OTel Exponential Histogram dense buckets
// representation to Prometheus Native Histogram sparse bucket representation.
//
// The translation logic is taken from the client_golang `histogram.go#makeBuckets`
// function, see `makeBuckets` https://github.com/prometheus/client_golang/blob/main/prometheus/histogram.go
// The bucket indexes conversion was adjusted, since OTel exp. histogram bucket
// index 0 corresponds to the range (1, base] while Prometheus bucket index 0
// to the range (base 1].
//
// scaleDown is the factor by which the buckets are scaled down. In other words 2^scaleDown buckets will be merged into one.
func convertBucketsLayout(buckets pmetric.ExponentialHistogramDataPointBuckets, scaleDown int32) ([]prompb.BucketSpan, []int64) {
	bucketCounts := buckets.BucketCounts()
	if bucketCounts.Len() == 0 {
		return nil, nil
	}

	var (
		spans     []prompb.BucketSpan
		deltas    []int64
		count     int64
		prevCount int64
	)

	appendDelta := func(count int64) {
		spans[len(spans)-1].Length++
		deltas = append(deltas, count-prevCount)
		prevCount = count
	}

	// Let the compiler figure out that this is const during this function by
	// moving it into a local variable.
	numBuckets := bucketCounts.Len()

	// The offset is scaled and adjusted by 1 as described above.
	bucketIdx := buckets.Offset()>>scaleDown + 1

	spans = append(spans, prompb.BucketSpan{
		Offset: bucketIdx,
		Length: 0,
	})

	for i := 0; i < numBuckets; i++ {
		// The offset is scaled and adjusted by 1 as described above.
		nextBucketIdx := (int32(i)+buckets.Offset())>>scaleDown + 1
		if bucketIdx == nextBucketIdx { // We have not collected enough buckets to merge yet.
			count += int64(bucketCounts.At(i))
			continue
		}
		if count == 0 {
			count = int64(bucketCounts.At(i))
			continue
		}

		gap := nextBucketIdx - bucketIdx - 1
		if gap > 2 {
			// We have to create a new span, because we have found a gap
			// of more than two buckets. The constant 2 is copied from the logic in
			// https://github.com/prometheus/client_golang/blob/27f0506d6ebbb117b6b697d0552ee5be2502c5f2/prometheus/histogram.go#L1296
			spans = append(spans, prompb.BucketSpan{
				Offset: gap,
				Length: 0,
			})
		} else {
			// We have found a small gap (or no gap at all).
			// Insert empty buckets as needed.
			for j := int32(0); j < gap; j++ {
				appendDelta(0)
			}
		}
		appendDelta(count)
		count = int64(bucketCounts.At(i))
		bucketIdx = nextBucketIdx
	}
	// Need to use the last item's index. The offset is scaled and adjusted by 1 as described above.
	gap := (int32(numBuckets)+buckets.Offset()-1)>>scaleDown + 1 - bucketIdx
	if gap > 2 {
		// We have to create a new span, because we have found a gap
		// of more than two buckets. The constant 2 is copied from the logic in
		// https://github.com/prometheus/client_golang/blob/27f0506d6ebbb117b6b697d0552ee5be2502c5f2/prometheus/histogram.go#L1296
		spans = append(spans, prompb.BucketSpan{
			Offset: gap,
			Length: 0,
		})
	} else {
		// We have found a small gap (or no gap at all).
		// Insert empty buckets as needed.
		for j := int32(0); j < gap; j++ {
			appendDelta(0)
		}
	}
	appendDelta(count)

	return spans, deltas
}

func (c *PrometheusConverter) addCustomBucketsHistogramDataPoints(ctx context.Context, dataPoints pmetric.HistogramDataPointSlice,
	resource pcommon.Resource, settings Settings, promName string) (annotations.Annotations, error) {
	var annots annotations.Annotations

	for x := 0; x < dataPoints.Len(); x++ {
		if err := c.everyN.checkContext(ctx); err != nil {
			return annots, err
		}

		pt := dataPoints.At(x)

		histogram, ws, err := histogramToCustomBucketsHistogram(pt)
		annots.Merge(ws)
		if err != nil {
			return annots, err
		}

		lbls := createAttributes(
			resource,
			pt.Attributes(),
			settings,
			nil,
			true,
			model.MetricNameLabel,
			promName,
		)

		ts, _ := c.getOrCreateTimeSeries(lbls)
		ts.Histograms = append(ts.Histograms, histogram)

		exemplars, err := getPromExemplars[pmetric.HistogramDataPoint](ctx, &c.everyN, pt)
		if err != nil {
			return annots, err
		}
		ts.Exemplars = append(ts.Exemplars, exemplars...)
	}

	return annots, nil
}

func histogramToCustomBucketsHistogram(p pmetric.HistogramDataPoint) (prompb.Histogram, annotations.Annotations, error) {
	var annots annotations.Annotations

	positiveSpans, positiveDeltas := convertHistogramBucketsToNHCBLayout(p.BucketCounts().AsRaw())

	h := prompb.Histogram{
		// The counter reset detection must be compatible with Prometheus to
		// safely set ResetHint to NO. This is not ensured currently.
		// Sending a sample that triggers counter reset but with ResetHint==NO
		// would lead to Prometheus panic as it does not double check the hint.
		// Thus we're explicitly saying UNKNOWN here, which is always safe.
		// TODO: using created time stamp should be accurate, but we
		// need to know here if it was used for the detection.
		// Ref: https://github.com/open-telemetry/opentelemetry-collector-contrib/pull/28663#issuecomment-1810577303
		// Counter reset detection in Prometheus: https://github.com/prometheus/prometheus/blob/f997c72f294c0f18ca13fa06d51889af04135195/tsdb/chunkenc/histogram.go#L232
		ResetHint: prompb.Histogram_UNKNOWN,
		Schema:    -53,

		PositiveSpans:  positiveSpans,
		PositiveDeltas: positiveDeltas,
		CustomValues:   p.ExplicitBounds().AsRaw(),

		Timestamp: convertTimeStamp(p.Timestamp()),
	}

	if p.Flags().NoRecordedValue() {
		h.Sum = math.Float64frombits(value.StaleNaN)
		h.Count = &prompb.Histogram_CountInt{CountInt: value.StaleNaN}
	} else {
		if p.HasSum() {
			h.Sum = p.Sum()
		}
		h.Count = &prompb.Histogram_CountInt{CountInt: p.Count()}
		if p.Count() == 0 && h.Sum != 0 {
			annots.Add(fmt.Errorf("histogram data point has zero count, but non-zero sum: %f", h.Sum))
		}
	}
	return h, annots, nil

}

func convertHistogramBucketsToNHCBLayout(buckets []uint64) ([]prompb.BucketSpan, []int64) {
	if len(buckets) == 0 {
		return nil, nil
	}

	var (
		spans     []prompb.BucketSpan
		deltas    []int64
		count     int64
		prevCount int64
	)

	appendDelta := func(count int64) {
		spans[len(spans)-1].Length++
		deltas = append(deltas, count-prevCount)
		prevCount = count
	}

	var offset int
	for offset < len(buckets) && buckets[offset] == 0 {
		offset++
	}
	bucketCounts := buckets[offset:]

	bucketIdx := offset + 1
	spans = append(spans, prompb.BucketSpan{
		Offset: int32(offset),
		Length: 0,
	})

	for i := 0; i < len(bucketCounts); i++ {
		nextBucketIdx := offset + i + 1
		if bucketIdx == nextBucketIdx { // We have not collected enough buckets to merge yet.
			count += int64(bucketCounts[i])
			continue
		}
		if count == 0 {
			count = int64(bucketCounts[i])
			continue
		}

		iDelta := nextBucketIdx - bucketIdx - 1

		if iDelta > 2 {
			spans = append(spans, prompb.BucketSpan{
				Offset: int32(iDelta),
				Length: 0,
			})
		} else {
			// Either no gap or a gap <= 2
			for j := int32(0); j < int32(iDelta); j++ {
				appendDelta(0)
			}
		}
		appendDelta(count)
		count = int64(bucketCounts[i])
		bucketIdx = nextBucketIdx
	}

	iDelta := int32(len(bucketCounts)+offset-1) + 1 - int32(bucketIdx)
	if iDelta > 2 {
		spans = append(spans, prompb.BucketSpan{
			Offset: iDelta,
			Length: 0,
		})
	} else {
		for j := int32(0); j < iDelta; j++ {
			appendDelta(0)
		}
	}
	appendDelta(count)

	return spans, deltas
}
