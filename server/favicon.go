package server

import (
	"encoding/base64"
	"net/http"
)

// The product icon. Without it every page load asks for /favicon.ico and logs
// a 404. /favicon.svg stays an SVG, wrapping the same image, for pages and
// bookmarks that already point at it.
var faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><image width="64" height="64" href="data:image/png;base64,` +
	base64.StdEncoding.EncodeToString(brandIcon) + `"/></svg>`

func serveFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(brandIcon)
}

func serveFaviconSVG(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(faviconSVG))
}
