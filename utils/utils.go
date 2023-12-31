package utils

import (
	"fmt"
	"strconv"
	"time"
)

// returns time passed as a srting
func TimePassedAgo(t time.Time) string {
	duration := time.Since(t)

	days := int(duration.Hours() / 24)
	hours := int(duration.Hours()) % 24
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	var result string

	if days > 0 {
		result = fmt.Sprintf("%d days ago", days)
	} else if hours > 0 {
		result = fmt.Sprintf("%d hours ago", hours)
	} else if minutes > 0 {
		result = fmt.Sprintf("%d minutes ago", minutes)
	} else {
		result = fmt.Sprintf("%d seconds ago", seconds)
	}

	return result
}

// returns true is the string is present in the array of strings
func StringIsInSlice(whatToFind string, whereToSearch []string) bool {
	for _, s := range whereToSearch {
		if s == whatToFind {
			return true
		}
	}
	return false
}

// formats 100000 as 100,000
func FormatWithThousandSeparators(n uint64) string {
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

func VisualiseSwapStatus(statusText string) string {
	switch statusText {
	case "State_ClaimedCoop":
		return "❌"
	case "State_SwapCanceled":
		return "❌"
	case "State_SendCancel":
		return "❌"
	case "State_ClaimedPreimage":
		return "💰"
	default:
		return "⌛"
	}
}
