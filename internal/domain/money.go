package domain

import (
	"errors"
	"fmt"
	"math/big"
)

// Money is an amount in minor units (cents for USD). Trading systems must never
// represent cash as float64 — 0.1 + 0.2 != 0.3 compounds into real settlement
// breaks. All arithmetic here is integer or exact rational.
type Money int64

// BasisPoints is one hundredth of a percent. Repo rates and haircuts are quoted
// in bps, so storing them as integers keeps quotes exact through comparison and
// round-tripping to the database.
type BasisPoints int64

const (
	// bpsPerUnit is the number of basis points in 1.0 (100%).
	bpsPerUnit = 10_000

	// moneyMarketBasis is the ACT/360 day-count convention used for USD repo.
	// Interest accrues on actual elapsed days over a 360-day year.
	moneyMarketBasis = 360
)

var (
	// ErrNegativeAmount is returned when an amount that must be positive is not.
	ErrNegativeAmount = errors.New("amount must be positive")

	// ErrInvalidTenor is returned when a repo's term is not at least one day.
	ErrInvalidTenor = errors.New("tenor must be at least one day")

	// ErrValidation wraps every field-level rejection from Validate. The API
	// layer matches on it to answer 400 rather than 500, so classification is by
	// identity rather than by sniffing error strings.
	ErrValidation = errors.New("validation failed")
)

func (m Money) String() string {
	neg := ""
	if m < 0 {
		neg, m = "-", -m
	}
	return fmt.Sprintf("%s%d.%02d", neg, int64(m)/100, int64(m)%100)
}

// String renders basis points as a percentage. The sign is applied separately
// because integer division truncates toward zero: -25bps would otherwise render
// as "0.25%", dropping the minus that makes it special collateral.
func (b BasisPoints) String() string {
	neg := ""
	if b < 0 {
		neg, b = "-", -b
	}
	return fmt.Sprintf("%s%d.%02d%%", neg, int64(b)/100, int64(b)%100)
}

// Interest returns the repo interest accrued on principal at rate over days,
// on an ACT/360 basis:
//
//	interest = principal * rate * days / 360
//
// The computation is done in exact rational arithmetic and rounded half-up at
// the final step, so a single rounding error is introduced rather than one per
// intermediate operation.
func Interest(principal Money, rate BasisPoints, days int) (Money, error) {
	if principal <= 0 {
		return 0, ErrNegativeAmount
	}
	if days < 1 {
		return 0, ErrInvalidTenor
	}

	// principal * rate * days / (10_000 * 360)
	num := new(big.Int).Mul(big.NewInt(int64(principal)), big.NewInt(int64(rate)))
	num.Mul(num, big.NewInt(int64(days)))
	den := big.NewInt(bpsPerUnit * moneyMarketBasis)

	return Money(roundHalfUp(num, den)), nil
}

// RepurchasePrice returns what the borrower repays at maturity: the cash
// borrowed plus accrued interest.
func RepurchasePrice(principal Money, rate BasisPoints, days int) (Money, error) {
	interest, err := Interest(principal, rate, days)
	if err != nil {
		return 0, err
	}
	return principal + interest, nil
}

// CollateralRequired returns the market value of securities that must be
// delivered against a cash loan, given a haircut.
//
// Under the standard margin convention the cash advanced is the collateral's
// market value discounted by the haircut, so the collateral must exceed the
// cash:
//
//	collateral = cash * (1 + haircut)
//
// A 2% haircut on $10,000,000 of cash therefore requires $10,200,000 of
// securities. The excess is the lender's buffer against a fall in collateral
// value before they can liquidate.
func CollateralRequired(cash Money, haircut BasisPoints) (Money, error) {
	if cash <= 0 {
		return 0, ErrNegativeAmount
	}
	if haircut < 0 {
		return 0, fmt.Errorf("haircut: %w", ErrNegativeAmount)
	}

	// cash * (10_000 + haircut) / 10_000
	num := new(big.Int).Mul(
		big.NewInt(int64(cash)),
		big.NewInt(bpsPerUnit+int64(haircut)),
	)
	return Money(roundHalfUp(num, big.NewInt(bpsPerUnit))), nil
}

// roundHalfUp divides num by den, rounding halves away from zero.
func roundHalfUp(num, den *big.Int) int64 {
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))

	// Compare 2*|rem| against den to decide whether to round away from zero.
	twice := new(big.Int).Abs(rem)
	twice.Lsh(twice, 1)
	if twice.Cmp(new(big.Int).Abs(den)) >= 0 {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	return quo.Int64()
}
