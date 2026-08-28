package service

import (
	"strconv"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
)

// QuotaPolicy returns the exact native-to-USD conversion policy for the
// platform's current quota_per_unit snapshot.
func QuotaPolicy(settings lottery.StatusSettings) (quota.QuotaPerUnitPolicy, bool) {
	if settings.QuotaPerUnit <= 0 {
		return quota.QuotaPerUnitPolicy{}, false
	}
	policy, err := quota.NewQuotaPerUnitPolicy(strconv.FormatFloat(settings.QuotaPerUnit, 'f', -1, 64))
	if err != nil {
		return quota.QuotaPerUnitPolicy{}, false
	}
	return policy, true
}

// QuotaMoney converts a native quota amount into a traceable Money snapshot.
// Without a verified quota_per_unit the result is explicitly unavailable
// instead of a misleading $0.00.
func QuotaMoney(native float64, settings lottery.StatusSettings, source string, observedAt time.Time) quota.Money {
	policy, ok := QuotaPolicy(settings)
	if !ok {
		return quota.UnavailableUSD(source, observedAt)
	}
	amount, err := quota.ParseNativeFloat(native)
	if err != nil {
		return quota.UnavailableUSD(source, observedAt)
	}
	return policy.Convert(amount, quota.Provenance{Source: source, ObservedAt: observedAt})
}

// USDMoney wraps a value the platform already reported in USD as a confirmed
// snapshot under the already-usd-v1 rule.
func USDMoney(raw float64, source string, observedAt time.Time) quota.Money {
	amount, err := quota.ParseUSDFloat(raw)
	if err != nil {
		return quota.UnavailableUSD(source, observedAt)
	}
	return quota.NewAlreadyUSDPolicy().Convert(amount, quota.Provenance{Source: source, ObservedAt: observedAt})
}

func normalizeUsage(usage lottery.UserUsage, settings lottery.StatusSettings) lottery.UserUsage {
	now := time.Now().UTC()
	if policy, ok := QuotaPolicy(settings); ok {
		quotaAmount, quotaErr := quota.ParseNativeFloat(float64(usage.Quota))
		usedAmount, usedErr := quota.ParseNativeFloat(float64(usage.UsedQuota))
		if quotaErr == nil {
			money := policy.Convert(quotaAmount, quota.Provenance{Source: "user.quota", ObservedAt: now})
			usage.QuotaUSD = &money
		}
		if usedErr == nil {
			money := policy.Convert(usedAmount, quota.Provenance{Source: "user.used_quota", ObservedAt: now})
			usage.UsedQuotaUSD = &money
		}
		usage.QuotaConversionAvailable = quotaErr == nil
	} else {
		usage.QuotaConversionAvailable = false
	}
	if !usage.QuotaConversionAvailable {
		usage.QuotaConversionError = "无法获取美元换算配置"
	} else {
		usage.QuotaConversionError = ""
	}
	return usage
}

// quotaMoneyOrUnavailable converts an integral native amount, falling back to
// an explicit unavailable snapshot when no verified rule exists.
func quotaMoneyOrUnavailable(native int64, settings lottery.StatusSettings, source string, observedAt time.Time) quota.Money {
	money := QuotaMoney(float64(native), settings, source, observedAt)
	if money.State == quota.StateConfirmed {
		return money
	}
	return quota.UnavailableUSD(source, observedAt)
}

func quotaMoneyPointerOrUnavailable(native int64, settings lottery.StatusSettings, source string, observedAt time.Time) *quota.Money {
	money := quotaMoneyOrUnavailable(native, settings, source, observedAt)
	return &money
}
