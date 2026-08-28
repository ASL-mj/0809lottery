package service

import (
	"fmt"

	"skyeapi/lottery-bot/internal/lottery"
)

// QuotaUSD converts the site's native quota units to US dollars.
func QuotaUSD(value int64, settings lottery.StatusSettings) (*float64, bool) {
	return QuotaAmountUSD(float64(value), settings)
}

// QuotaAmountUSD converts a possibly fractional native quota amount to USD.
func QuotaAmountUSD(value float64, settings lottery.StatusSettings) (*float64, bool) {
	if settings.QuotaPerUnit <= 0 {
		return nil, false
	}
	amount := value / settings.QuotaPerUnit
	return &amount, true
}

func normalizeUsage(usage lottery.UserUsage, settings lottery.StatusSettings) lottery.UserUsage {
	usage.QuotaUSD, usage.QuotaConversionAvailable = QuotaUSD(usage.Quota, settings)
	usage.UsedQuotaUSD, _ = QuotaUSD(usage.UsedQuota, settings)
	if !usage.QuotaConversionAvailable {
		usage.QuotaConversionError = "无法获取美元换算配置"
	} else {
		usage.QuotaConversionError = ""
	}
	return usage
}

func formatUSD(value int64, settings lottery.StatusSettings) string {
	amount, ok := QuotaUSD(value, settings)
	if !ok {
		return "暂时无法换算"
	}
	return fmt.Sprintf("$%.2f", *amount)
}
