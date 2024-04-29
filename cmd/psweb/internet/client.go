package internet

import (
	"log"
	"net/http"
	"net/url"
	"time"

	"peerswap-web/cmd/psweb/config"

	"golang.org/x/net/proxy"
)

// configures an http client with an optional Tor proxy
func GetHttpClient(useProxy bool) *http.Client {
	var httpClient *http.Client

	if useProxy && config.Config.ProxyURL != "" {
		p, err := url.Parse(config.Config.ProxyURL)
		if err != nil {
			log.Println("Mempool getHttpClient:", err)
			return nil
		}
		dialer, err := proxy.SOCKS5("tcp", p.Host, nil, proxy.Direct)
		if err != nil {
			log.Println("Mempool getHttpClient:", err)
			return nil
		}
		httpClient = &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
			Timeout: 5 * time.Second,
		}
	} else {
		httpClient = &http.Client{
			Timeout: 5 * time.Second,
		}
	}
	return httpClient
}
