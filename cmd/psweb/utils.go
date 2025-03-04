package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// returns time passed as a string
func timePassedAgo(t time.Time) string {
	duration := time.Since(t)

	days := int(duration.Hours() / 24)
	hours := int(duration.Hours()) % 24
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	var result string

	if days == 1 {
		result = fmt.Sprintf("%d day ago", days)
	} else if days > 1 {
		result = fmt.Sprintf("%d days ago", days)
	} else if hours == 1 {
		result = fmt.Sprintf("%d hour ago", hours)
	} else if hours > 1 {
		result = fmt.Sprintf("%d hours ago", hours)
	} else if minutes == 1 {
		result = fmt.Sprintf("%d minute ago", minutes)
	} else if minutes > 1 {
		result = fmt.Sprintf("%d minutes ago", minutes)
	} else {
		result = fmt.Sprintf("%d seconds ago", seconds)
	}

	return result
}

// returns true if the string is present in the array of strings
func stringIsInSlice(whatToFind string, whereToSearch []string) bool {
	for _, s := range whereToSearch {
		if s == whatToFind {
			return true
		}
	}
	return false
}

// formats 1234.567 as 1,234.567
func formatFloat(number float64) string {
	integerPart := uint64(math.Trunc(number))
	decimalPart := number - float64(integerPart)

	// Format the integer part with commas
	formattedIntegerPart := formatWithThousandSeparators(uint64(integerPart))

	if decimalPart == 0 {
		return formattedIntegerPart
	} else {
		return formattedIntegerPart + strings.TrimPrefix(fmt.Sprintf("%.3f", decimalPart), "0")
	}
}

// formats 100000 as 100,000
func formatWithThousandSeparators(n uint64) string {
	if n == 0 {
		return "-"
	}

	// Convert the integer to a string
	numStr := strconv.FormatUint(n, 10)

	// Determine the length of the number
	length := len(numStr)

	// Calculate the number of separators needed
	separatorCount := (length - 1) / 3

	// Create a new string with separators
	result := make([]byte, length+separatorCount)

	// Iterate through the string in reverse to add separators
	j := 0
	for i := length - 1; i >= 0; i-- {
		result[j] = numStr[i]
		j++
		if i > 0 && (length-i)%3 == 0 {
			result[j] = ','
			j++
		}
	}

	// Reverse the result to get the correct order
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return string(result)
}

var hourGlassRotate = 0

func visualiseSwapState(state string, rotate bool) string {
	switch state {
	case "State_ClaimedCoop",
		"State_ClaimedCsv",
		"State_SwapCanceled",
		"State_SendCancel":
		return "❌"
	case "State_ClaimedPreimage":
		return "💰"
	}

	if rotate {
		hourGlassRotate += 1

		if hourGlassRotate == 3 {
			hourGlassRotate = 0
		}

		switch hourGlassRotate {
		case 0:
			return "⏳"
		case 1:
			return "⌛"
		case 2:
			return "<span class=\"rotate-span\">⏳</span>" // rotate 90
		}
	}

	return "⌛"
}

func simplifySwapState(state string) string {
	switch state {
	case "State_ClaimedCoop",
		"State_ClaimedCsv",
		"State_SwapCanceled",
		"State_SendCancel":
		return "failed"
	case "State_ClaimedPreimage":
		return "success"
	}

	return "pending"
}

func toSats(amount float64) uint64 {
	return uint64(math.Round(amount * 100_000_000))
}

func toUint(num int64) uint64 {
	return uint64(num)
}

// displays balances in millions
func toMil(num uint64) string {
	if num == 0 {
		return "-"
	}
	if num >= 9_500_000 {
		return fmt.Sprintf("%.0fm", float64(num)/1_000_000)
	}
	if num >= 99_500 {
		return fmt.Sprintf("%.1fm", float64(num)/1_000_000)
	}
	if num >= 500 {
		return fmt.Sprintf("%.0fk", float64(num)/1_000)
	}
	return formatWithThousandSeparators(num)
}

func formatSigned(num int64) string {
	if num < 0 {
		return "-" + formatWithThousandSeparators(uint64(-num))
	}
	return formatWithThousandSeparators(uint64(num))
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	return false
}
