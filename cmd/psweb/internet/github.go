package internet

import (
	"encoding/json"
	"log"
	"net/http"
)

// fetch the latest tag of PeerSwap Web from github.com
func GetLatestTag() string {

	url := "http://api.github.com/repos/impa10r/peerswap-web/tags"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := GetHttpClient(true)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("GetLatestTag: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var tags []map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&tags)
	if err != nil {
		return ""
	}

	if len(tags) > 0 {
		latestTag := tags[0]["name"].(string)
		return latestTag
	} else {
		return ""
	}
}
