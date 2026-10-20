//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"time"
)

// httpGet fetches a URL with a bounded timeout. The registries are local, so a
// hung request always means a broken deployment rather than a slow mirror.
func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, truncate(string(body)))
	}
	return body, nil
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func strconvItoa(v int) string { return strconv.Itoa(v) }

func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
