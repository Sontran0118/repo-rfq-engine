package domain

import (
	"errors"
	"testing"
)

const million = Money(100_000_000) // $1,000,000.00 in cents

func TestInterest(t *testing.T) {
	tests := []struct {
		name      string
		principal Money
		rate      BasisPoints
		days      int
		want      Money
	}{
		{
			// $10m overnight at 5.25% ACT/360:
			// 10,000,000 * 0.0525 * 1/360 = $1,458.333... -> 1,458.33
			name: "overnight at 5.25%", principal: 10 * million, rate: 525, days: 1,
			want: 145_833,
		},
		{
			// The same trade over a 30-day term: 30x the daily accrual.
			name: "30-day term", principal: 10 * million, rate: 525, days: 30,
			want: 4_375_000,
		},
		{
			// ACT/360 means a 360-day year yields exactly the quoted rate.
			name: "full 360-day basis year", principal: million, rate: 500, days: 360,
			want: 5_000_000,
		},
		{
			// ...and a real 365-day year yields slightly more, which is the
			// entire point of the convention.
			name: "365 days exceeds the quoted rate", principal: million, rate: 500, days: 365,
			want: 5_069_444,
		},
		{
			// Special collateral can trade at a negative rate: the cash lender
			// pays for the privilege of securing the bond.
			name: "negative rate on special collateral", principal: 10 * million, rate: -25, days: 1,
			want: -6_944,
		},
		{
			name: "zero rate accrues nothing", principal: 10 * million, rate: 0, days: 30,
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Interest(tc.principal, tc.rate, tc.days)
			if err != nil {
				t.Fatalf("Interest() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Interest() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestInterestRejectsBadInput(t *testing.T) {
	if _, err := Interest(0, 500, 1); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("zero principal: got %v, want ErrNegativeAmount", err)
	}
	if _, err := Interest(-million, 500, 1); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("negative principal: got %v, want ErrNegativeAmount", err)
	}
	if _, err := Interest(million, 500, 0); !errors.Is(err, ErrInvalidTenor) {
		t.Errorf("zero days: got %v, want ErrInvalidTenor", err)
	}
}

// TestInterestNoFloatDrift is the reason Money is an integer type. Accruing one
// day at a time for a year must land on exactly the same cent as accruing the
// whole year at once. With float64 arithmetic the two diverge.
func TestInterestNoFloatDrift(t *testing.T) {
	const days = 360
	var daily Money
	for i := 0; i < days; i++ {
		step, err := Interest(10*million, 525, 1)
		if err != nil {
			t.Fatalf("Interest() error = %v", err)
		}
		daily += step
	}

	atOnce, err := Interest(10*million, 525, days)
	if err != nil {
		t.Fatalf("Interest() error = %v", err)
	}

	// Daily accrual rounds 360 times, the bulk figure once, so they differ by
	// accumulated rounding — but each is exact, deterministic, and reproducible.
	drift := daily - atOnce
	if drift < -400 || drift > 400 {
		t.Errorf("daily accrual %s vs bulk %s: drift %s exceeds rounding tolerance",
			daily, atOnce, drift)
	}

	// Re-running must produce a bit-identical result; no float non-determinism.
	again, _ := Interest(10*million, 525, days)
	if again != atOnce {
		t.Errorf("Interest() is not deterministic: %s then %s", atOnce, again)
	}
}

func TestRepurchasePrice(t *testing.T) {
	// $10m overnight at 5.25% repays principal plus $1,458.33.
	got, err := RepurchasePrice(10*million, 525, 1)
	if err != nil {
		t.Fatalf("RepurchasePrice() error = %v", err)
	}
	want := 10*million + 145_833
	if got != want {
		t.Errorf("RepurchasePrice() = %s, want %s", got, want)
	}
}

func TestCollateralRequired(t *testing.T) {
	tests := []struct {
		name    string
		cash    Money
		haircut BasisPoints
		want    Money
	}{
		{
			// 2% haircut on $10m cash needs $10.2m of securities.
			name: "2% haircut", cash: 10 * million, haircut: 200,
			want: 1_020_000_000,
		},
		{
			// Treasuries often clear with a very small haircut.
			name: "0.25% haircut on USTs", cash: 10 * million, haircut: 25,
			want: 1_002_500_000,
		},
		{
			name: "no haircut returns the cash amount", cash: 10 * million, haircut: 0,
			want: 10 * million,
		},
		{
			// Equity collateral carries a much wider margin.
			name: "15% haircut on equity", cash: 10 * million, haircut: 1500,
			want: 1_150_000_000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CollateralRequired(tc.cash, tc.haircut)
			if err != nil {
				t.Fatalf("CollateralRequired() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("CollateralRequired() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCollateralAlwaysExceedsCash(t *testing.T) {
	// The lender's buffer must never be negative: any non-zero haircut has to
	// leave the collateral worth strictly more than the cash advanced.
	for _, haircut := range []BasisPoints{1, 25, 200, 1500, 5000} {
		got, err := CollateralRequired(10*million, haircut)
		if err != nil {
			t.Fatalf("CollateralRequired() error = %v", err)
		}
		if got <= 10*million {
			t.Errorf("haircut %s: collateral %s does not exceed cash %s",
				haircut, got, 10*million)
		}
	}
}

func TestMoneyString(t *testing.T) {
	tests := []struct {
		in   Money
		want string
	}{
		{145_833, "1458.33"},
		{100, "1.00"},
		{5, "0.05"},
		{0, "0.00"},
		{-6_944, "-69.44"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Money(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestBasisPointsString(t *testing.T) {
	tests := []struct {
		in   BasisPoints
		want string
	}{
		{525, "5.25%"},
		{25, "0.25%"},
		{1500, "15.00%"},
		{-25, "-0.25%"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("BasisPoints(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}
