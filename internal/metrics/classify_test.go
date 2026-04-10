package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyDecisionTree(t *testing.T) {
	tests := []struct {
		name     string
		features SignalFeatures
		meta     MetricMeta
		want     Classification
	}{
		{
			name:     "saturation overrides everything",
			features: SignalFeatures{SaturationDetected: true, DeltaPct: 50, CV: 0.1},
			meta:     MetricMeta{Kind: KindResourceUtilization, BetterDirection: DirectionDown},
			want:     ClassSaturation,
		},
		{
			name:     "spike: high z-score, low ratio, low delta",
			features: SignalFeatures{MaxZScore: 4.0, SpikeRatio: 0.05, DeltaPct: 5},
			meta:     MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want:     ClassSpike,
		},
		{
			name:     "stable: low delta",
			features: SignalFeatures{DeltaPct: 3, CV: 0.1},
			meta:     MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want:     ClassStable,
		},
		{
			name:     "noisy: low delta but high CV",
			features: SignalFeatures{DeltaPct: 3, CV: 0.5},
			meta:     MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want:     ClassNoisy,
		},
		{
			name: "recovery: was high, now trending back down toward baseline",
			features: SignalFeatures{
				DeltaPct:    15,
				BreachRatio: 0.1,
				TrendScore:  -0.05,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown, SLOThreshold: float64Ptr(0.7)},
			want: ClassRecovery,
		},
		{
			name: "continued improvement is stable, not recovery",
			features: SignalFeatures{
				DeltaPct:    -15,
				BreachRatio: 0.1,
				TrendScore:  -0.05,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown, SLOThreshold: float64Ptr(0.7)},
			want: ClassStable,
		},
		{
			name: "recovery requires SLO: without SLOThreshold, falls through to default",
			features: SignalFeatures{
				DeltaPct:    15,
				BreachRatio: 0, // no SLO → BreachRatio always 0
				TrendScore:  -0.05,
				CV:          0.2,
			},
			// Without SLOThreshold the recovery branch must not fire.
			// Delta is significant and degrading → default step_regression fallback.
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassStepRegression,
		},
		{
			name: "step regression: sudden shift",
			features: SignalFeatures{
				DeltaPct:           25,
				StepChangePct:      30,
				StepChangeDetected: true,
				CV:                 0.15,
				BreachRatio:        0.5,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassStepRegression,
		},
		{
			name: "sustained regression: slow degradation",
			features: SignalFeatures{
				DeltaPct:    15,
				BreachRatio: 0.4,
				// Strictly above TrendStrongBand (0.05) so the sustained
				// branch fires rather than the default step fallback.
				TrendScore: 0.08,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassSustainedRegression,
		},
		{
			name: "noisy with significant delta",
			features: SignalFeatures{
				DeltaPct: 20,
				CV:       0.5,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassNoisy,
		},
		{
			name: "default degradation fallback",
			features: SignalFeatures{
				DeltaPct: 20,
				CV:       0.2,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassStepRegression,
		},
		{
			name: "direction none: no degradation possible",
			features: SignalFeatures{
				DeltaPct: 20,
				CV:       0.2,
			},
			meta: MetricMeta{Kind: KindThroughput, BetterDirection: DirectionNone},
			want: ClassStable,
		},
		{
			name: "direction up: value dropping is degradation",
			features: SignalFeatures{
				DeltaPct: -20,
				CV:       0.2,
			},
			meta: MetricMeta{Kind: KindAvailability, BetterDirection: DirectionUp},
			want: ClassStepRegression,
		},
		{
			name: "direction up: value rising is not degradation",
			features: SignalFeatures{
				DeltaPct: 20,
				CV:       0.2,
			},
			meta: MetricMeta{Kind: KindAvailability, BetterDirection: DirectionUp},
			want: ClassStable,
		},
		{
			name: "direction up: sustained regression",
			features: SignalFeatures{
				DeltaPct:    -15,
				BreachRatio: 0.4,
				TrendScore:  -0.08,
			},
			meta: MetricMeta{Kind: KindAvailability, BetterDirection: DirectionUp},
			want: ClassSustainedRegression,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// All decision-tree cases are tested with reliable data: the
			// point of this test is the pure classification logic, not the
			// reliability downgrade pass. Downgrade behavior is covered by
			// TestClassifyReliabilityDowngrade below.
			tt.features.DataQuality.Reliable = true
			tt.features.BaselineReliable = true
			got := Classify(&tt.features, tt.meta)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestClassifyReliabilityDowngrade verifies that classifications that would
// otherwise imply a regression are downgraded when the underlying data
// cannot support them.
func TestClassifyReliabilityDowngrade(t *testing.T) {
	baseRegression := SignalFeatures{
		DeltaPct:           25,
		StepChangePct:      30,
		StepChangeDetected: true,
		CV:                 0.15,
		BreachRatio:        0.5,
		BaselineReliable:   true,
		DataQuality:        DataQuality{Reliable: true},
	}
	meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}

	t.Run("reliable data → step_regression", func(t *testing.T) {
		f := baseRegression
		require.Equal(t, ClassStepRegression, Classify(&f, meta))
	})

	t.Run("unreliable current window → insufficient_data", func(t *testing.T) {
		f := baseRegression
		f.DataQuality.Reliable = false
		require.Equal(t, ClassInsufficientData, Classify(&f, meta))
	})

	t.Run("unreliable baseline → insufficient_data", func(t *testing.T) {
		f := baseRegression
		f.BaselineReliable = false
		require.Equal(t, ClassInsufficientData, Classify(&f, meta))
	})

	t.Run("flapping + unreliable baseline → still flapping", func(t *testing.T) {
		slo := 1.0
		f := SignalFeatures{
			BreachRatio:       0.5,
			BreachTransitions: 20,
			DataQuality:       DataQuality{Reliable: true, ActualPoints: 40},
			BaselineReliable:  false, // baseline is thin, but flapping is current-window only
		}
		flapMeta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown, SLOThreshold: &slo}
		require.Equal(t, ClassFlapping, Classify(&f, flapMeta))
	})

	t.Run("flapping + unreliable current window → insufficient_data", func(t *testing.T) {
		slo := 1.0
		f := SignalFeatures{
			BreachRatio:       0.5,
			BreachTransitions: 20,
			DataQuality:       DataQuality{Reliable: false, ActualPoints: 40}, // gappy current window
			BaselineReliable:  true,
		}
		flapMeta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown, SLOThreshold: &slo}
		require.Equal(t, ClassInsufficientData, Classify(&f, flapMeta))
	})

	t.Run("noisy is not downgraded even with unreliable baseline", func(t *testing.T) {
		f := SignalFeatures{
			DeltaPct:         3,
			CV:               0.5,
			DataQuality:      DataQuality{Reliable: true},
			BaselineReliable: false, // thin baseline
		}
		require.Equal(t, ClassNoisy, Classify(&f, meta))
	})

	t.Run("saturation is never downgraded", func(t *testing.T) {
		f := SignalFeatures{
			SaturationDetected: true,
			BaselineReliable:   false,
			DataQuality:        DataQuality{Reliable: false},
		}
		require.Equal(t, ClassSaturation, Classify(&f, meta))
	})
}

func float64Ptr(v float64) *float64 { return &v }

// TestClassifyImprovement verifies the improvement class fires when delta is
// significant in the favorable direction and trend continues improving.
func TestClassifyImprovement(t *testing.T) {
	t.Run("latency: mean dropped and still trending down → improvement", func(t *testing.T) {
		f := SignalFeatures{
			DeltaPct:         -20, // current is 20% better than baseline
			TrendScore:       -0.10,
			BreachRatio:      0.05,
			CV:               0.1,
			DataQuality:      DataQuality{Reliable: true},
			BaselineReliable: true,
		}
		meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}
		assert.Equal(t, ClassImprovement, Classify(&f, meta))
	})

	t.Run("availability: going up is improvement", func(t *testing.T) {
		f := SignalFeatures{
			DeltaPct:         20,
			TrendScore:       0.10,
			BreachRatio:      0.0,
			CV:               0.1,
			DataQuality:      DataQuality{Reliable: true},
			BaselineReliable: true,
		}
		meta := MetricMeta{Kind: KindAvailability, BetterDirection: DirectionUp}
		assert.Equal(t, ClassImprovement, Classify(&f, meta))
	})

	t.Run("directionless metric: no improvement label", func(t *testing.T) {
		f := SignalFeatures{
			DeltaPct:         -20,
			TrendScore:       -0.10,
			CV:               0.1,
			DataQuality:      DataQuality{Reliable: true},
			BaselineReliable: true,
		}
		meta := MetricMeta{Kind: KindThroughput, BetterDirection: DirectionNone}
		assert.NotEqual(t, ClassImprovement, Classify(&f, meta))
	})
}

// TestClassifyFlapping verifies flapping detection requires an SLO, enough
// points, a high transition rate, and a breach ratio in the mid band.
func TestClassifyFlapping(t *testing.T) {
	slo := 1.0
	meta := MetricMeta{
		Kind:            KindLatency,
		BetterDirection: DirectionDown,
		SLOThreshold:    &slo,
	}

	t.Run("oscillating across threshold → flapping", func(t *testing.T) {
		f := SignalFeatures{
			DeltaPct:          2, // near-zero mean delta — flapping is orthogonal
			BreachRatio:       0.5,
			BreachTransitions: 20, // 20 / 40 = 50% transition rate
			DataQuality:       DataQuality{Reliable: true, ActualPoints: 40},
			BaselineReliable:  true,
		}
		assert.Equal(t, ClassFlapping, Classify(&f, meta))
	})

	t.Run("stuck in breach → not flapping", func(t *testing.T) {
		f := SignalFeatures{
			DeltaPct:          30,
			StepChangePct:     30,
			BreachRatio:       0.9, // above flapping upper band
			BreachTransitions: 1,
			DataQuality:       DataQuality{Reliable: true, ActualPoints: 40},
			BaselineReliable:  true,
		}
		assert.NotEqual(t, ClassFlapping, Classify(&f, meta))
	})

	t.Run("no SLO → flapping impossible", func(t *testing.T) {
		f := SignalFeatures{
			BreachRatio:       0.5,
			BreachTransitions: 20,
			DataQuality:       DataQuality{Reliable: true, ActualPoints: 40},
			BaselineReliable:  true,
		}
		metaNoSLO := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}
		assert.NotEqual(t, ClassFlapping, Classify(&f, metaNoSLO))
	})

	t.Run("low transition rate → not flapping even with mid breach ratio", func(t *testing.T) {
		f := SignalFeatures{
			BreachRatio:       0.5,
			BreachTransitions: 2, // 2 / 40 = 5% — well below 15% threshold
			DataQuality:       DataQuality{Reliable: true, ActualPoints: 40},
			BaselineReliable:  true,
		}
		assert.NotEqual(t, ClassFlapping, Classify(&f, meta))
	})
}

func TestClassificationIsValid(t *testing.T) {
	valid := []Classification{ClassStable, ClassNoisy, ClassSpike, ClassStepRegression, ClassSustainedRegression, ClassRecovery, ClassSaturation, ClassImprovement, ClassFlapping, ClassInsufficientData}
	for _, c := range valid {
		assert.True(t, c.IsValid())
	}
	invalid := []Classification{"", "garbage", "Stable", "STABLE"}
	for _, c := range invalid {
		assert.False(t, c.IsValid())
	}
}

func TestTrendDirectionIsValid(t *testing.T) {
	valid := []TrendDirection{TrendFlat, TrendUp, TrendDown}
	for _, d := range valid {
		assert.True(t, d.IsValid())
	}
	invalid := []TrendDirection{"", "sideways", "Flat", "UP"}
	for _, d := range invalid {
		assert.False(t, d.IsValid())
	}
}

func TestClassificationThresholdsValidate(t *testing.T) {
	tests := []struct {
		name    string
		t       ClassificationThresholds
		wantErr bool
	}{
		{
			name:    "valid thresholds",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: 0.3, CVForNoisy: 0.3, SpikeZScore: 3},
			wantErr: false,
		},
		{
			name:    "zero-value struct is invalid",
			t:       ClassificationThresholds{},
			wantErr: true,
		},
		{
			name:    "negative SignificantDeltaPct",
			t:       ClassificationThresholds{SignificantDeltaPct: -1, BreachRatioForRegress: 0.3, CVForNoisy: 0.3, SpikeZScore: 3},
			wantErr: true,
		},
		{
			name:    "BreachRatioForRegress above 1",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: 1.5, CVForNoisy: 0.3, SpikeZScore: 3},
			wantErr: true,
		},
		{
			name:    "BreachRatioForRegress negative",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: -0.1, CVForNoisy: 0.3, SpikeZScore: 3},
			wantErr: true,
		},
		{
			name:    "zero CVForNoisy",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: 0.3, CVForNoisy: 0, SpikeZScore: 3},
			wantErr: true,
		},
		{
			name:    "zero SpikeZScore",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: 0.3, CVForNoisy: 0.3, SpikeZScore: 0},
			wantErr: true,
		},
		{
			name:    "boundary: BreachRatioForRegress = 0 is valid",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: 0, CVForNoisy: 0.3, SpikeZScore: 3},
			wantErr: false,
		},
		{
			name:    "boundary: BreachRatioForRegress = 1 is valid",
			t:       ClassificationThresholds{SignificantDeltaPct: 10, BreachRatioForRegress: 1, CVForNoisy: 0.3, SpikeZScore: 3},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.t.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestEffectiveThresholds_ZeroValueFallback(t *testing.T) {
	// A MetricMeta with a zero-value thresholds pointer should fall back to defaults.
	zeroThresholds := &ClassificationThresholds{}
	meta := MetricMeta{
		Kind:       KindLatency,
		Thresholds: zeroThresholds,
	}
	got := meta.EffectiveThresholds()
	want := DefaultThresholdsFor(KindLatency)
	assert.Equal(t, want, got)
}

func TestEffectiveThresholds_ValidCustom(t *testing.T) {
	custom := &ClassificationThresholds{SignificantDeltaPct: 5, BreachRatioForRegress: 0.2, CVForNoisy: 0.5, SpikeZScore: 2.5}
	meta := MetricMeta{
		Kind:       KindLatency,
		Thresholds: custom,
	}
	got := meta.EffectiveThresholds()
	assert.Equal(t, *custom, got)
}
