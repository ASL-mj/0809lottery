package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

type State string

const (
	// StateConfirmed means the amount was computed by a versioned, verified
	// conversion rule; Display is safe to render.
	StateConfirmed State = "confirmed"
	// StateUnverified marks legacy floating-point records that predate the
	// exact rules. The value is shown as raw history, never as a confirmed
	// dollar amount.
	StateUnverified State = "unverified"
	// StateUnavailable means no verified conversion rule exists for the
	// field, so no dollar value may be displayed at all.
	StateUnavailable State = "unavailable"
)

// Money is the traceable amount DTO. Exact values live as decimal strings;
// floats never cross the boundary.
type Money struct {
	Currency   string    `json:"currency"`
	Value      string    `json:"value,omitempty"`
	Display    string    `json:"display,omitempty"`
	State      State     `json:"state"`
	Source     string    `json:"source"`
	Formula    string    `json:"formula,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// UnmarshalJSON accepts both the current object form and bare numbers from
// historical records. A bare number becomes an unverified USD value so old
// snapshots remain readable without being silently reinterpreted.
func (m *Money) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		type alias Money
		var decoded alias
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*m = Money(decoded)
		return nil
	}
	var legacy float64
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("decode money: %w", err)
	}
	*m = UnverifiedUSD("", legacy, time.Time{})
	return nil
}

// Provenance records where an amount came from and when it was observed.
type Provenance struct {
	Source     string
	ObservedAt time.Time
}

// NativeAmount is an exact, integral native quota value.
type NativeAmount struct {
	rat *big.Rat
}

func (a NativeAmount) String() string {
	if a.rat == nil {
		return "0"
	}
	return a.rat.RatString()
}

// nativeRat returns the exact rational behind the amount, never nil.
func nativeRat(a NativeAmount) *big.Rat {
	if a.rat == nil {
		return new(big.Rat)
	}
	return a.rat
}

// usdRat returns the exact rational behind the amount, never nil.
func usdRat(a USDAmount) *big.Rat {
	if a.rat == nil {
		return new(big.Rat)
	}
	return a.rat
}

// USDAmount is an exact decimal USD value taken from the platform.
type USDAmount struct {
	rat *big.Rat
}

// ParseNative parses an integral native quota value. Blank, fractional,
// negative and non-decimal inputs are rejected; scientific notation is not
// accepted so the exact string form is always preserved.
func ParseNative(raw string) (NativeAmount, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return NativeAmount{}, errors.New("native amount is blank")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return NativeAmount{}, fmt.Errorf("native amount %q must be a non-negative integer", raw)
		}
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return NativeAmount{}, fmt.Errorf("native amount %q must be an integer", raw)
	}
	return NativeAmount{rat: value}, nil
}

// ParseNativeFloat converts an integral float (as decoded from upstream JSON)
// into an exact native amount; fractional input is rejected.
func ParseNativeFloat(value float64) (NativeAmount, error) {
	if value != value || value > 1e15 || value < -1e15 {
		return NativeAmount{}, fmt.Errorf("native amount %v is out of range", value)
	}
	rat := new(big.Rat).SetFloat64(value)
	if !rat.IsInt() {
		return NativeAmount{}, fmt.Errorf("native amount %v must be an integer", value)
	}
	return NativeAmount{rat: rat}, nil
}

// ParseUSD parses an exact decimal USD value reported by the platform.
func ParseUSD(raw string) (USDAmount, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return USDAmount{}, errors.New("usd amount is blank")
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return USDAmount{}, fmt.Errorf("usd amount %q is not a decimal number", raw)
	}
	if value.Sign() < 0 {
		return USDAmount{}, fmt.Errorf("usd amount %q must not be negative", raw)
	}
	return USDAmount{rat: value}, nil
}

// ParseUSDFloat converts a float64 USD value (as decoded from upstream JSON)
// into an exact decimal via its shortest round-trip representation.
func ParseUSDFloat(value float64) (USDAmount, error) {
	if value != value || value > 1e15 || value < -1e15 {
		return USDAmount{}, fmt.Errorf("usd amount %v is out of range", value)
	}
	rat := new(big.Rat).SetFloat64(value)
	return USDAmount{rat: rat}, nil
}

// displayUSD renders a confirmed amount as a dollar string using half-up
// rounding for cent-sized values and exact significant decimals for non-zero
// sub-cent values.
func displayUSD(value *big.Rat) string {
	if value == nil || value.Sign() == 0 {
		return "$0.00"
	}
	// Keep sub-cent rewards visible. The platform can return small, non-zero
	// quota awards that would otherwise round to the misleading "$0.00".
	absolute := new(big.Rat).Set(value)
	if absolute.Sign() < 0 {
		absolute.Neg(absolute)
	}
	if absolute.Cmp(big.NewRat(1, 200)) < 0 {
		prefix := "$"
		if value.Sign() < 0 {
			prefix = "-$"
		}
		return prefix + exactDecimal(absolute)
	}
	cents := new(big.Rat).Mul(value, big.NewRat(100, 1))
	negative := cents.Sign() < 0
	if negative {
		cents.Neg(cents)
	}
	sum := new(big.Rat).Add(cents, big.NewRat(1, 2))
	quo := new(big.Int).Quo(sum.Num(), sum.Denom())
	dollars, rem := new(big.Int).QuoRem(quo, big.NewInt(100), new(big.Int))
	sign := ""
	if negative {
		sign = "-"
	}
	return fmt.Sprintf("%s$%s.%02d", sign, dollars.String(), rem.Int64())
}

// exactString renders the exact decimal value, trimming trailing zeros.
func exactString(value *big.Rat) string {
	text := value.FloatString(12)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" || text == "-" {
		return "0"
	}
	return text
}

// UnverifiedUSD converts a legacy floating-point record into an unverified
// money value. It keeps the raw value for display-free auditing.
func UnverifiedUSD(source string, value float64, observedAt time.Time) Money {
	rat := new(big.Rat).SetFloat64(value)
	return Money{
		Currency:   "USD",
		Value:      exactString(rat),
		State:      StateUnverified,
		Source:     source,
		ObservedAt: observedAt,
	}
}

// UnavailableUSD marks a field whose conversion rule is not confirmed. No
// value or display is emitted so callers can never mistake it for $0.00.
func UnavailableUSD(source string, observedAt time.Time) Money {
	return Money{
		Currency:   "USD",
		State:      StateUnavailable,
		Source:     source,
		ObservedAt: observedAt,
	}
}
