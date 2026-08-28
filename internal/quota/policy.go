package quota

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	quotaPerUnitFormula = "quota-per-unit-v1: usd = native / %s"
	alreadyUSDFormula   = "already-usd-v1: usd = upstream usd value"
	// exactDecimals bounds the rendered precision of a converted value. The
	// rational math itself stays exact; only the string stops here.
	exactDecimals = 12
)

// QuotaPerUnitPolicy converts native quota units to USD with the platform's
// verified quota_per_unit constant, using exact rational arithmetic.
type QuotaPerUnitPolicy struct {
	unit    *big.Rat
	unitRaw string
}

// parsePlainDecimal accepts plain decimal numbers only: digits with at most
// one decimal point. Scientific notation is rejected so formula snapshots
// keep their exact literal form.
func parsePlainDecimal(raw string) (*big.Rat, bool) {
	if raw == "" {
		return nil, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return nil, false
	}
	for _, part := range parts {
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, false
			}
		}
	}
	return new(big.Rat).SetString(raw)
}

func NewQuotaPerUnitPolicy(quotaPerUnit string) (QuotaPerUnitPolicy, error) {
	raw := strings.TrimSpace(quotaPerUnit)
	if raw == "" {
		return QuotaPerUnitPolicy{}, fmt.Errorf("quota_per_unit is blank")
	}
	unit, ok := parsePlainDecimal(raw)
	if !ok || unit.Sign() <= 0 {
		return QuotaPerUnitPolicy{}, fmt.Errorf("quota_per_unit %q must be a positive number", raw)
	}
	return QuotaPerUnitPolicy{unit: unit, unitRaw: raw}, nil
}

func (p QuotaPerUnitPolicy) Formula() string {
	return fmt.Sprintf(quotaPerUnitFormula, p.unitRaw)
}

// Convert renders one native amount as a confirmed USD money value.
func (p QuotaPerUnitPolicy) Convert(amount NativeAmount, provenance Provenance) Money {
	usd := new(big.Rat).Quo(nativeRat(amount), p.unit)
	return Money{
		Currency:   "USD",
		Value:      exactDecimal(usd),
		Display:    displayUSD(usd),
		State:      StateConfirmed,
		Source:     provenance.Source,
		Formula:    p.Formula(),
		ObservedAt: observedAt(provenance),
	}
}

// Remainder computes max(total - used, 0) exactly and reports whether the
// clamp kicked in.
func (p QuotaPerUnitPolicy) Remainder(total, used NativeAmount) (NativeAmount, bool) {
	diff := new(big.Rat).Sub(nativeRat(total), nativeRat(used))
	if diff.Sign() < 0 {
		return NativeAmount{rat: new(big.Rat)}, true
	}
	return NativeAmount{rat: diff}, false
}

// AlreadyUSDPolicy passes through values the platform already reported in USD.
type AlreadyUSDPolicy struct{}

func NewAlreadyUSDPolicy() AlreadyUSDPolicy {
	return AlreadyUSDPolicy{}
}

func (AlreadyUSDPolicy) Formula() string {
	return alreadyUSDFormula
}

func (p AlreadyUSDPolicy) Convert(amount USDAmount, provenance Provenance) Money {
	return Money{
		Currency:   "USD",
		Value:      exactDecimal(usdRat(amount)),
		Display:    displayUSD(usdRat(amount)),
		State:      StateConfirmed,
		Source:     provenance.Source,
		Formula:    p.Formula(),
		ObservedAt: observedAt(provenance),
	}
}

func exactDecimal(value *big.Rat) string {
	if value == nil {
		return "0"
	}
	text := value.FloatString(exactDecimals)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" || text == "-" {
		return "0"
	}
	return text
}

func observedAt(provenance Provenance) time.Time {
	if provenance.ObservedAt.IsZero() {
		return time.Now().UTC()
	}
	return provenance.ObservedAt.UTC()
}
