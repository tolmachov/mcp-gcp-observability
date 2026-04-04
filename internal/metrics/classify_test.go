package metrics

import (
	"testing"
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
				DeltaPct:   15,
				BreachRatio: 0.1,
				TrendScore:  -0.05,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassRecovery,
		},
		{
			name: "continued improvement is stable, not recovery",
			features: SignalFeatures{
				DeltaPct:   -15,
				BreachRatio: 0.1,
				TrendScore:  -0.05,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassStable,
		},
		{
			name: "step regression: sudden shift",
			features: SignalFeatures{
				DeltaPct:       25,
				StepChangePct:  30,
				CV:             0.15,
				BreachRatio:    0.5,
			},
			meta: MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			want: ClassStepRegression,
		},
		{
			name: "sustained regression: slow degradation",
			features: SignalFeatures{
				DeltaPct:    15,
				BreachRatio: 0.4,
				TrendScore:  0.05,
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
				TrendScore:  -0.05,
			},
			meta: MetricMeta{Kind: KindAvailability, BetterDirection: DirectionUp},
			want: ClassSustainedRegression,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(&tt.features, tt.meta)
			if got != tt.want {
				t.Errorf("Classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassificationIsValid(t *testing.T) {
	valid := []Classification{ClassStable, ClassNoisy, ClassSpike, ClassStepRegression, ClassSustainedRegression, ClassRecovery, ClassSaturation}
	for _, c := range valid {
		if !c.IsValid() {
			t.Errorf("Classification(%q).IsValid() = false, want true", c)
		}
	}
	invalid := []Classification{"", "garbage", "Stable", "STABLE"}
	for _, c := range invalid {
		if c.IsValid() {
			t.Errorf("Classification(%q).IsValid() = true, want false", c)
		}
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
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
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
	if got != want {
		t.Errorf("EffectiveThresholds() with zero-value = %+v, want defaults %+v", got, want)
	}
}

func TestEffectiveThresholds_ValidCustom(t *testing.T) {
	custom := &ClassificationThresholds{SignificantDeltaPct: 5, BreachRatioForRegress: 0.2, CVForNoisy: 0.5, SpikeZScore: 2.5}
	meta := MetricMeta{
		Kind:       KindLatency,
		Thresholds: custom,
	}
	got := meta.EffectiveThresholds()
	if got != *custom {
		t.Errorf("EffectiveThresholds() = %+v, want custom %+v", got, *custom)
	}
}
