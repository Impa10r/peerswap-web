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
		log.Println("Error creating request:", err)
		return ""
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := GetHttpClient(true)
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
		log.Println("Error decoding JSON:", err)
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
