package remotewrite

import (
	"encoding/base64"
	"regexp"
)

// basicAuthHeader returns the value for an HTTP Basic Authorization header.
func basicAuthHeader(user, pass string) string {
	creds := user + ":" + pass
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// urlCredsRE matches userinfo embedded in a URL: scheme://user:pass@host/...
var urlCredsRE = regexp.MustCompile(`(://)([^:/@\s]+):([^@\s]+)@`)

// redactErr returns a string form of err with any embedded URL userinfo
// scrubbed. net/http's error messages occasionally include the full
// request URL, which can leak credentials supplied via a userinfo URL.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	s = urlCredsRE.ReplaceAllString(s, "${1}REDACTED:REDACTED@")
	return s
}
