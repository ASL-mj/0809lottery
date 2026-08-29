package quota

import (
	"encoding/json"
	"testing"
	"time"
)

var provenance = Provenance{
	Source:     "subscription.amount_total",
	ObservedAt: time.Unix(0, 0).UTC(),
}

func TestQuotaPerUnitPolicyKeepsExactRemainder(t *testing.T) {
	amount, err := ParseNative("1000001")
	if err != nil {
		t.Fatalf("ParseNative() error = %v", err)
	}
	policy, err := NewQuotaPerUnitPolicy("500000")
	if err != nil {
		t.Fatalf("NewQuotaPerUnitPolicy() error = %v", err)
	}
	money := policy.Convert(amount, provenance)
	if money.Value != "2.000002" {
		t.Fatalf("Value = %s, want 2.000002", money.Value)
	}
	if money.State != StateConfirmed || money.Currency != "USD" {
		t.Fatalf("unexpected money = %#v", money)
	}
	if money.Formula != "quota-per-unit-v1: usd = native / 500000" {
		t.Fatalf("Formula = %q", money.Formula)
	}
	if money.Display != "$2.00" {
		t.Fatalf("Display = %q", money.Display)
	}
	if money.Source != provenance.Source || !money.ObservedAt.Equal(provenance.ObservedAt) {
		t.Fatalf("provenance lost: %#v", money)
	}
}

func TestQuotaPerUnitPolicyKeepsNonZeroSubCentDisplay(t *testing.T) {
	amount, err := ParseNative("5")
	if err != nil {
		t.Fatalf("ParseNative() error = %v", err)
	}
	policy, err := NewQuotaPerUnitPolicy("500000")
	if err != nil {
		t.Fatalf("NewQuotaPerUnitPolicy() error = %v", err)
	}
	money := policy.Convert(amount, provenance)
	if money.Value != "0.00001" || money.Display != "$0.00001" {
		t.Fatalf("sub-cent money = %#v, want value 0.00001 and display $0.00001", money)
	}
}

func TestQuotaPerUnitPolicyRejectsInvalidUnits(t *testing.T) {
	for _, unit := range []string{"", "0", "-5", "abc", "1.5e3"} {
		if _, err := NewQuotaPerUnitPolicy(unit); err == nil {
			t.Fatalf("NewQuotaPerUnitPolicy(%q) error = nil", unit)
		}
	}
}

func TestParseNativeRejectsBlankFractionalAndNegative(t *testing.T) {
	for _, raw := range []string{"", "   ", "12.5", "-1", "1e5"} {
		if _, err := ParseNative(raw); err == nil {
			t.Fatalf("ParseNative(%q) error = nil", raw)
		}
	}
	amount, err := ParseNative(" 42 ")
	if err != nil || amount.String() != "42" {
		t.Fatalf("ParseNative(42) = %v, %v", amount, err)
	}
}

func TestAlreadyUSDPolicyKeepsPlatformValueAsIs(t *testing.T) {
	amount, err := ParseUSD("12.5")
	if err != nil {
		t.Fatalf("ParseUSD() error = %v", err)
	}
	money := NewAlreadyUSDPolicy().Convert(amount, Provenance{Source: "dashboard.todaySpend", ObservedAt: provenance.ObservedAt})
	if money.Value != "12.5" || money.Display != "$12.50" || money.State != StateConfirmed {
		t.Fatalf("unexpected money = %#v", money)
	}
	if money.Formula != "already-usd-v1: usd = upstream usd value" {
		t.Fatalf("Formula = %q", money.Formula)
	}
}

func TestRemainingClampsNegativeToZero(t *testing.T) {
	policy, err := NewQuotaPerUnitPolicy("500000")
	if err != nil {
		t.Fatalf("NewQuotaPerUnitPolicy() error = %v", err)
	}
	total, err := ParseNative("100")
	if err != nil {
		t.Fatalf("ParseNative(total) error = %v", err)
	}
	used, err := ParseNative("250")
	if err != nil {
		t.Fatalf("ParseNative(used) error = %v", err)
	}
	remaining, clamped := policy.Remainder(total, used)
	if !clamped || remaining.String() != "0" {
		t.Fatalf("Remainder() = %s, %v", remaining, clamped)
	}
	money := policy.Convert(remaining, provenance)
	if money.Value != "0" || money.Display != "$0.00" {
		t.Fatalf("remaining money = %#v", money)
	}
}

func TestMoneyUnverifiedAndUnavailableStates(t *testing.T) {
	unverified := UnverifiedUSD("checkin.checkin_quota_awarded_usd", 2.4, provenance.ObservedAt)
	if unverified.State != StateUnverified || unverified.Value == "" || unverified.Display != "" {
		t.Fatalf("legacy money = %#v", unverified)
	}
	unavailable := UnavailableUSD("subscription.remaining", provenance.ObservedAt)
	if unavailable.State != StateUnavailable || unavailable.Value != "" || unavailable.Display != "" {
		t.Fatalf("unavailable money = %#v", unavailable)
	}
}

// Historical records persisted as bare floating-point numbers must surface as
// unverified, never reinterpreted under a new conversion rule.
func TestMoneyUnmarshalLegacyNumberBecomesUnverified(t *testing.T) {
	var money Money
	if err := json.Unmarshal([]byte(`2.4`), &money); err != nil {
		t.Fatalf("unmarshal legacy number: %v", err)
	}
	if money.State != StateUnverified || money.Value != "2.4" || money.Formula != "" {
		t.Fatalf("legacy money = %#v", money)
	}

	full := []byte(`{"currency":"USD","value":"2.000002","display":"$2.00","state":"confirmed","source":"subscription.amount_total","formula":"quota-per-unit-v1: usd = native / 500000","observed_at":"1970-01-01T00:00:00Z"}`)
	var decoded Money
	if err := json.Unmarshal(full, &decoded); err != nil {
		t.Fatalf("unmarshal money object: %v", err)
	}
	if decoded.Value != "2.000002" || decoded.State != StateConfirmed {
		t.Fatalf("decoded money = %#v", decoded)
	}
}
