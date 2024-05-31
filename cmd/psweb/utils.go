package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/config"
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
	} else if hours > 0 {
		result = fmt.Sprintf("%d hours ago", hours)
	} else if minutes > 0 {
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
	return uint64(float64(100_000_000) * amount)
}

func toUint(num int64) uint64 {
	return uint64(num)
}

// displays balances in millions
func toMil(num uint64) string {
	if num == 0 {
		return "-"
	}
	if num >= 10_000_000 {
		return fmt.Sprintf("%d", num/1_000_000) + "m"
	}
	return fmt.Sprintf("%.1f", float32(num)/1_000_000) + "m"
}

func formatSigned(num int64) string {
	if num < 0 {
		return "-" + formatWithThousandSeparators(uint64(-num))
	}
	return formatWithThousandSeparators(uint64(num))
}

// msatToSatUp converts millisatoshis to satoshis, rounding up.
func msatToSatUp(msat uint64) uint64 {
	// Divide msat by 1000 and round up if there's any remainder.
	sat := msat / 1000
	if msat%1000 != 0 {
		sat++
	}
	return sat
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

func restart(w http.ResponseWriter, r *http.Request) {
	// assume systemd will restart it
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "PeerSwap Web UI is restarting...")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Please navigate manualy to http://"+strings.Split(r.Host, ":")[0]+":"+config.Config.ListenPort)

	// Flush the response writer to ensure the message is sent before shutdown
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Delay to ensure the message is displayed
	go func() {
		time.Sleep(1 * time.Second)
		log.Println("Restart requested, stopping PSWeb.")
		os.Exit(0)
	}()
}

// NoOpWriter is an io.Writer that does nothing.
type NoOpWriter struct{}

// Write discards the data and returns success.
func (NoOpWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

// NewMuteLogger creates a logger that discards all log output.
func NewMuteLogger() *log.Logger {
	return log.New(NoOpWriter{}, "", 0)
}

// html snippet to display and update fee PPM
func feeInputField(peerNodeId string, channelId uint64, direction string, feePerMil int64, backgroundColor string, fontColor string, showAll bool) string {
	// direction: inbound or outbound
	fieldId := strconv.FormatUint(channelId, 10) + "_" + direction
	align := "margin-left: 1px"
	if direction == "inbound" {
		align = "text-align: right"
	}

	nextPage := "/?"
	if showAll {
		nextPage += "showall&"
	}

	t := `<td title="` + strings.Title(direction) + ` fee PPM" id="scramble" style="width: 6ch; padding: 0px; ` + align + `">`
	t += `<form id="` + fieldId + `" autocomplete="off" action="/submit" method="post">`
	t += `<input autocomplete="false" name="hidden" type="text" style="display:none;">`
	t += `<input type="hidden" name="action" value="setFee">`
	t += `<input type="hidden" name="peerNodeId" value="` + peerNodeId + `">`
	t += `<input type="hidden" name="direction" value="` + direction + `">`
	t += `<input type="hidden" name="nextPage" value="` + nextPage + `">`
	t += `<input type="hidden" name="channelId" value="` + strconv.FormatUint(channelId, 10) + `">`
	t += `<input type="number" style="width: 6ch; text-align: center; background-color: ` + backgroundColor + `; color: ` + fontColor + `" name="feeRate" value="` + strconv.FormatInt(feePerMil, 10) + `" onchange="feeSubmitForm('` + fieldId + `')">`
	t += `</form>`
	t += `</td>`

	return t
}
