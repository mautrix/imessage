package config

import (
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    "time"

    "maunium.net/go/mautrix"
    "go.mau.fi/mautrix-imessage/ipc"
)

// sanitizeURL removes user credentials and query parameters from a URL to prevent sensitive data exposure.
// Returns an error if URL parsing fails.
func sanitizeURL(urlStr string) (string, error) {
    parsedURL, err := url.Parse(urlStr)
    if err != nil {
        return "", fmt.Errorf("failed to parse URL: %w", err)
    }
    parsedURL.User = nil
    parsedURL.RawQuery = ""
    return parsedURL.String(), nil
}

type configRedirect struct {
    URL        string `json:"url"`
    Redirected bool   `json:"redirected"`
}

// outputConfigURL encodes the configRedirect structure into JSON and prints it.
func outputConfigURL(url string, redirected bool) error {
    configData := &configRedirect{
        URL:        url,
        Redirected: redirected,
    }
    encodedData, err := json.Marshal(configData)
    if err != nil {
        return fmt.Errorf("failed to encode config data: %w", err)
    }
    fmt.Printf("%s\n", encodedData)
    return nil
}

// shared HTTP client for reuse in multiple functions
var httpClient = &http.Client{
    Timeout: 30 * time.Second,
}

// checkRedirect follows the redirect of a given URL and returns the final URL.
func checkRedirect(initialURL string) (string, error) {
    // ... [Rest of the function with improved error handling and comments]
    // ...
}

// Download retrieves the configuration file from a given URL and saves it to a specified path.
func Download(configURL, saveTo string, outputRedirect bool) error {
    // ... [Rest of the function with improved error handling and comments]
    // ...
}
