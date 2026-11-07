module github.com/flixermedia/flixer-streamer

go 1.25.0

require github.com/getsentry/sentry-go v0.50.0

require (
	github.com/flixermedia/flixer-logger v0.0.0
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

replace github.com/flixermedia/flixer-logger => ./third_party/flixer-logger
