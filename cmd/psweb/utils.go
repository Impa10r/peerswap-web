package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// returns time passed as a srting
func timePassedAgo(t time.Time) string {
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
func stringIsInSlice(whatToFind string, whereToSearch []string) bool {
	for _, s := range whereToSearch {
		if s == whatToFind {
			return true
		}
	}
	return false
}

// formats 100000 as 100,000
func formatWithThousandSeparators(n uint64) string {
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

func visualiseSwapStatus(statusText string, rotate bool) string {
	switch statusText {
	case "State_ClaimedCoop":
		return "<a href=\"/\">❌</a>"
	case "State_ClaimedCsv":
		return "<a href=\"/\">❌</a>"
	case "State_SwapCanceled":
		return "<a href=\"/\">❌</a>"
	case "State_SendCancel":
		return "<a href=\"/\">❌</a>"
	case "State_ClaimedPreimage":
		return "<a href=\"/\">💰</a>"
	}

	if rotate {
		hourGlassRotate += 1

		if hourGlassRotate == 3 {
			hourGlassRotate = 0
		}

		switch hourGlassRotate {
		case 0:
			return "<a href=\"/\">⏳</a>"
		case 1:
			return "<a href=\"/\">⌛</a>"
		case 2:
			return "<span class=\"rotate-span\"><a href=\"/\">⏳</a></span>" // rotate 90
		}
	}

	return "⌛"
}

func getLatestTag() string {

	url := "http://api.github.com/repos/impa10r/peerswap-web/tags"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Println("Error creating request:", err)
		return ""
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("Error making request:", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Println("Failed to fetch tags. Status code:", resp.StatusCode)
		return ""
	}

	var tags []map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&tags)
	if err != nil {
		fmt.Println("Error decoding JSON:", err)
		return ""
	}

	if len(tags) > 0 {
		latestTag := tags[0]["name"].(string)
		return latestTag
	} else {
		log.Println("No tags found in the repository.")
		return ""
	}
}

func setLogging() error {
	// Set log file name
	logFileName := filepath.Join(config.DataDir, "psweb.log")
	var err error
	// Open log file in append mode, create if it doesn't exist
	logFile, err = os.OpenFile(logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	// Set log output to both file and standard output
	multi := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multi)

	log.SetFlags(log.Ldate | log.Ltime)
	if os.Getenv("DEBUG") == "1" {
		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	}

	return nil
}

func closeLogFile() {
	if logFile != nil {
		if err := logFile.Close(); err != nil {
			log.Println("Error closing log file:", err)
		}
	}
}

func toSats(amount float64) uint64 {
	return uint64(float64(100000000) * amount)
}

func toBitcoin(amountSats uint64) float64 {
	return float64(amountSats) / float64(100000000)
}

func toUint(num int64) uint64 {
	return uint64(num)
}

func toMil(num uint64) string {
	return fmt.Sprintf("%.1f", float32(num/1000000))
}

func getNodeAlias(key string) string {
	for _, n := range aliasCache {
		if n.PublicKey == key {
			return n.Alias
		}
	}

	// try lnd
	alias := lndGetAlias(key)

	if alias == "" {
		// try mempool
		alias = mempoolGetNodeAlias(key)
	}

	if alias == "" {
		// return first 20 chars of key
		return key[:20]
	}

	// save to cache
	aliasCache = append(aliasCache, AliasCache{
		PublicKey: key,
		Alias:     alias,
	})

	return alias
}
