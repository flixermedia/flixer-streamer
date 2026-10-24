package httpserver

import "os"

// appVersion is the release baked into the Docker image (APP_VERSION build arg); "dev" outside of one.
func appVersion() string {
	if version := os.Getenv("APP_VERSION"); version != "" {
		return version
	}
	return "dev"
}
